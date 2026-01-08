# FHE Coprocessor

Fully Homomorphic Encryption (FHE) computation workers for the Lux blockchain.

## Architecture

```
┌─────────────┐     ┌─────────────┐     ┌─────────────┐
│  Blockchain │────▶│   Gateway   │────▶│    Redis    │
│   Events    │     │  (listener) │     │   (queue)   │
└─────────────┘     └─────────────┘     └──────┬──────┘
                                               │
                    ┌─────────────┐            │
                    │  Scheduler  │◀───────────┘
                    │ (coordinator)│
                    └─────────────┘
                           │
           ┌───────────────┼───────────────┐
           ▼               ▼               ▼
    ┌───────────┐   ┌───────────┐   ┌───────────┐
    │  Worker   │   │  Worker   │   │  Worker   │
    │  (TFHE)   │   │  (TFHE)   │   │  (TFHE)   │
    └───────────┘   └───────────┘   └───────────┘
```

## Components

- **Gateway**: Listens to blockchain events, submits FHE computation jobs
- **Scheduler**: Coordinates worker registration, health, and job distribution
- **Worker**: Executes FHE operations using TFHE library

## Quick Start

### Prerequisites

- Go 1.22+
- Docker & Docker Compose
- Redis (or use docker-compose)

### Build

```bash
make build
```

### Run with Docker Compose

```bash
# Start all services
docker compose up -d

# Scale workers
docker compose up -d --scale worker=4

# View logs
docker compose logs -f worker
```

### Run Locally

```bash
# Start Redis
docker run -d -p 6379:6379 redis:alpine

# Start scheduler
./bin/scheduler -redis localhost:6379

# Start gateway
./bin/gateway -redis localhost:6379

# Start worker(s)
./bin/tfhe-worker -redis localhost:6379 -workers 4
```

## Configuration

### Worker Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-workers` | 4 | Number of worker goroutines |
| `-redis` | localhost:6379 | Redis address |
| `-storage` | /tmp/fhe-storage | Ciphertext storage path |
| `-server-key` | "" | Path to TFHE server key |
| `-metrics` | :9090 | Metrics server address |
| `-log-level` | info | Log level |

### Gateway Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-start-block` | 0 | Block to start scanning (0 = latest) |
| `-confirmations` | 2 | Required block confirmations |
| `-redis` | localhost:6379 | Redis address |
| `-http` | :8080 | HTTP API address |

### Scheduler Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-redis` | localhost:6379 | Redis address |
| `-http` | :8081 | HTTP API address |
| `-cleanup-period` | 30s | Stale worker cleanup interval |
| `-worker-timeout` | 60s | Worker heartbeat timeout |

## API

### Gateway

```bash
# Health check
curl http://localhost:8080/health

# Get last processed block
curl http://localhost:8080/status

# Get job status
curl http://localhost:8080/job/{job_id}
```

### Scheduler

```bash
# Health check
curl http://localhost:8081/health

# List workers
curl http://localhost:8081/workers

# Get cluster status
curl http://localhost:8081/status

# Submit job manually
curl -X POST http://localhost:8081/submit \
  -H "Content-Type: application/json" \
  -d '{"id":"job1","operation":0,"lhs_handle":"abc","rhs_handle":"def"}'
```

### Worker Metrics

```bash
# Health check
curl http://localhost:9090/health

# Prometheus metrics
curl http://localhost:9090/metrics
```

## FHE Operations

| Code | Operation | Description |
|------|-----------|-------------|
| 0 | Add | Addition |
| 1 | Sub | Subtraction |
| 2 | Mul | Multiplication |
| 3 | Not | Bitwise NOT |
| 4 | And | Bitwise AND |
| 5 | Or | Bitwise OR |
| 6 | Xor | Bitwise XOR |
| 7 | Eq | Equality |
| 8 | Ne | Not equal |
| 9 | Gt | Greater than |
| 10 | Ge | Greater or equal |
| 11 | Lt | Less than |
| 12 | Le | Less or equal |
| 13 | Shl | Shift left |
| 14 | Shr | Shift right |
| 15 | Rotl | Rotate left |
| 16 | Rotr | Rotate right |
| 17 | Min | Minimum |
| 18 | Max | Maximum |
| 19 | Neg | Negation |

## Development

```bash
# Format code
make fmt

# Run linter
make lint

# Run tests
make test

# Run tests with coverage
make test-coverage

# Build Docker images
make docker
```

## License

Apache-2.0
