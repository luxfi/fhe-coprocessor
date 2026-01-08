// Package worker provides FHE computation worker functionality.
package worker

import (
	"context"
	"fmt"
	"time"

	"github.com/luxfi/fhe-coprocessor/internal/queue"
	"github.com/luxfi/fhe-coprocessor/internal/storage"
	"github.com/luxfi/fhe-coprocessor/pkg/fhe"
	"go.uber.org/zap"
)

// OperationExecutor executes FHE operations.
type OperationExecutor struct {
	adapter *fhe.Adapter
	storage storage.Storage
	logger  *zap.Logger
}

// NewOperationExecutor creates a new operation executor.
func NewOperationExecutor(serverKey []byte, store storage.Storage, logger *zap.Logger) (*OperationExecutor, error) {
	adapter, err := fhe.NewAdapter(serverKey)
	if err != nil {
		return nil, fmt.Errorf("create tfhe adapter: %w", err)
	}

	return &OperationExecutor{
		adapter: adapter,
		storage: store,
		logger:  logger,
	}, nil
}

// ExecutionResult contains the result of an FHE operation.
type ExecutionResult struct {
	ResultHandle storage.Handle
	Duration     time.Duration
	Error        error
}

// Execute performs the FHE operation specified in the job.
func (e *OperationExecutor) Execute(ctx context.Context, job *queue.Job) ExecutionResult {
	start := time.Now()

	logger := e.logger.With(
		zap.String("job_id", job.ID),
		zap.Uint8("operation", job.Operation),
	)

	logger.Debug("executing operation")

	// Load LHS ciphertext.
	lhsData, err := e.storage.Load(ctx, storage.Handle(job.LHSHandle))
	if err != nil {
		logger.Error("failed to load lhs", zap.Error(err))
		return ExecutionResult{Error: fmt.Errorf("load lhs: %w", err)}
	}

	lhs := &fhe.Ciphertext{Data: lhsData, BitLen: 64} // TODO: Get bitlen from metadata.

	var rhs *fhe.Ciphertext
	if job.RHSHandle != "" {
		rhsData, err := e.storage.Load(ctx, storage.Handle(job.RHSHandle))
		if err != nil {
			logger.Error("failed to load rhs", zap.Error(err))
			return ExecutionResult{Error: fmt.Errorf("load rhs: %w", err)}
		}
		rhs = &fhe.Ciphertext{Data: rhsData, BitLen: 64}
	}

	// Execute the operation.
	result, err := e.adapter.Execute(ctx, fhe.OperationType(job.Operation), lhs, rhs)
	if err != nil {
		logger.Error("operation failed", zap.Error(err))
		return ExecutionResult{Error: fmt.Errorf("execute: %w", err), Duration: time.Since(start)}
	}

	// Store the result.
	handle, err := e.storage.Store(ctx, result.Data)
	if err != nil {
		logger.Error("failed to store result", zap.Error(err))
		return ExecutionResult{Error: fmt.Errorf("store result: %w", err), Duration: time.Since(start)}
	}

	duration := time.Since(start)
	logger.Info("operation completed",
		zap.Duration("duration", duration),
		zap.String("result_handle", string(handle)),
	)

	return ExecutionResult{
		ResultHandle: handle,
		Duration:     duration,
	}
}

// OperationStats tracks execution statistics.
type OperationStats struct {
	TotalExecutions  int64
	SuccessCount     int64
	FailureCount     int64
	TotalDuration    time.Duration
	OperationCounts  map[fhe.OperationType]int64
	OperationTimings map[fhe.OperationType]time.Duration
}

// NewOperationStats creates a new stats tracker.
func NewOperationStats() *OperationStats {
	return &OperationStats{
		OperationCounts:  make(map[fhe.OperationType]int64),
		OperationTimings: make(map[fhe.OperationType]time.Duration),
	}
}

// Record records an execution result.
func (s *OperationStats) Record(op fhe.OperationType, result ExecutionResult) {
	s.TotalExecutions++
	s.TotalDuration += result.Duration
	s.OperationCounts[op]++
	s.OperationTimings[op] += result.Duration

	if result.Error != nil {
		s.FailureCount++
	} else {
		s.SuccessCount++
	}
}

// AverageDuration returns the average operation duration.
func (s *OperationStats) AverageDuration() time.Duration {
	if s.TotalExecutions == 0 {
		return 0
	}
	return time.Duration(int64(s.TotalDuration) / s.TotalExecutions)
}

// SuccessRate returns the success rate as a percentage.
func (s *OperationStats) SuccessRate() float64 {
	if s.TotalExecutions == 0 {
		return 0
	}
	return float64(s.SuccessCount) / float64(s.TotalExecutions) * 100
}
