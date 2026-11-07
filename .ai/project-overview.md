# RAG Prompt Registry Service - Project Overview

## Overview

The **`rag-prompt-registry-service`** is a high-throughput, low-latency vertical domain microservice implemented in **Go** that serves as the single source of truth for prompt template lifecycles, runtime context binding, model-specific chat-markup adaptation, and output schema contracts within the disaggregated RAG platform.

The system explicitly decouples administrative lifecycle governance (Control Plane) from runtime execution (Data Plane):
1. **Control Plane (`rag-prompt-registry-service`)**:
   - Centralized administrative daemon (`cmd/prompt-registry-server/main.go`) managing immutable template versions ($v_1 \dots v_n$), JSON output schemas, and persona overrides.
   - Optimistic tag pointer management (`@production`, `@staging`, `@canary`) with atomic updates.
   - Redis 7.2 Pub/Sub broadcast engine pushing pointer evictions to downstream SDKs immediately.
   - Relational transactional audit ledger recording all configuration changes in PostgreSQL 16+ without external CDC dependencies.
2. **Data Plane (`go-prompt-sdk` embedded library in `pkg/promptsdk`)**:
   - Embedded Go module imported directly by calling orchestrators (`rag-search-orchestrator`).
   - Thread-safe, read-through local template memory cache backed by background invalidation listeners.
   - Zero-allocation variable interpolation engine using byte buffer pools (`sync.Pool`).
   - Dialect transformers (Anthropic XML, Meta Llama-3 special tokens, OpenAI/Mistral roles and JSON schemas).
   - Tokenizer-aware context framing to guard prompt budgets against markup overhead before injecting context.

```
CONTROL PLANE (rag-prompt-registry-service)               DATA PLANE (In-Process on Orchestrator)
┌──────────────────────────────────────────────┐          ┌────────────────────────────────────────────────────────┐
│ rag-prompt-registry-service                  │          │ rag-search-orchestrator                                │
│                                              │          │                                                        │
│ • Immutable Template Engine (v1..vn)         │          │ 1. Vector Retrieval & Context Assembly (50 KB)         │
│ • Mutable Tag Pointer Store (@prod -> v2)    │          │                                                        │
│ • JSON Schema Registry                       │          │ 2. go-prompt-sdk (Embedded Library):                   │
│ • Invalidation Broadcaster (Redis Pub/Sub)   │          │    • In-Memory Local Cache (Zero Network Serialization)│
│ • Database Audit Ledger (Direct Write)       │          │    • Dynamic sync.Pool Variable Interpolation          │
└──────────────────────┬───────────────────────┘          │    • Model Tokenizer-Aware Frame Truncation            │
                       │ Synchronize AST / Schema         │    • Target Dialect AST Adaptation                     │
                       ▼ (Low-Frequency Ingress)          │                                                        │
┌──────────────────────────────────────────────┐          │ 3. Forward Formatted Prompt Payload to Inference       │
│ Redis 7 Cluster (L2 Cache + Invalidation Bus)│─────────►│                                                        │
└──────────────────────────────────────────────┘          └────────────────────────────────────────────────────────┘
```

---

## Strategic Objectives

1. **Immutable Template Lineage**: Enforce strict append-only versioning ($v_1 \dots v_n$) across prompt templates in PostgreSQL, guaranteeing deterministic evaluation and auditability.
2. **Decoupled Control vs. Data Plane (ADR-001)**: Centralize administrative lifecycle governance (versioning, RBAC, schema registry) within `rag-prompt-registry-service`, while delegating runtime variable binding to an embedded Go SDK (`go-prompt-sdk`).
3. **Zero-Serialization In-Process Compilation**: Eliminate network hops for large context payloads (50 KB parent containers) by rendering strings in-process within calling orchestrators via pooled buffers (`sync.Pool`).
4. **Deterministic Multi-Dialect Adaptation**: Translate structured AST prompts into vendor-specific chat markup (Anthropic XML, Meta Llama-3 special headers, OpenAI JSON schema mode) directly in the client runtime.
5. **Strict Output Contracts**: Bind every prompt version to a registered JSON schema (`schema_id` is required in `CreateTemplateRequest` and `PublishVersionRequest` across both REST and gRPC and must resolve to a registered schema) to govern downstream structured inference generation.
6. **Instant Multi-Instance Cache Coherence (ADR-002)**: Broadcast tag pointer mutations (`@production`) over Redis Pub/Sub (`channel:prompt:invalidation`) to invalidate all distributed in-memory SDK caches within $<100$ms.
7. **Continuous Documentation & Contract Synchronization**: Ensure internal OpenAPI 3.1 specifications (`openapi/openapi.yaml`, `/docs`, `/api/docs`) and `.ai/doc-api.md` are continuously synchronized with every code change.

---

## Quantitative Success Criteria (SLIs & SLOs)

- **In-Process SDK Render Latency**: $P99 \le 0.5$ms to bind 50 KB context payloads and compile target dialects using zero heap allocations on the hot path.
- **Control Plane Registry Read Latency**: $P99 \le 2$ms when fetching raw AST templates directly from Redis Cluster or PostgreSQL.
- **Global Tag Propagation Latency**: Full invalidation and cache synchronization across all orchestrator instances in $<500$ms following an atomic pointer switch.
- **Control Plane Pod Footprint**: Flat memory usage $\le 128$ MiB RSS; CPU requests 100m, limits 500m per pod (handling administrative traffic only).
- **Availability**: 99.99% availability for the registry control plane; 100% resilient serving during registry downtime (SDK runs gracefully from local memory).

---

## Scope & Exclusions

### In-Scope
- **Control Plane Registry Service (`rag-prompt-registry-service`)**:
  - Administrative CRUD plane managing template versions, JSON output schemas, and persona overrides.
  - Optimistic pointer management (`@production`, `@staging`, `@canary`) with atomic updates.
  - Redis Pub/Sub broadcast engine pushing pointer evictions to downstream SDKs immediately.
  - Relational transactional audit ledger recording all configuration changes in PostgreSQL without external CDC dependencies.
  - PgBouncer integration verifying Simple Protocol Mode via `jackc/pgx/v5`.
  - HTTP REST endpoints for administration, OpenAPI 3.1 docs (`/docs`, `/redoc`, `/api/docs`), `/healthz`, `/ready`, and Prometheus `/metrics`.
  - gRPC transport exposing `proto/v1/prompt_service.proto`.
- **Data Plane Client SDK (`go-prompt-sdk` in `pkg/promptsdk`)**:
  - Embedded Go module used by `rag-search-orchestrator`.
  - Thread-safe, read-through local template memory cache backed by background invalidation listeners.
  - Zero-allocation variable interpolation engine using byte buffer pools (`sync.Pool`).
  - Dialect transformers (Anthropic XML, Meta Llama-3 special tokens, OpenAI roles).
  - Tokenizer-aware context framing to guard prompt budgets against markup overhead.

### Exclusions (Strictly Out-of-Scope)
- **Network Payload Interpolation**: Transporting raw context blocks over gRPC for string concatenation is strictly forbidden.
- **Heavy CDC Pipelines**: External Kafka Connect / Debezium stacks are removed from the prompt domain; operational volume does not warrant CDC infrastructure.
- **Direct Model Execution**: Network egress to LLM providers remains isolated to the API Gateway and inference serving pools.
- **Downstream Output Validation**: Verifying model-generated output strings against registered schemas is handled downstream by the API Gateway or worker runtimes.
- **Vector Search & RRF Ranking**: Query routing and reciprocal rank fusion remain strictly encapsulated in `rag-search-orchestrator`.
