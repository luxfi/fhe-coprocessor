// Package gateway provides blockchain event listening and job submission.
package gateway

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/luxfi/fhe-coprocessor/internal/queue"
	"github.com/luxfi/fhe-coprocessor/internal/storage"
	"go.uber.org/zap"
)

// EventType represents blockchain FHE events.
type EventType uint8

const (
	EventCompute EventType = iota
	EventStore
	EventDecrypt
)

// FHEEvent represents an FHE operation request from the blockchain.
type FHEEvent struct {
	TxHash      string
	BlockNumber uint64
	EventType   EventType
	Operation   uint8
	LHSHandle   string
	RHSHandle   string
	Caller      string
	Timestamp   time.Time
}

// BlockchainClient defines the interface for blockchain interaction.
type BlockchainClient interface {
	// SubscribeEvents subscribes to FHE events starting from a block.
	SubscribeEvents(ctx context.Context, fromBlock uint64, events chan<- FHEEvent) error
	// GetLatestBlock returns the latest block number.
	GetLatestBlock(ctx context.Context) (uint64, error)
	// SubmitResult submits computation result back to the blockchain.
	SubmitResult(ctx context.Context, jobID string, resultHandle string) error
}

// Config holds gateway configuration.
type Config struct {
	// StartBlock is the block to start scanning from (0 = latest).
	StartBlock uint64
	// ConfirmationBlocks is the number of confirmations required.
	ConfirmationBlocks uint64
	// PollInterval is the block polling interval when not using subscriptions.
	PollInterval time.Duration
	// BatchSize is the maximum events to process per batch.
	BatchSize int
}

// DefaultConfig returns sensible defaults.
func DefaultConfig() Config {
	return Config{
		StartBlock:         0,
		ConfirmationBlocks: 2,
		PollInterval:       2 * time.Second,
		BatchSize:          100,
	}
}

// Listener listens to blockchain events and submits FHE computation jobs.
type Listener struct {
	cfg     Config
	client  BlockchainClient
	queue   queue.Queue
	storage storage.Storage
	logger  *zap.Logger

	lastBlock uint64
	mu        sync.RWMutex
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

// NewListener creates a new blockchain event listener.
func NewListener(cfg Config, client BlockchainClient, q queue.Queue, store storage.Storage, logger *zap.Logger) *Listener {
	if cfg.PollInterval == 0 {
		cfg.PollInterval = DefaultConfig().PollInterval
	}
	if cfg.BatchSize == 0 {
		cfg.BatchSize = DefaultConfig().BatchSize
	}

	return &Listener{
		cfg:     cfg,
		client:  client,
		queue:   q,
		storage: store,
		logger:  logger,
	}
}

// Start starts listening to blockchain events.
func (l *Listener) Start(ctx context.Context) error {
	ctx, l.cancel = context.WithCancel(ctx)

	startBlock := l.cfg.StartBlock
	if startBlock == 0 {
		latest, err := l.client.GetLatestBlock(ctx)
		if err != nil {
			return fmt.Errorf("get latest block: %w", err)
		}
		startBlock = latest
	}

	l.mu.Lock()
	l.lastBlock = startBlock
	l.mu.Unlock()

	l.logger.Info("starting event listener", zap.Uint64("start_block", startBlock))

	events := make(chan FHEEvent, l.cfg.BatchSize)

	l.wg.Add(2)
	go l.subscribe(ctx, startBlock, events)
	go l.processEvents(ctx, events)

	return nil
}

// Stop gracefully stops the listener.
func (l *Listener) Stop() error {
	if l.cancel != nil {
		l.cancel()
	}
	l.wg.Wait()
	return nil
}

// LastBlock returns the last processed block number.
func (l *Listener) LastBlock() uint64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.lastBlock
}

func (l *Listener) subscribe(ctx context.Context, fromBlock uint64, events chan<- FHEEvent) {
	defer l.wg.Done()
	defer close(events)

	err := l.client.SubscribeEvents(ctx, fromBlock, events)
	if err != nil && !errors.Is(err, context.Canceled) {
		l.logger.Error("subscription error", zap.Error(err))
	}
}

func (l *Listener) processEvents(ctx context.Context, events <-chan FHEEvent) {
	defer l.wg.Done()

	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-events:
			if !ok {
				return
			}
			l.handleEvent(ctx, event)
		}
	}
}

func (l *Listener) handleEvent(ctx context.Context, event FHEEvent) {
	logger := l.logger.With(
		zap.String("tx_hash", event.TxHash),
		zap.Uint64("block", event.BlockNumber),
		zap.Uint8("event_type", uint8(event.EventType)),
	)

	logger.Debug("processing event")

	switch event.EventType {
	case EventCompute:
		l.handleComputeEvent(ctx, logger, event)
	case EventStore:
		l.handleStoreEvent(ctx, logger, event)
	case EventDecrypt:
		l.handleDecryptEvent(ctx, logger, event)
	default:
		logger.Warn("unknown event type")
	}

	l.mu.Lock()
	if event.BlockNumber > l.lastBlock {
		l.lastBlock = event.BlockNumber
	}
	l.mu.Unlock()
}

func (l *Listener) handleComputeEvent(ctx context.Context, logger *zap.Logger, event FHEEvent) {
	jobID := generateJobID(event.TxHash, event.BlockNumber)

	job := &queue.Job{
		ID:        jobID,
		Operation: event.Operation,
		LHSHandle: event.LHSHandle,
		RHSHandle: event.RHSHandle,
	}

	if err := l.queue.Push(ctx, job); err != nil {
		logger.Error("failed to enqueue job", zap.Error(err))
		return
	}

	logger.Info("compute job enqueued", zap.String("job_id", jobID))
}

func (l *Listener) handleStoreEvent(ctx context.Context, logger *zap.Logger, event FHEEvent) {
	// Store events contain ciphertext data in LHSHandle (as hex).
	data, err := hex.DecodeString(event.LHSHandle)
	if err != nil {
		logger.Error("invalid ciphertext hex", zap.Error(err))
		return
	}

	handle, err := l.storage.Store(ctx, data)
	if err != nil {
		logger.Error("failed to store ciphertext", zap.Error(err))
		return
	}

	logger.Info("ciphertext stored", zap.String("handle", string(handle)))
}

func (l *Listener) handleDecryptEvent(ctx context.Context, logger *zap.Logger, event FHEEvent) {
	// Decryption requests require special handling with key management.
	// For now, we just log them.
	logger.Info("decrypt request received", zap.String("handle", event.LHSHandle))

	// TODO: Implement threshold decryption protocol.
}

func generateJobID(txHash string, blockNumber uint64) string {
	// Combine tx hash and block for uniqueness.
	blockBytes := big.NewInt(int64(blockNumber)).Bytes()
	combined := append([]byte(txHash), blockBytes...)
	// Simple hash for job ID.
	hash := make([]byte, 16)
	for i, b := range combined {
		hash[i%16] ^= b
	}
	return hex.EncodeToString(hash)
}

// MockBlockchainClient is a simple mock for testing.
type MockBlockchainClient struct {
	latestBlock uint64
	events      []FHEEvent
	mu          sync.Mutex
}

// NewMockBlockchainClient creates a mock client.
func NewMockBlockchainClient() *MockBlockchainClient {
	return &MockBlockchainClient{latestBlock: 100}
}

func (m *MockBlockchainClient) SubscribeEvents(ctx context.Context, fromBlock uint64, events chan<- FHEEvent) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			m.mu.Lock()
			for _, e := range m.events {
				select {
				case events <- e:
				default:
				}
			}
			m.events = nil
			m.mu.Unlock()
		}
	}
}

func (m *MockBlockchainClient) GetLatestBlock(ctx context.Context) (uint64, error) {
	return m.latestBlock, nil
}

func (m *MockBlockchainClient) SubmitResult(ctx context.Context, jobID string, resultHandle string) error {
	return nil
}

// AddEvent adds an event to the mock.
func (m *MockBlockchainClient) AddEvent(event FHEEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, event)
}
