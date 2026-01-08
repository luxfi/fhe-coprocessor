.PHONY: all build test clean fmt lint docker

GO := go
GOFLAGS := -trimpath
LDFLAGS := -s -w

BINDIR := bin
CMDS := tfhe-worker gateway scheduler

all: build

build: $(CMDS)

$(CMDS):
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BINDIR)/$@ ./cmd/$@

test:
	$(GO) test -v -race ./...

test-coverage:
	$(GO) test -v -race -coverprofile=coverage.out ./...
	$(GO) tool cover -html=coverage.out -o coverage.html

clean:
	rm -rf $(BINDIR) coverage.out coverage.html

fmt:
	$(GO) fmt ./...
	gofumpt -w .

lint:
	golangci-lint run ./...

deps:
	$(GO) mod download
	$(GO) mod tidy

docker:
	docker build -t luxfi/fhe-worker:latest -f Dockerfile --target worker .
	docker build -t luxfi/fhe-gateway:latest -f Dockerfile --target gateway .
	docker build -t luxfi/fhe-scheduler:latest -f Dockerfile --target scheduler .

docker-push:
	docker push luxfi/fhe-worker:latest
	docker push luxfi/fhe-gateway:latest
	docker push luxfi/fhe-scheduler:latest

# Development targets
dev-worker: build
	./$(BINDIR)/tfhe-worker -log-level debug

dev-gateway: build
	./$(BINDIR)/gateway -log-level debug

dev-scheduler: build
	./$(BINDIR)/scheduler -log-level debug

# Run all services locally
dev: build
	@echo "Starting Redis..."
	docker run -d --name fhe-redis -p 6379:6379 redis:alpine || true
	@echo "Starting services..."
	./$(BINDIR)/scheduler -log-level debug &
	./$(BINDIR)/gateway -log-level debug &
	./$(BINDIR)/tfhe-worker -log-level debug -workers 2

stop:
	-pkill -f tfhe-worker
	-pkill -f gateway
	-pkill -f scheduler
	docker stop fhe-redis || true
	docker rm fhe-redis || true
