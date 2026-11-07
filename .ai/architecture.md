# Architecture Context - RAG Prompt Registry Service

## System Stack & Specifications

| Layer | Technology | Role & Operational Characteristics |
| :--- | :--- | :--- |
| **Language Runtime** | Go 1.22+ (64-bit) | High-concurrency network I/O, minimal memory footprint, zero GC overhead on hot path |
| **Control Plane Server** | Go Daemon (`cmd/prompt-registry-server/main.go`) | Administrative lifecycle governance, optimistic tag promotion, and schema registry |
| **Data Plane SDK** | Embedded Go Library (`pkg/promptsdk`) | In-process string interpolation, dialect transformation, and local caching in calling orchestrators |
| **Relational Storage** | PostgreSQL 16+ via PgBouncer | Immutable template versioning ($v_1 \dots v_n$), tag pointer store, and transactional audit ledger |
| **Database Driver** | `jackc/pgx/v5` with `PreferSimpleProtocol` | Transaction pooling compatibility preventing prepared statement lookup errors |
| **Cache & Bus** | Redis 7.2 Cluster | L2 distributed template cache & Pub/Sub invalidation bus (`channel:prompt:invalidation`) |
| **Buffer Allocator** | `sync.Pool` | Reusable byte buffers achieving zero heap allocations during 50 KB context interpolation |
| **Management APIs** | HTTP/REST + gRPC / Protobuf v3 | Low-frequency admin mutations, OpenAPI 3.1 contracts, health probes, and metrics |
| **Observability** | Prometheus + OpenTelemetry | Histograms (`prompt_sdk_render_duration_seconds`), cache hit gauges, and structured JSON logs |
| **Deployment Target** | Kubernetes (Distroless Docker Container) | 2 pod replicas, requests 100m CPU / 64MiB RAM, limits 500m CPU / 128MiB RAM |

---

## Production-Grade 5-Layer Backend Architecture

Aligned with [Production Grade Backend Concepts.pdf](tasks/Production%20Grade%20Backend%20Concepts.pdf):

### Layer 1: API & Routing
- **Entry Points (`cmd/prompt-registry-server/main.go`)**:
  - Initializes runtime environment, parses 12-factor configuration, establishes PostgreSQL connection pool via `jackc/pgx/v5`, bootstraps Redis 7.2 client, and binds both gRPC and HTTP listeners.
  - Concurrently serves the internal administration HTTP server on port `8080` exposing `/docs`, `/redoc`, `/api/docs`, `/openapi.yaml`, `/healthz`, `/ready`, and `/metrics`.
  - Concurrently serves the gRPC management server on port `50051` exposing `proto/v1/prompt_service.proto`.
- **Endpoints & Routing**:
  - `POST /v1/templates`: Create template aggregate and publish initial immutable version $v_1$.
  - `POST /v1/templates/{id}/versions`: Publish new immutable template version $v_{n+1}$.
  - `GET /v1/templates/{id}/versions/{version}`: Fetch canonical AST representation for specific version.
  - `PUT /v1/templates/{id}/tags/{tag}`: Atomically promote semantic pointer tag (e.g. `@production -> v2`) and trigger Redis Pub/Sub invalidation.
  - `GET /v1/templates/{id}/tags/{tag}`: Resolve current target version of a semantic tag pointer.
  - `POST /v1/schemas`: Register JSON output schema contracts.
  - `GET /docs`: Interactive Swagger UI powered by OpenAPI 3.1.
  - `GET /redoc`: Interactive ReDoc UI.
  - `GET /api/docs` & `GET /openapi.yaml`: Machine-readable OpenAPI 3.1 specification.
  - `GET /healthz`: Liveness probe verifying server process responsiveness.
  - `GET /ready`: Readiness probe verifying PostgreSQL pool connectivity and Redis cluster reachability.
  - `GET /metrics`: Prometheus metrics scrape endpoint.
- **Middleware & Interception**:
  - Structured request tracing middleware injecting correlated `trace_id` and `request_id` (ULID) headers.
  - Tenant extraction middleware verifying caller tenancy (`tenant_id`).
  - Panic recovery middleware cleanly catching panics, sanitizing error details, and preventing process crashes.
- **Input Validation & Sanitization**:
  - Strict JSON schema and Protobuf AST parsing ensuring template syntax, variable markers `{{variable}}`, and JSON schema drafts conform to specification before reaching storage.
- **Rate Limiting & Throttling**:
  - Token-bucket rate limiter guarding administrative write operations to protect the relational database from mutation storms.
- **Idempotency**:
  - Immutable version publishing is idempotent: attempting to publish an identical template version digest returns the existing version without duplication.
  - Semantic tag pointer mutations use optimistic locking with version comparison.

### Layer 2: Data Persistence & State
- **Database Migrations & Immutability**:
  - Version-controlled SQL migration files in `internal/infrastructure/persistence/postgres/migrations/`.
  - Tables: `prompt_templates`, `prompt_versions` (append-only, strict monotonic versions $v_1 \dots v_n$), `prompt_tags` (mutable pointers `@production`, `@staging`, `@canary`), `prompt_audit_ledger` (immutable compliance log), and `json_schemas`.
- **Database Transactions (ACID) & Internal Audit Ledger (ADR-004)**:
  - All template mutations and corresponding `prompt_audit_ledger` records are written atomically within the same PostgreSQL transaction.
  - Deprecates external Kafka Connect / Debezium CDC pipelines for the prompt registry domain.
- **Connection Pooling & PgBouncer Compatibility (ADR-003)**:
  - Connection pooling via `jackc/pgx/v5` pool.
  - When communicating through PgBouncer in transaction pooling mode, the pool must be initialized with `PreferSimpleProtocol: true` to avoid prepared statement caching collisions across pooled connections.
- **Caching Mechanics (L1 & L2)**:
  - **L1 Cache (In-Process on SDK)**: High-speed, read-through concurrent hash map residing in caller memory (`pkg/promptsdk/cache.go`). Keyed by immutable version identifiers (`template:{id}:v{version}`) with unbounded TTL, and mutable tag pointers (`template:{id}:tag:{tag}`) with 5-minute safety fallback TTL.
  - **L2 Cache (Distributed)**: Redis 7.2 Cluster caching canonical serialized AST definitions and JSON schemas.
- **Pagination & Query Limits**:
  - Administrative template and version list endpoints enforce strict cursor-based pagination with upper bounds (`limit <= 100`) to prevent OOM errors.
- **Resource Identification Strategies**:
  - High-entropy UUIDv4/ULIDs for template and schema identifiers combined with human-readable semantic tags (`@production`, `@staging`, `@canary`).

### Layer 3: Architecture & Scale
- **Decoupled Control vs. Data Plane (ADR-001)**:
  - String interpolation, dialect transformation, and context binding run purely in-process on calling orchestrators via `go-prompt-sdk` (`pkg/promptsdk`), eliminating gRPC payload serialization for 50 KB context chunks.
- **Asynchronous Cache Invalidation Bus (ADR-002)**:
  - Control plane publishes tag pointer mutations to Redis Pub/Sub (`channel:prompt:invalidation`).
  - All running instances of `go-prompt-sdk` subscribe to this channel, evicting stale pointer entries in $<100$ms ($<500$ms global SLO).
- **High Availability & Redundancy**:
  - Control plane deployed with 2 replicas behind Kubernetes Service with `PodDisruptionBudget` (minAvailable: 1).
  - Resilient Data Plane: Calling orchestrators continue rendering cached templates in memory even if the control plane database or registry pods are completely offline.
- **Creational Design Patterns & Buffer Pooling**:
  - Singletons for Redis clients and database connection pools.
  - `sync.Pool` byte buffer reuse in `pkg/promptsdk/compiler.go` yielding 0 heap escapes and $P99 \le 0.5$ms latency during 10,000 renders/sec.

### Layer 4: Security & Identity
- **Authentication & Authorization (AuthN vs AuthZ)**:
  - Tenant boundary isolation: All template aggregates, versions, and tags are partitioned by `tenant_id`.
  - Administrative operations require verified service-to-service mTLS or bearer JWT tokens with explicit role claims (`prompt:write`, `prompt:publish`, `tag:promote`).
- **Configuration & Secrets Management**:
  - 12-Factor App design: Database passwords, Redis credentials, and service ports are injected strictly through environment variables.
  - Zero plaintext secrets in code or logs.
- **Secured Logging Invariant**:
  - All logging is structured JSON.
  - **Strict Redaction**: Never log raw prompt templates, raw context passages, customer secrets, authorization headers, or database credentials.
  - Emit only metadata: `tenant_id`, `template_id`, `version`, `tag`, `latency_ms`, and truncated SHA-256 digests.
- **Container Hardening**:
  - Distroless multi-stage Docker build (`gcr.io/distroless/static-debian12:nonroot`).
  - Runs as non-root UID 65532, read-only root filesystem, dropped Linux capabilities.

### Layer 5: Observability & Quality
- **Structured JSON Logging**: Standardized key-value structured logging using Go `log/slog`.
- **Prometheus Observability Matrix**:
  - `prompt_sdk_render_duration_seconds`: Histogram tracking in-process SDK compilation latency ($P99 \le 0.5$ms).
  - `prompt_registry_mutation_duration_seconds`: Histogram tracking control plane write latencies.
  - `prompt_sdk_cache_hit_ratio`: Gauge tracking local SDK memory hits (Target $>99.9\%$).
  - `prompt_invalidation_events_published_total`: Counter tracking Redis Pub/Sub eviction signals.
- **Production Alert Rules**:
  - *SDK High Render Latency*: Alerts if SDK in-process rendering P99 exceeds 2ms over 5m window.
  - *Pub/Sub Invalidation Failure*: Alerts if the registry fails to publish pointer mutations to Redis.
  - *Elevated Pointer Rollbacks*: Alerts if tag promotions are rolled back within 15 minutes of release.
- **Health Probes & Graceful Shutdown**:
  - `/healthz` and `/ready` endpoints.
  - Graceful shutdown handles `SIGINT`/`SIGTERM`, flushing pending Redis publish buffers, closing gRPC listeners, and draining active database connections within 15 seconds.

---

## Monorepo Directory Boundaries

```
rag-prompt-registry-service/
├── cmd/
│   └── prompt-registry-server/       # Control plane administration daemon
│       └── main.go
├── proto/v1/
│   ├── prompt_service.proto          # Template CRUD and pointer management RPCs
│   └── types.proto                   # Structured template AST, dialects, and schema contracts
├── openapi/
│   └── openapi.yaml                  # Management plane REST specifications
├── pkg/
│   └── promptsdk/                    # Reusable embedded Go SDK (Data Plane)
│       ├── client.go                 # Registry gRPC client with fallback caching
│       ├── cache.go                  # Thread-safe in-memory cache with Redis Pub/Sub eviction
│       ├── compiler.go               # sync.Pool string interpolation engine
│       ├── dialects/
│       │   ├── anthropic.go          # XML structural wrapper (<context>, <instructions>)
│       │   ├── llama.go              # Meta special header tokens (<|start_header_id|>)
│       │   └── standard.go           # OpenAI/Mistral JSON schema builder
│       └── tokenizer/
│           └── budget_guard.go       # Token-aware context boundary calculations
├── internal/
│   ├── domain/                       # Control plane business logic
│   │   ├── entity/
│   │   │   ├── prompt_template.go    # Append-only version aggregate root
│   │   │   ├── prompt_tag.go         # Pointer mutation aggregate (@production)
│   │   │   └── audit_entry.go        # Compliance ledger entry
│   │   └── repository/
│   │       ├── prompt_repository.go
│   │       └── audit_repository.go
│   ├── application/
│   │   ├── command/
│   │   │   ├── publish_version.go    # Creates immutable v_{n+1}
│   │   │   └── promote_tag.go        # Updates pointer and triggers Redis Pub/Sub
│   │   └── query/
│   │       ├── get_template_ast.go   # Returns canonical template representation
│   │       └── list_templates.go     # Control plane discovery
│   ├── infrastructure/
│   │   ├── persistence/
│   │   │   ├── postgres/
│   │   │   │   ├── pool.go           # pgx/v5 PreferSimpleProtocol configuration
│   │   │   │   ├── prompt_repo.go    # Monotonic version and pointer queries
│   │   │   │   └── audit_repo.go     # Direct relational audit ledger writer
│   │   │   └── migrations/
│   │   └── cache/
│   │       └── redis/
│   │           ├── cluster_client.go # Redis 7 client
│   │           └── invalidation_pub.go# Pub/Sub broadcaster for tag mutations
│   └── transport/
│       ├── grpc/
│       │   └── prompt_admin_server.go# Low-volume control plane gRPC server
│       └── http/
│           └── router.go             # Internal admin UI, OpenAPI docs, and probe gateway
├── deployments/
│   ├── helm/
│   └── docker/
│       └── Dockerfile                # Minimal multi-stage distroless build
├── test/
│   ├── unit/                         # Compiler, dialect formatting, and parser tests
│   ├── integration/                  # Testcontainers suites (Postgres + Redis)
│   └── sdk/                          # Concurrent cache eviction and rendering benchmarks
├── Makefile
├── go.mod
└── go.sum
```

---

## Architectural Decision Records (ADRs)

- **ADR-001: Separation of Registry Control Plane and SDK Data Plane**: The registry service exclusively manages metadata, versioning, and schemas over low-volume APIs. String interpolation and dialect compilation run in-process within calling services via `go-prompt-sdk` to eliminate network serialization bottlenecks.
- **ADR-002: Invalidation via Redis Pub/Sub**: Eliminates static TTL split-brain windows by broadcasting tag pointer updates (`@production`) to all distributed SDK caches over Redis Pub/Sub (`channel:prompt:invalidation`).
- **ADR-003: Simple Protocol Mode for PgBouncer**: Configures the PostgreSQL connection pool (`jackc/pgx/v5`) to use Simple Protocol Mode (`PreferSimpleProtocol: true`), avoiding prepared statement failures in transaction pooling mode.
- **ADR-004: Lightweight Audit Ledger**: Administrative changes write directly to an internal `prompt_audit_ledger` table within the same transaction, deprecating external Debezium/Kafka CDC requirements for this service.

---

## Operational Runbooks

### Runbook 1: Immediate Pointer Rollback
1. Identify application regressions triggered by a new prompt version.
2. Run the pointer mutation command via CLI or admin endpoint:
   `PUT /v1/templates/{name}/tags/@production` with payload: `{"version": "v_{previous}"}`.
3. The service updates PostgreSQL and emits an invalidation event to Redis Pub/Sub.
4. Confirm via Prometheus that orchestrator nodes pick up the target version within $<500$ms.

### Runbook 2: Resolving Pub/Sub Invalidation Drops
1. If an orchestrator instance loses its Redis Pub/Sub connection, it falls back to the 5-minute safety TTL on mutable pointer tags.
2. Check the `prompt_sdk_pubsub_connected` gauge on the orchestrator.
3. If disconnected, restart the client connection pool or force a local cache wipe via the orchestrator's diagnostic endpoint.
