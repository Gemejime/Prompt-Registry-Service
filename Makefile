.PHONY: all build run fmt vet lint test test-unit test-integration bench security-audit clean

BINARY_NAME := prompt-registry-server
GO := go

all: fmt vet test build

build:
	$(GO) build -trimpath -ldflags="-s -w" -o bin/$(BINARY_NAME) ./cmd/prompt-registry-server

run:
	$(GO) run ./cmd/prompt-registry-server

fmt:
	gofmt -w -s .

vet:
	$(GO) vet ./...

lint:
	golangci-lint run --timeout=5m

test: test-unit test-integration

test-unit:
	$(GO) test -v -race -covermode=atomic -coverprofile=coverage.out ./test/unit/...

test-integration:
	$(GO) test -v ./test/integration/...

bench:
	$(GO) test -v -bench=. -benchmem ./pkg/promptsdk/...

security-audit:
	@echo "Auditing codebase for secured logging invariants..."
	@if grep -rnE '(log|slog|zap|logrus)\.(Print|Info|Debug|Warn|Error).*(raw_prompt|raw_context|password|secret|bearer|authorization)' . --exclude-dir={.git,.github,vendor,tasks,test}; then \
		echo "CRITICAL: Potential unredacted sensitive payload logging detected"; \
		exit 1; \
	fi
	@echo "Secured logging audit passed."

clean:
	rm -rf bin/ coverage.out

