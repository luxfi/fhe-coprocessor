package worker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/luxfi/fhe-coprocessor/internal/queue"
	"github.com/luxfi/fhe-coprocessor/internal/storage"
	"github.com/luxfi/fhe-coprocessor/pkg/fhe"
	"go.uber.org/zap"
)

// Config holds worker pool configuration.
type Config struct {
	// NumWorkers is the number of concurrent workers.
	NumWorkers int
	// ServerKey is the TFHE server key bytes.
	ServerKey []byte
	// ShutdownTimeout is the graceful shutdown timeout.
	ShutdownTimeout time.Duration
}

// DefaultConfig returns sensible defaults.
func DefaultConfig() Config {
	return Config{
		NumWorkers:      4,
		ShutdownTimeout: 30 * time.Second,
	}
}

// Pool manages a pool of FHE computation workers.
type Pool struct {
	cfg      Config
	queue    queue.Queue
	storage  storage.Storage
	executor *OperationExecutor
	logger   *zap.Logger

	wg       sync.WaitGroup
	cancel   context.CancelFunc
	running  atomic.Bool
	stats    *OperationStats
	statsMu  sync.RWMutex
}

// NewPool creates a new worker pool.
func NewPool(cfg Config, q queue.Queue, store storage.Storage, logger *zap.Logger) (*Pool, error) {
	if cfg.NumWorkers <= 0 {
		cfg.NumWorkers = DefaultConfig().NumWorkers
	}
	if cfg.ShutdownTimeout == 0 {
		cfg.ShutdownTimeout = DefaultConfig().ShutdownTimeout
	}

	executor, err := NewOperationExecutor(cfg.ServerKey, store, logger)
	if err != nil {
		return nil, fmt.Errorf("create executor: %w", err)
	}

	return &Pool{
		cfg:      cfg,
		queue:    q,
		storage:  store,
		executor: executor,
		logger:   logger,
		stats:    NewOperationStats(),
	}, nil
}

// Start starts the worker pool.
func (p *Pool) Start(ctx context.Context) error {
	if p.running.Load() {
		return errors.New("pool already running")
	}

	ctx, p.cancel = context.WithCancel(ctx)
	p.running.Store(true)

	p.logger.Info("starting worker pool", zap.Int("workers", p.cfg.NumWorkers))

	for i := 0; i < p.cfg.NumWorkers; i++ {
		p.wg.Add(1)
		go p.worker(ctx, i)
	}

	return nil
}

// Stop gracefully stops the worker pool.
func (p *Pool) Stop() error {
	if !p.running.Load() {
		return nil
	}

	p.logger.Info("stopping worker pool")
	p.cancel()

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		p.logger.Info("worker pool stopped")
	case <-time.After(p.cfg.ShutdownTimeout):
		p.logger.Warn("shutdown timeout exceeded")
		return errors.New("shutdown timeout")
	}

	p.running.Store(false)
	return nil
}

// Stats returns current pool statistics.
func (p *Pool) Stats() OperationStats {
	p.statsMu.RLock()
	defer p.statsMu.RUnlock()

	// Return a copy.
	stats := *p.stats
	stats.OperationCounts = make(map[fhe.OperationType]int64)
	stats.OperationTimings = make(map[fhe.OperationType]time.Duration)
	for k, v := range p.stats.OperationCounts {
		stats.OperationCounts[k] = v
	}
	for k, v := range p.stats.OperationTimings {
		stats.OperationTimings[k] = v
	}
	return stats
}

func (p *Pool) worker(ctx context.Context, id int) {
	defer p.wg.Done()

	logger := p.logger.With(zap.Int("worker_id", id))
	logger.Debug("worker started")

	for {
		select {
		case <-ctx.Done():
			logger.Debug("worker stopping")
			return
		default:
		}

		job, err := p.queue.Pop(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			logger.Error("failed to pop job", zap.Error(err))
			time.Sleep(time.Second) // Backoff on error.
			continue
		}

		p.processJob(ctx, logger, job)
	}
}

func (p *Pool) processJob(ctx context.Context, logger *zap.Logger, job *queue.Job) {
	logger = logger.With(zap.String("job_id", job.ID))

	// Mark as processing.
	job.Status = queue.StatusProcessing
	if err := p.queue.Update(ctx, job); err != nil {
		logger.Error("failed to update job status", zap.Error(err))
	}

	// Execute.
	result := p.executor.Execute(ctx, job)

	// Record stats.
	p.statsMu.Lock()
	p.stats.Record(fhe.OperationType(job.Operation), result)
	p.statsMu.Unlock()

	// Update job status.
	if result.Error != nil {
		job.Status = queue.StatusFailed
		job.Error = result.Error.Error()
		logger.Error("job failed", zap.Error(result.Error))
	} else {
		job.Status = queue.StatusCompleted
		job.ResultHandle = string(result.ResultHandle)
		logger.Info("job completed", zap.Duration("duration", result.Duration))
	}

	if err := p.queue.Update(ctx, job); err != nil {
		logger.Error("failed to update job result", zap.Error(err))
	}
}

// HealthCheck returns nil if the pool is healthy.
func (p *Pool) HealthCheck() error {
	if !p.running.Load() {
		return errors.New("pool not running")
	}
	return nil
}
