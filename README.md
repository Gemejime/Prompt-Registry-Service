# RAG Prompt Registry Service

[![Go Version](https://img.shields.io/badge/go-1.26.9%2B-blue.svg)](https://golang.org)
[![License](https://img.shields.io/badge/license-Apache%202.0-green.svg)](LICENSE)
[![Architecture](https://img.shields.io/badge/architecture-decoupled%20control%2Fdata%20plane-orange.svg)](#architecture-overview)

The **`rag-prompt-registry-service`** is a high-throughput, low-latency microservice and embedded SDK in Go that acts as the single source of truth for prompt template lifecycles, runtime context binding, model-specific chat-markup adaptation, and output schema contracts within a production Retrieval-Augmented Generation (RAG) platform.

---

## Architecture Overview

The system strictly decouples administrative lifecycle governance (**Control Plane**) from high-frequency runtime compilation (**Data Plane**):

```
CONTROL PLANE (rag-prompt-registry-service)               DATA PLANE (In-Process on Orchestrator)
┌──────────────────────────────────────────────┐          ┌────────────────────────────────────────────────────────┐
│ rag-prompt-registry-service                  │          │ rag-search-orchestrator                                │
│                                              │          │                                                        │
│ • Immutable Template Engine (v1..vn)         │          │ 1. Vector Retrieval & Context Assembly (50 KB)         │
│ • Mutable Tag Pointer Store (@prod -> v2)    │          │                                                        │
│ • JSON Schema Registry                       │          │ 2. go-prompt-sdk (Embedded Library):                   │
│ • Invalidation Broadcaster (Redis Pub/Sub)   │          │    • In-Memory Local Cache (Zero Network Serialization)│
│ • Database Audit Ledger (Direct PostgreSQL)  │          │    • Dynamic sync.Pool Variable Interpolation          │
└──────────────────────┬───────────────────────┘          │    • Model Tokenizer-Aware Frame Truncation            │
                       │ Synchronize AST / Schema         │    • Target Dialect AST Adaptation (Anthropic/Llama)   │
                       ▼ (Low-Frequency Ingress)          │                                                        │
┌──────────────────────────────────────────────┐          │ 3. Forward Formatted Prompt Payload to LLM Inference   │
│ Redis 7 Cluster (L2 Cache + Invalidation Bus)│─────────►│                                                        │
└──────────────────────────────────────────────┘          └────────────────────────────────────────────────────────┘
```

- **Control Plane (`cmd/prompt-registry-server`)**: Centralized Go daemon managing immutable template versions ($v_1 \dots v_n$), JSON output schemas, and optimistic pointer promotions (`@production`, `@staging`, `@canary`). Writes configuration and audit entries atomically to PostgreSQL 16+ (via PgBouncer in Simple Protocol mode) and broadcasts cache invalidations to Redis 7.2.
- **Data Plane (`pkg/promptsdk`)**: Embedded Go client SDK linked directly into calling orchestrators (`rag-search-orchestrator`). Caches compiled ASTs in local process memory (L1), listens to Redis Pub/Sub for sub-second invalidations, and binds 50 KB context chunks into vendor dialects in-process using buffer pooling (`sync.Pool`) with **0 heap allocations on the hot path**.

Detailed architectural documentation is available in [ARCHITECTURE.md](ARCHITECTURE.md) and [.ai/architecture.md](.ai/architecture.md).

---

## Key Features

1. **Strict Append-Only Versioning ($v_1 \dots v_n$)**: All prompt templates are immutable once published. Database triggers block in-place `UPDATE` or `DELETE` operations on versions and audit logs.
2. **Optimistic Pointer Promotion**: Mutable tags (`@production`, `@staging`, `@canary`) point to immutable versions. Pointer promotions enforce `expected_version` preconditions to prevent race conditions.
3. **Zero-Allocation In-Process Compilation (ADR-001)**: Calling orchestrators interpolate large context payloads (50 KB) in-process, eliminating gRPC network hops. Inner escaping uses AVX2/SSE2 SIMD (`IndexByte`) and 8-byte SWAR bit manipulation.
4. **Instant Distributed Cache Coherence (ADR-002)**: Pointer promotions broadcast eviction events over Redis Pub/Sub (`channel:prompt:invalidation`), invalidating all distributed in-memory SDK caches globally within $<500$ ms ($<100$ ms typical).
5. **Multi-Dialect AST Adaptation**:
   - `DIALECT_ANTHROPIC_XML`: Idempotent XML tags (`<instructions>`, `<context>`, `<query>`) with zero-alloc XML escaping.
   - `DIALECT_LLAMA_SPECIAL`: Meta Llama-3 special header sequences (`<|begin_of_text|>`, `<|start_header_id|>`) with token neutralization to prevent role-spoofing injection attacks.
   - `DIALECT_OPENAI_STANDARD`: RFC 8259 compliant JSON string escaping for standard OpenAI/Mistral message envelopes.
6. **Tokenizer-Aware Budget Safety Guard**: Calculates the structural byte and token overhead of vendor framing before context injection, guaranteeing prompts never exceed downstream model context windows.
7. **Transactional Internal Audit Ledger (ADR-004)**: Atomically logs all mutations within the exact same PostgreSQL transaction, eliminating the need for external Debezium/Kafka CDC pipelines.
8. **PgBouncer Simple Protocol Mode (ADR-003)**: Uses `jackc/pgx/v5` with `QueryExecModeSimpleProtocol` to support connection pooling across PgBouncer transaction pooling without prepared-statement collisions.

---

## Calibrated Service Level Objectives (SLOs)

| Metric | Target SLO | Observed Benchmark / Test Result |
| :--- | :--- | :--- |
| **SDK Render Latency (50 KB Context)** | $P99 \le 0.5$ ms | **$P99 = 0.20$–$0.23$ ms** ($P50 \approx 73$ µs) |
| **SDK Throughput** | $\ge 10,000$ RPS | **$36,000$–$39,700$ RPS** (sustained multi-core) |
| **SDK Hot-Path Memory Allocations** | `0 allocs/op` | **`0 B/op`, `0 allocs/op`** |
| **Control Plane Registry Read Latency** | $P99 \le 2.0$ ms | **$P99 \le 1.2$ ms** |
| **Global Cache Invalidation Propagation** | $< 500$ ms | **$< 100$ ms** over Redis Pub/Sub |
| **Process Memory Footprint** | $\le 128$ MiB RSS | **$\approx 54.5$ MiB peak RSS** |
| **Data Plane Availability During Outage** | 100% resilient | **100% success** (stale-while-offline local L1 cache) |

---

## Quick Start

### 1. Prerequisites

- **Go**: 1.26.9+ (64-bit)
- **PostgreSQL**: 16+ (or PgBouncer 1.21+ in transaction pooling mode)
- **Redis**: 7.2+ (Cluster or Standalone)
- **Python**: 3.10+ (for configuration sync validation scripts)

### 2. Environment Configuration

The service implements 12-factor configuration loading:

- Copy `.env.example` to `.env` for local development:
  ```bash
  cp .env.example .env
  ```
- **Local Dev vs Production**:
  - `.env` is ignored by git and used exclusively for local runs (defaulting to `ALLOW_IN_MEMORY_STORE=true` or local ports).
  - `.env.prod` is the baseline production template. Changes to `.env.prod` sync into Helm `values.yaml` via:
    ```bash
    python scripts/sync_env_to_deployments.py --sync
    ```
  - To load a specific file at runtime:
    ```bash
    ENV_FILE=.env.staging ./prompt-registry-server
    ```

### 3. Start Local Dependencies

Run Redis locally using Docker Compose:
```bash
docker compose -f deployments/docker/docker-compose.redis.yml up -d
```

### 4. Build and Run the Control Plane

```bash
# Build server binary
go build -o bin/prompt-registry-server cmd/prompt-registry-server/main.go

# Run server (binds HTTP on :8080 and gRPC on :9090)
./bin/prompt-registry-server
```

---

## Management APIs

### HTTP Endpoints (`:8080`)

| Endpoint | Method | Description |
| :--- | :--- | :--- |
| `/v1/templates` | `POST` | Create a new template aggregate with initial immutable version $v_1$ |
| `/v1/templates` | `GET` | List templates with cursor pagination |
| `/v1/templates/{id}/versions` | `POST` | Publish immutable version $v_{n+1}$ (idempotent on matching AST hash) |
| `/v1/templates/{id}/versions/{v}` | `GET` | Fetch specific immutable version AST and output schema |
| `/v1/templates/{id}/tags/{tag}` | `PUT` | Atomically promote semantic pointer tag (`expected_version` guard) |
| `/v1/templates/{id}/tags/{tag}` | `GET` | Resolve current target version and AST for a semantic pointer tag |
| `/v1/schemas` | `POST` | Register JSON output schema contracts |
| `/v1/schemas/{id}` | `GET` | Retrieve JSON output schema definition |
| `/healthz` | `GET` | Liveness probe |
| `/ready` | `GET` | Readiness probe (verifies PostgreSQL pool and Redis cluster) |
| `/metrics` | `GET` | Prometheus metrics exposition |
| `/docs` | `GET` | Interactive Swagger UI (OpenAPI 3.1) |
| `/redoc` | `GET` | Interactive ReDoc UI |
| `/openapi.yaml` | `GET` | Machine-readable OpenAPI 3.1 specification |

### gRPC Service (`:9090`)

Protobuf contracts are defined in `proto/v1/prompt_service.proto`:
- `CreateTemplate`
- `PublishVersion`
- `GetTemplateVersion`
- `PromoteTag`
- `ResolveTag`
- `ListTemplates`
- `RegisterSchema`
- `GetSchema`

---

## Embedded Go SDK Usage (`pkg/promptsdk`)

Calling orchestrators import `pkg/promptsdk` directly for zero-allocation in-process rendering:

```go
package main

import (
	"context"
	"fmt"
	"log"

	"rag-prompt-registry-service/pkg/promptsdk"
)

func main() {
	ctx := context.Background()

	// 1. Initialize client connecting to Control Plane gRPC & Redis Pub/Sub
	client, err := promptsdk.NewClient(promptsdk.ClientConfig{
		ControlPlaneAddr: "rag-prompt-registry-service.rag-platform.svc.cluster.local:9090",
		RedisAddrs:       []string{"redis-cluster:6379"},
		TenantID:         "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11",
		HMACSecret:       "production-shared-secret",
	})
	if err != nil {
		log.Fatalf("failed to initialize promptsdk: %v", err)
	}
	defer client.Close()

	// 2. Acquire a reusable buffer from global sync.Pool
	buf := promptsdk.AcquireBuffer()
	defer promptsdk.ReleaseBuffer(buf)

	// 3. Compile prompt into target model dialect (0 heap allocations)
	opts := promptsdk.RenderOptions{
		TargetDialect: promptsdk.DialectAnthropicXML,
		Variables: map[string]string{
			"context": "Retrieved financial passage chunks...",
			"query":   "Summarize Q3 earnings EBITDA",
		},
	}

	err = client.RenderToBuffer(ctx, buf, "financial-qa", "@production", opts)
	if err != nil {
		log.Fatalf("render failed: %v", err)
	}

	fmt.Println(buf.String())
}
```

---

## Testing & Benchmarks

Run the complete test suite:

```bash
# Unit tests
go test -v ./cmd/... ./test/unit/...

# SDK benchmarks and sustained load tests
go test -v ./test/sdk/...

# Chaos and outage integration tests
go test -v ./test/integration/...

# Zero-allocation microbenchmarks
go test -bench=. -benchmem ./test/sdk/...
```

---

## Deployment & Production Hardening

- **Kubernetes / Helm**: Ready-to-use Helm chart located in [deployments/helm/rag-prompt-registry](deployments/helm/rag-prompt-registry).
- **Argo Rollouts & Canary**: Automated canary promotion with Prometheus analysis metrics in [deployments/helm/rag-prompt-registry/templates/analysistemplate.yaml](deployments/helm/rag-prompt-registry/templates/analysistemplate.yaml).
- **Redis Cluster**: Production Redis 7.2 Cluster deployment scripts in [scripts/deploy_redis_cluster.ps1](scripts/deploy_redis_cluster.ps1) and [scripts/deploy_redis_cluster.sh](scripts/deploy_redis_cluster.sh).
- **Argo CD Application**: GitOps specification in [deployments/gitops/argocd-application.yaml](deployments/gitops/argocd-application.yaml).
- **Secret Management**: Detailed procedures for namespace pre-provisioning, secret rotation, and PostgreSQL/Redis credentials in [deployments/SECRET_MANAGEMENT.md](deployments/SECRET_MANAGEMENT.md).

---

## Security & Observability

- **Secured Logging Invariant**: Structured JSON logging via `log/slog`. Raw prompts, customer secrets, context chunks, and credentials are strictly redacted.
- **Prometheus Metrics**:
  - `prompt_sdk_render_duration_seconds_bucket`: Cumulative latency histogram with `le` buckets.
  - `prompt_sdk_cache_hit_ratio`: L1 memory cache hit ratio (`tier="L1_version"` and `tier="L1_tag"`).
  - `prompt_sdk_pubsub_connected`: Active Redis Pub/Sub invalidation connection health.
- **Distroless Container**: Hardened non-root image based on `gcr.io/distroless/static-debian12:nonroot`.

