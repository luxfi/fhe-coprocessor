# Build stage
FROM golang:1.22-alpine AS builder

RUN apk add --no-cache git make

WORKDIR /app

# Copy go mod files first for better caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build all binaries
RUN make build

# Worker image
FROM alpine:3.19 AS worker

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

COPY --from=builder /app/bin/tfhe-worker /usr/local/bin/tfhe-worker

# Create non-root user
RUN addgroup -S fhe && adduser -S fhe -G fhe
USER fhe

# Storage directory
VOLUME /data/storage

EXPOSE 9090

ENTRYPOINT ["tfhe-worker"]
CMD ["-storage", "/data/storage", "-metrics", ":9090"]

# Gateway image
FROM alpine:3.19 AS gateway

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

COPY --from=builder /app/bin/gateway /usr/local/bin/gateway

RUN addgroup -S fhe && adduser -S fhe -G fhe
USER fhe

VOLUME /data/storage

EXPOSE 8080

ENTRYPOINT ["gateway"]
CMD ["-storage", "/data/storage", "-http", ":8080"]

# Scheduler image
FROM alpine:3.19 AS scheduler

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

COPY --from=builder /app/bin/scheduler /usr/local/bin/scheduler

RUN addgroup -S fhe && adduser -S fhe -G fhe
USER fhe

EXPOSE 8081

ENTRYPOINT ["scheduler"]
CMD ["-http", ":8081"]
