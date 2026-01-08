// Command gateway runs the blockchain event listener and job gateway.
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

	"github.com/luxfi/fhe-coprocessor/internal/gateway"
	"github.com/luxfi/fhe-coprocessor/internal/queue"
	"github.com/luxfi/fhe-coprocessor/internal/storage"
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
		startBlock     = flag.Uint64("start-block", 0, "block to start scanning from (0 = latest)")
		confirmations  = flag.Uint64("confirmations", 2, "required block confirmations")
		redisAddr      = flag.String("redis", "localhost:6379", "Redis address")
		redisDB        = flag.Int("redis-db", 0, "Redis database number")
		queueName      = flag.String("queue", "default", "queue name")
		storagePath    = flag.String("storage", "/tmp/fhe-storage", "ciphertext storage path")
		httpAddr       = flag.String("http", ":8080", "HTTP API address")
		logLevel       = flag.String("log-level", "info", "log level (debug, info, warn, error)")
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

	// Storage.
	store, err := storage.NewFileStorage(*storagePath)
	if err != nil {
		return fmt.Errorf("create storage: %w", err)
	}

	// Blockchain client (mock for now).
	client := gateway.NewMockBlockchainClient()

	// Event listener.
	listener := gateway.NewListener(gateway.Config{
		StartBlock:         *startBlock,
		ConfirmationBlocks: *confirmations,
		PollInterval:       2 * time.Second,
		BatchSize:          100,
	}, client, q, store, logger)

	// Context with cancellation.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start listener.
	if err := listener.Start(ctx); err != nil {
		return fmt.Errorf("start listener: %w", err)
	}

	// HTTP API.
	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		lastBlock := listener.LastBlock()
		fmt.Fprintf(w, `{"last_block": %d}`, lastBlock)
	})

	mux.HandleFunc("/job/", func(w http.ResponseWriter, r *http.Request) {
		jobID := r.URL.Path[len("/job/"):]
		if jobID == "" {
			http.Error(w, "job ID required", http.StatusBadRequest)
			return
		}

		job, err := q.Get(r.Context(), jobID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"%s","status":%d,"result":"%s","error":"%s"}`,
			job.ID, job.Status, job.ResultHandle, job.Error)
	})

	server := &http.Server{
		Addr:         *httpAddr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	go func() {
		logger.Info("HTTP server starting", zap.String("addr", *httpAddr))
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

	if err := listener.Stop(); err != nil {
		logger.Error("listener shutdown error", zap.Error(err))
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
