// Package storage provides ciphertext storage and retrieval.
package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Common errors.
var (
	ErrNotFound      = errors.New("ciphertext not found")
	ErrStorageFull   = errors.New("storage capacity exceeded")
	ErrInvalidHandle = errors.New("invalid ciphertext handle")
	ErrAlreadyExists = errors.New("ciphertext already exists")
)

// Handle uniquely identifies a ciphertext.
type Handle string

// ComputeHandle generates a handle from ciphertext data.
func ComputeHandle(data []byte) Handle {
	hash := sha256.Sum256(data)
	return Handle(hex.EncodeToString(hash[:]))
}

// Storage defines the interface for ciphertext storage.
type Storage interface {
	// Store saves a ciphertext and returns its handle.
	Store(ctx context.Context, data []byte) (Handle, error)
	// Load retrieves a ciphertext by handle.
	Load(ctx context.Context, handle Handle) ([]byte, error)
	// Delete removes a ciphertext.
	Delete(ctx context.Context, handle Handle) error
	// Exists checks if a ciphertext exists.
	Exists(ctx context.Context, handle Handle) (bool, error)
	// Close closes the storage.
	Close() error
}

// MemoryStorage implements in-memory ciphertext storage.
type MemoryStorage struct {
	mu       sync.RWMutex
	data     map[Handle][]byte
	capacity int64
	size     int64
}

// NewMemoryStorage creates a new in-memory storage.
func NewMemoryStorage(capacityMB int64) *MemoryStorage {
	return &MemoryStorage{
		data:     make(map[Handle][]byte),
		capacity: capacityMB * 1024 * 1024,
	}
}

func (s *MemoryStorage) Store(ctx context.Context, data []byte) (Handle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	handle := ComputeHandle(data)

	if _, exists := s.data[handle]; exists {
		return handle, nil // Dedup by content hash.
	}

	if s.size+int64(len(data)) > s.capacity {
		return "", ErrStorageFull
	}

	s.data[handle] = append([]byte(nil), data...)
	s.size += int64(len(data))

	return handle, nil
}

func (s *MemoryStorage) Load(ctx context.Context, handle Handle) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	data, exists := s.data[handle]
	if !exists {
		return nil, ErrNotFound
	}

	return append([]byte(nil), data...), nil
}

func (s *MemoryStorage) Delete(ctx context.Context, handle Handle) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, exists := s.data[handle]
	if !exists {
		return ErrNotFound
	}

	s.size -= int64(len(data))
	delete(s.data, handle)
	return nil
}

func (s *MemoryStorage) Exists(ctx context.Context, handle Handle) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	_, exists := s.data[handle]
	return exists, nil
}

func (s *MemoryStorage) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.data = nil
	s.size = 0
	return nil
}

// FileStorage implements file-based ciphertext storage.
type FileStorage struct {
	baseDir string
}

// NewFileStorage creates a new file-based storage.
func NewFileStorage(baseDir string) (*FileStorage, error) {
	if err := os.MkdirAll(baseDir, 0750); err != nil {
		return nil, fmt.Errorf("create storage dir: %w", err)
	}

	return &FileStorage{baseDir: baseDir}, nil
}

func (s *FileStorage) path(handle Handle) string {
	h := string(handle)
	if len(h) < 4 {
		return filepath.Join(s.baseDir, h)
	}
	// Shard by first 2 chars to avoid too many files in one directory.
	return filepath.Join(s.baseDir, h[:2], h)
}

func (s *FileStorage) Store(ctx context.Context, data []byte) (Handle, error) {
	handle := ComputeHandle(data)
	path := s.path(handle)

	if _, err := os.Stat(path); err == nil {
		return handle, nil // Already exists (dedup).
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return "", fmt.Errorf("create shard dir: %w", err)
	}

	// Write atomically via temp file.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return "", fmt.Errorf("write temp file: %w", err)
	}

	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("rename temp file: %w", err)
	}

	return handle, nil
}

func (s *FileStorage) Load(ctx context.Context, handle Handle) ([]byte, error) {
	data, err := os.ReadFile(s.path(handle))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("read file: %w", err)
	}
	return data, nil
}

func (s *FileStorage) Delete(ctx context.Context, handle Handle) error {
	if err := os.Remove(s.path(handle)); err != nil {
		if os.IsNotExist(err) {
			return ErrNotFound
		}
		return fmt.Errorf("remove file: %w", err)
	}
	return nil
}

func (s *FileStorage) Exists(ctx context.Context, handle Handle) (bool, error) {
	_, err := os.Stat(s.path(handle))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, fmt.Errorf("stat file: %w", err)
}

func (s *FileStorage) Close() error {
	return nil
}

// RedisStorage implements Redis-based ciphertext storage.
type RedisStorage struct {
	client *redis.Client
	prefix string
	ttl    time.Duration
}

// RedisStorageConfig holds Redis storage settings.
type RedisStorageConfig struct {
	Addr     string
	Password string
	DB       int
	Prefix   string
	TTL      time.Duration
}

// NewRedisStorage creates a new Redis-backed storage.
func NewRedisStorage(cfg RedisStorageConfig) (*RedisStorage, error) {
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

	prefix := cfg.Prefix
	if prefix == "" {
		prefix = "fhe:ct:"
	}

	ttl := cfg.TTL
	if ttl == 0 {
		ttl = 24 * time.Hour
	}

	return &RedisStorage{
		client: client,
		prefix: prefix,
		ttl:    ttl,
	}, nil
}

func (s *RedisStorage) key(handle Handle) string {
	return s.prefix + string(handle)
}

func (s *RedisStorage) Store(ctx context.Context, data []byte) (Handle, error) {
	handle := ComputeHandle(data)

	err := s.client.Set(ctx, s.key(handle), data, s.ttl).Err()
	if err != nil {
		return "", fmt.Errorf("redis set: %w", err)
	}

	return handle, nil
}

func (s *RedisStorage) Load(ctx context.Context, handle Handle) ([]byte, error) {
	data, err := s.client.Get(ctx, s.key(handle)).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("redis get: %w", err)
	}
	return data, nil
}

func (s *RedisStorage) Delete(ctx context.Context, handle Handle) error {
	result, err := s.client.Del(ctx, s.key(handle)).Result()
	if err != nil {
		return fmt.Errorf("redis del: %w", err)
	}
	if result == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *RedisStorage) Exists(ctx context.Context, handle Handle) (bool, error) {
	result, err := s.client.Exists(ctx, s.key(handle)).Result()
	if err != nil {
		return false, fmt.Errorf("redis exists: %w", err)
	}
	return result > 0, nil
}

func (s *RedisStorage) Close() error {
	return s.client.Close()
}

// CachedStorage wraps a storage with an in-memory LRU cache.
type CachedStorage struct {
	backend Storage
	cache   *MemoryStorage
}

// NewCachedStorage creates a cached storage layer.
func NewCachedStorage(backend Storage, cacheMB int64) *CachedStorage {
	return &CachedStorage{
		backend: backend,
		cache:   NewMemoryStorage(cacheMB),
	}
}

func (s *CachedStorage) Store(ctx context.Context, data []byte) (Handle, error) {
	handle, err := s.backend.Store(ctx, data)
	if err != nil {
		return "", err
	}

	// Cache on write (ignore cache errors).
	s.cache.Store(ctx, data)

	return handle, nil
}

func (s *CachedStorage) Load(ctx context.Context, handle Handle) ([]byte, error) {
	// Try cache first.
	if data, err := s.cache.Load(ctx, handle); err == nil {
		return data, nil
	}

	// Fall back to backend.
	data, err := s.backend.Load(ctx, handle)
	if err != nil {
		return nil, err
	}

	// Populate cache (ignore errors).
	s.cache.Store(ctx, data)

	return data, nil
}

func (s *CachedStorage) Delete(ctx context.Context, handle Handle) error {
	s.cache.Delete(ctx, handle)
	return s.backend.Delete(ctx, handle)
}

func (s *CachedStorage) Exists(ctx context.Context, handle Handle) (bool, error) {
	if exists, _ := s.cache.Exists(ctx, handle); exists {
		return true, nil
	}
	return s.backend.Exists(ctx, handle)
}

func (s *CachedStorage) Close() error {
	s.cache.Close()
	if closer, ok := s.backend.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}
