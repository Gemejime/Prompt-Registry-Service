# syntax=docker/dockerfile:1.7

# Stage 1: Hardened Static Go Builder
FROM golang:1.26-alpine AS builder

ARG VERSION=1.0.0
ARG TARGETOS=linux
ARG TARGETARCH

WORKDIR /src

# Download dependencies first for deterministic layer caching
COPY go.mod go.sum ./
RUN go mod download

# Copy application source and contracts
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY pkg/ ./pkg/
COPY proto/ ./proto/
COPY openapi/ ./openapi/

# Compile static binary with CGO disabled and stripped symbols
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/prompt-registry-server \
    ./cmd/prompt-registry-server

# Stage 2: Minimal Distroless Non-Root Runtime (Layer 4 & Layer 5)
FROM gcr.io/distroless/static-debian12:nonroot

LABEL org.opencontainers.image.title="rag-prompt-registry-service" \
      org.opencontainers.image.description="Control plane service for RAG Prompt Registry" \
      org.opencontainers.image.vendor="RAG Platform"

WORKDIR /app

COPY --from=builder /out/prompt-registry-server /app/prompt-registry-server
COPY --from=builder /src/openapi/openapi.yaml /app/openapi/openapi.yaml

# Non-root distroless UID/GID (65532:65532), compatible with readOnlyRootFilesystem: true
USER 65532:65532

# 8080: HTTP REST, OpenAPI Doc API (/docs, /redoc, /openapi.yaml), Probes (/healthz, /ready), Metrics (/metrics)
# 9090: gRPC Control Plane Management API
EXPOSE 8080 9090

HEALTHCHECK --interval=15s --timeout=3s --start-period=5s --retries=3 \
    CMD ["/app/prompt-registry-server", "-healthcheck"]

ENTRYPOINT ["/app/prompt-registry-server"]

