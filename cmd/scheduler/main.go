// Command scheduler manages job scheduling and worker coordination.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/luxfi/fhe-coprocessor/internal/queue"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// WorkerInfo represents a registered worker.
type WorkerInfo struct {
	ID         string    `json:"id"`
	Address    string    `json:"address"`
	Capacity   int       `json:"capacity"`
	ActiveJobs int       `json:"active_jobs"`
	LastSeen   time.Time `json:"last_seen"`
}

// Scheduler coordinates job distribution across workers.
type Scheduler struct {
	queue   queue.Queue
	workers map[string]*WorkerInfo
	mu      sync.RWMutex
	logger  *zap.Logger
}

// NewScheduler creates a new scheduler.
func NewScheduler(q queue.Queue, logger *zap.Logger) *Scheduler {
	return &Scheduler{
		queue:   q,
		workers: make(map[string]*WorkerInfo),
		logger:  logger,
	}
}

// RegisterWorker registers a new worker.
func (s *Scheduler) RegisterWorker(info WorkerInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()

	info.LastSeen = time.Now()
	s.workers[info.ID] = &info
	s.logger.Info("worker registered",
		zap.String("id", info.ID),
		zap.String("address", info.Address),
		zap.Int("capacity", info.Capacity),
	)
}

// DeregisterWorker removes a worker.
func (s *Scheduler) DeregisterWorker(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.workers[id]; ok {
		delete(s.workers, id)
		s.logger.Info("worker deregistered", zap.String("id", id))
	}
}

// Heartbeat updates worker last seen time.
func (s *Scheduler) Heartbeat(id string, activeJobs int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	worker, ok := s.workers[id]
	if !ok {
		return false
	}

	worker.LastSeen = time.Now()
	worker.ActiveJobs = activeJobs
	return true
}

// GetWorkers returns all registered workers.
func (s *Scheduler) GetWorkers() []WorkerInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()

	workers := make([]WorkerInfo, 0, len(s.workers))
	for _, w := range s.workers {
		workers = append(workers, *w)
	}
	return workers
}

// CleanupStale removes workers that haven't been seen recently.
func (s *Scheduler) CleanupStale(maxAge time.Duration) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	removed := 0
	now := time.Now()
	for id, w := range s.workers {
		if now.Sub(w.LastSeen) > maxAge {
			delete(s.workers, id)
			removed++
			s.logger.Info("removed stale worker", zap.String("id", id))
		}
	}
	return removed
}

// TotalCapacity returns the total available capacity.
func (s *Scheduler) TotalCapacity() int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	total := 0
	for _, w := range s.workers {
		total += w.Capacity - w.ActiveJobs
	}
	return total
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// Flags.
	var (
		redisAddr     = flag.String("redis", "localhost:6379", "Redis address")
		redisDB       = flag.Int("redis-db", 0, "Redis database number")
		queueName     = flag.String("queue", "default", "queue name")
		httpAddr      = flag.String("http", ":8081", "HTTP API address")
		cleanupPeriod = flag.Duration("cleanup-period", 30*time.Second, "worker cleanup period")
		workerTimeout = flag.Duration("worker-timeout", 60*time.Second, "worker heartbeat timeout")
		logLevel      = flag.String("log-level", "info", "log level (debug, info, warn, error)")
	)
	flag.Parse()

	// Logger.
	logger, err := newLogger(*logLevel)
	if err != nil {
		return fmt.Errorf("create logger: %w", err)
	}
	defer logger.Sync()

	// Queue.
	q, err := queue.NewRedisQueue(queue.RedisConfig{
		Addr: *redisAddr,
		DB:   *redisDB,
	}, *queueName)
	if err != nil {
		return fmt.Errorf("create queue: %w", err)
	}
	defer q.Close()

	// Scheduler.
	scheduler := NewScheduler(q, logger)

	// Context with cancellation.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Worker cleanup goroutine.
	go func() {
		ticker := time.NewTicker(*cleanupPeriod)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				removed := scheduler.CleanupStale(*workerTimeout)
				if removed > 0 {
					logger.Info("cleaned up stale workers", zap.Int("count", removed))
				}
			}
		}
	}()

	// HTTP API.
	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	mux.HandleFunc("/workers", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			workers := scheduler.GetWorkers()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(workers)

		case http.MethodPost:
			var info WorkerInfo
			if err := json.NewDecoder(r.Body).Decode(&info); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			scheduler.RegisterWorker(info)
			w.WriteHeader(http.StatusCreated)

		case http.MethodDelete:
			id := r.URL.Query().Get("id")
			if id == "" {
				http.Error(w, "id required", http.StatusBadRequest)
				return
			}
			scheduler.DeregisterWorker(id)
			w.WriteHeader(http.StatusNoContent)

		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			ID         string `json:"id"`
			ActiveJobs int    `json:"active_jobs"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		if !scheduler.Heartbeat(req.ID, req.ActiveJobs) {
			http.Error(w, "worker not found", http.StatusNotFound)
			return
		}

		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		workers := scheduler.GetWorkers()
		capacity := scheduler.TotalCapacity()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"workers":          len(workers),
			"total_capacity":   capacity,
		})
	})

	mux.HandleFunc("/submit", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var job queue.Job
		if err := json.NewDecoder(r.Body).Decode(&job); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		if err := q.Push(r.Context(), &job); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]string{"id": job.ID})
	})

	server := &http.Server{
		Addr:         *httpAddr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	go func() {
		logger.Info("scheduler starting", zap.String("addr", *httpAddr))
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("HTTP server error", zap.Error(err))
		}
	}()

	// Wait for shutdown signal.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	sig := <-sigCh
	logger.Info("received signal", zap.String("signal", sig.String()))

	// Graceful shutdown.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("HTTP server shutdown error", zap.Error(err))
	}

	logger.Info("shutdown complete")
	return nil
}

func newLogger(level string) (*zap.Logger, error) {
	lvl, err := zapcore.ParseLevel(level)
	if err != nil {
		lvl = zapcore.InfoLevel
	}

	cfg := zap.Config{
		Level:       zap.NewAtomicLevelAt(lvl),
		Development: false,
		Encoding:    "json",
		EncoderConfig: zapcore.EncoderConfig{
			TimeKey:        "ts",
			LevelKey:       "level",
			NameKey:        "logger",
			CallerKey:      "caller",
			MessageKey:     "msg",
			StacktraceKey:  "stacktrace",
			LineEnding:     zapcore.DefaultLineEnding,
			EncodeLevel:    zapcore.LowercaseLevelEncoder,
			EncodeTime:     zapcore.ISO8601TimeEncoder,
			EncodeDuration: zapcore.SecondsDurationEncoder,
			EncodeCaller:   zapcore.ShortCallerEncoder,
		},
		OutputPaths:      []string{"stdout"},
		ErrorOutputPaths: []string{"stderr"},
	}

	return cfg.Build()
}
