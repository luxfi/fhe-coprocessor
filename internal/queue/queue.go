// Package queue provides job queue abstractions for FHE computation requests.
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"
)

// Common errors.
var (
	ErrQueueEmpty     = errors.New("queue is empty")
	ErrJobNotFound    = errors.New("job not found")
	ErrConnectionLost = errors.New("queue connection lost")
)

// JobStatus represents the state of a job.
type JobStatus uint8

const (
	StatusPending JobStatus = iota
	StatusProcessing
	StatusCompleted
	StatusFailed
)

// Job represents an FHE computation request.
type Job struct {
	ID           string    `json:"id"`
	Operation    uint8     `json:"operation"`
	LHSHandle    string    `json:"lhs_handle"`
	RHSHandle    string    `json:"rhs_handle,omitempty"`
	ResultHandle string    `json:"result_handle,omitempty"`
	Status       JobStatus `json:"status"`
	Error        string    `json:"error,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Queue defines the interface for job queue operations.
type Queue interface {
	// Push adds a job to the queue.
	Push(ctx context.Context, job *Job) error
	// Pop retrieves and removes the next job from the queue.
	Pop(ctx context.Context) (*Job, error)
	// Update updates job status.
	Update(ctx context.Context, job *Job) error
	// Get retrieves a job by ID.
	Get(ctx context.Context, id string) (*Job, error)
	// Close closes the queue connection.
	Close() error
}

// RedisQueue implements Queue using Redis.
type RedisQueue struct {
	client    *redis.Client
	queueKey  string
	jobPrefix string
}

// RedisConfig holds Redis connection settings.
type RedisConfig struct {
	Addr     string
	Password string
	DB       int
}

// NewRedisQueue creates a new Redis-backed queue.
func NewRedisQueue(cfg RedisConfig, queueName string) (*RedisQueue, error) {
	client := redis.NewClient(&redis.Options{
		Addr:     cfg.Addr,
		Password: cfg.Password,
		DB:       cfg.DB,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("redis ping: %w", err)
	}

	return &RedisQueue{
		client:    client,
		queueKey:  "fhe:queue:" + queueName,
		jobPrefix: "fhe:job:",
	}, nil
}

func (q *RedisQueue) Push(ctx context.Context, job *Job) error {
	job.CreatedAt = time.Now()
	job.UpdatedAt = job.CreatedAt
	job.Status = StatusPending

	data, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("marshal job: %w", err)
	}

	pipe := q.client.Pipeline()
	pipe.Set(ctx, q.jobPrefix+job.ID, data, 24*time.Hour)
	pipe.LPush(ctx, q.queueKey, job.ID)

	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("push job: %w", err)
	}

	return nil
}

func (q *RedisQueue) Pop(ctx context.Context) (*Job, error) {
	result, err := q.client.BRPop(ctx, 0, q.queueKey).Result()
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, fmt.Errorf("pop job: %w", err)
	}

	if len(result) < 2 {
		return nil, ErrQueueEmpty
	}

	jobID := result[1]
	return q.Get(ctx, jobID)
}

func (q *RedisQueue) Update(ctx context.Context, job *Job) error {
	job.UpdatedAt = time.Now()

	data, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("marshal job: %w", err)
	}

	if err := q.client.Set(ctx, q.jobPrefix+job.ID, data, 24*time.Hour).Err(); err != nil {
		return fmt.Errorf("update job: %w", err)
	}

	return nil
}

func (q *RedisQueue) Get(ctx context.Context, id string) (*Job, error) {
	data, err := q.client.Get(ctx, q.jobPrefix+id).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, ErrJobNotFound
		}
		return nil, fmt.Errorf("get job: %w", err)
	}

	var job Job
	if err := json.Unmarshal(data, &job); err != nil {
		return nil, fmt.Errorf("unmarshal job: %w", err)
	}

	return &job, nil
}

func (q *RedisQueue) Close() error {
	return q.client.Close()
}

// NATSQueue implements Queue using NATS JetStream.
type NATSQueue struct {
	conn   *nats.Conn
	js     nats.JetStreamContext
	sub    *nats.Subscription
	stream string
}

// NATSConfig holds NATS connection settings.
type NATSConfig struct {
	URL    string
	Stream string
}

// NewNATSQueue creates a new NATS-backed queue.
func NewNATSQueue(cfg NATSConfig) (*NATSQueue, error) {
	nc, err := nats.Connect(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("nats connect: %w", err)
	}

	js, err := nc.JetStream()
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("jetstream: %w", err)
	}

	// Create stream if not exists.
	streamName := cfg.Stream
	if streamName == "" {
		streamName = "FHE_JOBS"
	}

	_, err = js.AddStream(&nats.StreamConfig{
		Name:      streamName,
		Subjects:  []string{"fhe.jobs.*"},
		Retention: nats.WorkQueuePolicy,
		MaxAge:    24 * time.Hour,
	})
	if err != nil && !errors.Is(err, nats.ErrStreamNameAlreadyInUse) {
		nc.Close()
		return nil, fmt.Errorf("create stream: %w", err)
	}

	return &NATSQueue{
		conn:   nc,
		js:     js,
		stream: streamName,
	}, nil
}

func (q *NATSQueue) Push(ctx context.Context, job *Job) error {
	job.CreatedAt = time.Now()
	job.UpdatedAt = job.CreatedAt
	job.Status = StatusPending

	data, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("marshal job: %w", err)
	}

	_, err = q.js.Publish("fhe.jobs.pending", data, nats.Context(ctx))
	if err != nil {
		return fmt.Errorf("publish job: %w", err)
	}

	return nil
}

func (q *NATSQueue) Pop(ctx context.Context) (*Job, error) {
	if q.sub == nil {
		sub, err := q.js.PullSubscribe("fhe.jobs.pending", "fhe-workers")
		if err != nil {
			return nil, fmt.Errorf("subscribe: %w", err)
		}
		q.sub = sub
	}

	msgs, err := q.sub.Fetch(1, nats.Context(ctx))
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}

	if len(msgs) == 0 {
		return nil, ErrQueueEmpty
	}

	msg := msgs[0]
	var job Job
	if err := json.Unmarshal(msg.Data, &job); err != nil {
		msg.Nak()
		return nil, fmt.Errorf("unmarshal job: %w", err)
	}

	if err := msg.Ack(); err != nil {
		return nil, fmt.Errorf("ack: %w", err)
	}

	return &job, nil
}

func (q *NATSQueue) Update(ctx context.Context, job *Job) error {
	job.UpdatedAt = time.Now()

	data, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("marshal job: %w", err)
	}

	subject := fmt.Sprintf("fhe.jobs.%s", job.ID)
	_, err = q.js.Publish(subject, data, nats.Context(ctx))
	if err != nil {
		return fmt.Errorf("publish update: %w", err)
	}

	return nil
}

func (q *NATSQueue) Get(ctx context.Context, id string) (*Job, error) {
	// NATS JetStream doesn't have direct key-value lookup for messages.
	// In production, you'd use NATS KV store or a separate storage layer.
	return nil, errors.New("get not implemented for NATS queue; use storage layer")
}

func (q *NATSQueue) Close() error {
	if q.sub != nil {
		q.sub.Unsubscribe()
	}
	q.conn.Close()
	return nil
}
