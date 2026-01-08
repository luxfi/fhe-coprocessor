// Command tfhe-worker runs FHE computation workers.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/luxfi/fhe-coprocessor/internal/queue"
	"github.com/luxfi/fhe-coprocessor/internal/storage"
	"github.com/luxfi/fhe-coprocessor/internal/worker"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// Flags.
	var (
		numWorkers  = flag.Int("workers", 4, "number of worker goroutines")
		redisAddr   = flag.String("redis", "localhost:6379", "Redis address")
		redisDB     = flag.Int("redis-db", 0, "Redis database number")
		queueName   = flag.String("queue", "default", "queue name")
		storagePath = flag.String("storage", "/tmp/fhe-storage", "ciphertext storage path")
		serverKey   = flag.String("server-key", "", "path to TFHE server key file")
		metricsAddr = flag.String("metrics", ":9090", "metrics server address")
		logLevel    = flag.String("log-level", "info", "log level (debug, info, warn, error)")
	)
	flag.Parse()

	// Logger.
	logger, err := newLogger(*logLevel)
	if err != nil {
		return fmt.Errorf("create logger: %w", err)
	}
	defer logger.Sync()

	// Load server key.
	var serverKeyBytes []byte
	if *serverKey != "" {
		serverKeyBytes, err = os.ReadFile(*serverKey)
		if err != nil {
			return fmt.Errorf("read server key: %w", err)
		}
	} else {
		logger.Warn("no server key provided, using mock key")
		serverKeyBytes = []byte("mock-server-key")
	}

	// Queue.
	q, err := queue.NewRedisQueue(queue.RedisConfig{
		Addr: *redisAddr,
		DB:   *redisDB,
	}, *queueName)
	if err != nil {
		return fmt.Errorf("create queue: %w", err)
	}
	defer q.Close()

	// Storage.
	store, err := storage.NewFileStorage(*storagePath)
	if err != nil {
		return fmt.Errorf("create storage: %w", err)
	}

	// Worker pool.
	pool, err := worker.NewPool(worker.Config{
		NumWorkers:      *numWorkers,
		ServerKey:       serverKeyBytes,
		ShutdownTimeout: 30 * time.Second,
	}, q, store, logger)
	if err != nil {
		return fmt.Errorf("create worker pool: %w", err)
	}

	// Context with cancellation.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start workers.
	if err := pool.Start(ctx); err != nil {
		return fmt.Errorf("start workers: %w", err)
	}

	// Metrics server.
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if err := pool.HealthCheck(); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		stats := pool.Stats()
		fmt.Fprintf(w, "# HELP fhe_operations_total Total FHE operations\n")
		fmt.Fprintf(w, "# TYPE fhe_operations_total counter\n")
		fmt.Fprintf(w, "fhe_operations_total{status=\"success\"} %d\n", stats.SuccessCount)
		fmt.Fprintf(w, "fhe_operations_total{status=\"failure\"} %d\n", stats.FailureCount)
		fmt.Fprintf(w, "# HELP fhe_operation_duration_seconds Average operation duration\n")
		fmt.Fprintf(w, "# TYPE fhe_operation_duration_seconds gauge\n")
		fmt.Fprintf(w, "fhe_operation_duration_seconds %f\n", stats.AverageDuration().Seconds())
	})

	server := &http.Server{
		Addr:    *metricsAddr,
		Handler: mux,
	}

	go func() {
		logger.Info("metrics server starting", zap.String("addr", *metricsAddr))
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("metrics server error", zap.Error(err))
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
		logger.Error("metrics server shutdown error", zap.Error(err))
	}

	if err := pool.Stop(); err != nil {
		logger.Error("worker pool shutdown error", zap.Error(err))
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
