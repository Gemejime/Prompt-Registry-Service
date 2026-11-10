# Implementation Plan: Phase 3 — Cache Invalidation & Orchestrator Wiring

## 1. Objective & Scope

Implement **Phase 3: Cache Invalidation & Orchestrator Wiring (Weeks 5–6)** of the `rag-prompt-registry-service` in strict compliance with `GEMINI.md`, `.ai/` system specifications (`project-overview.md`, `architecture.md`, `doc-api.md`, `code-standards.md`, `security.md`, `ai-workflow-rules.md`), and the reference blueprints in `tasks/` (`RAG Prompt Registry Service Plan.pdf`, `Production Grade Backend Concepts.pdf`, `Production-Grade Multimodal RAG Architectural Specification.pdf`, and `Go and Python Microservices Architecture.pdf`):

### 1. Multi-Instance Cache Invalidation Bus via Redis Pub/Sub (`channel:prompt:invalidation`, ADR-002)
- **Multi-Node Invalidation Topology**:
  - Connect multiple concurrent instances of `go-prompt-sdk` (`pkg/promptsdk`) to the Redis Pub/Sub invalidation bus on `channel:prompt:invalidation`.
  - When a pointer mutation (`@production`, `@staging`, `@canary`) or rollback occurs on the Control Plane, an `InvalidationEvent` payload (`event_id`, `tenant_id`, `template_id`, `tag`, `new_version`, `timestamp`) is broadcast.
  - Subscribing SDK instances immediately evict matching local `L1_tag` cache entries (sub-millisecond local eviction, $<500$ms global cluster propagation SLO from the atomic pointer switch).
- **Tenant Boundary Isolation (Layer 4)**:
  - Enforce tenant isolation in `L1Cache.HandleInvalidationPayload`: if the client has a configured `TenantID` and `evt.TenantID != ""`, events targeting different tenants are safely ignored without evicting local cache keys.
- **Connection Lifecycle, Resilience & Auto-Reconnect with Exponential Backoff (Runbook 2)**:
  - Harden Redis Pub/Sub subscription handling in `pkg/promptsdk/client.go` with automated reconnection and exponential backoff with jitter (initial backoff: 100ms, factor: 2.0, max backoff: 3s, jitter: $\pm 20\%$).
  - Reset backoff curve upon receiving the Redis `SUBSCRIBE` confirmation.
  - Dynamically maintain the Prometheus gauge `prompt_sdk_pubsub_connected` (value `1` when active, `0` when disconnected or reconnecting).
  - During Pub/Sub disconnection, enforce the 5-minute safety fallback TTL on mutable pointer tags (`L1_tag`) without dropping in-memory availability.
- **Forced Cache Wipe & Diagnostic API (Runbook 2 Step 3)**:
  - Implement `Client.Purge()` and `L1Cache.PurgeTags()` to allow diagnostic endpoints and operational runbooks to force a complete local mutable tag cache wipe.
- **Publication Telemetry**:
  - Ensure control plane tag promotions increment `prompt_invalidation_events_published_total{status="success"|"error"}` and report publication status in `PromoteTagResponse.broadcast_emitted`.

### 2. Embedded SDK Integration into `rag-search-orchestrator` Pipeline (`pkg/orchestrator`)
- **Decoupled Data Plane Topology (ADR-001 & PDF Pages 1, 2, 8)**:
  - Implement the `pkg/orchestrator` package fulfilling the Data Plane orchestration architecture.
  - Expose clean decoupled interfaces:
    1. `ContextRetriever`: retrieves parent document containers and metadata for a user query.
    2. `InferenceDispatcher`: forwards formatted model prompts to downstream model runtimes (Triton/vLLM/LLM Gateway), isolating network model invocation outside the prompt domain.
    3. `RAGPipeline`: coordinates the 4-stage pipeline:
       - **Stage 1: Context Assembly**: Assembles large context containers (50 KB parent document chunks) and conversational variables.
       - **Stage 2: In-Process Compilation (`go-prompt-sdk`)**: Directly invokes embedded `PromptSDK.RenderToBuffer` with `sync.Pool` byte buffers without network hops or gRPC context serialization.
       - **Stage 3: Tokenizer Budget Guard & Boundary Truncation**: Binds target dialect (Anthropic XML, Meta Llama-3, OpenAI Standard JSON) and truncates context on valid UTF-8/token boundaries to fit model context ceilings.
       - **Stage 4: Inference Forwarding**: Dispatches prompt payload to `InferenceDispatcher`.
- **Dual-Mode Execution (`Production` vs `Diagnostic`)**:
  - **Production Mode**: Returns compiled prompt and inference response with minimal overhead.
  - **Diagnostic Mode**: Returns detailed waterfall timings (`assembly_ms`, `sdk_render_ms`, `inference_ms`), token budget breakdown, and chunk provenance metadata.
- **Zero-Allocation Memory Profile**:
  - The pipeline's rendering stage operates with **0 heap allocations** on the hot path via `sync.Pool` byte buffer reuse and `RenderToBuffer`.
  - Enforce $P99 \le 0.5$ms rendering latency under parallel multi-goroutine load.
- **Observability & Pipeline Metrics**:
  - Expose Prometheus metrics:
    - `rag_orchestrator_pipeline_duration_seconds` (Histogram partitioned by mode and dialect)
    - `rag_orchestrator_context_bytes` (Histogram tracking assembled context sizes)
    - `rag_orchestrator_requests_total` (Counter partitioned by status)
    - Pass-through SDK metrics (`prompt_sdk_render_duration_seconds`, `prompt_sdk_cache_hit_ratio`, `prompt_sdk_pubsub_connected`).

### 3. Resilience, Chaos & Outage Serving
- **Control Plane & Redis Outage Resilience**:
  - Calling orchestrators continue rendering cached templates in memory with 100% availability and $<0.5$ms latency even when PostgreSQL, Redis, and the Control Plane daemon are completely offline.
- **Stale-While-Offline Past Fallback TTL**:
  - When the 5-minute fallback tag TTL expires during a sustained outage, singleflight coalescing prevents thundering herds, attempts a background refresh, records a cooldown, and immediately serves the cached version from memory without failing caller requests.
- **Uncached Tag Requests During Outage**:
  - If an un-cached tag is requested during an outage, the SDK returns `ErrTemplateNotFound` cleanly with cooldown protection, avoiding socket or goroutine leaks.

### 4. Integration & Benchmark Test Verification
- **Multi-Instance Invalidation Delivery Test (`test/integration/phase3_invalidation_bus_test.go`)**:
  - Spin up 3+ separate `Client` instances with distinct `NodeID`s subscribing to the same Redis Pub/Sub invalidation bus.
  - Verify tenant isolation: invalidation for Tenant B does not evict Tenant A's tags.
  - Mutate `@production` pointer from $v_1 \to v_2$ on the Control Plane.
  - Assert that all 3 client instances evict their local cache keys in $<500$ms and render $v_2$.
  - Test Pub/Sub disconnect & recovery (Runbook 2): verify fallback to 5-minute TTL on disconnect, `prompt_sdk_pubsub_connected` gauge transitions, and reconnect backoff behavior.
  - Test diagnostic cache wipe via `Client.Purge()`.
- **Orchestrator Pipeline Integration & Outage Test (`test/integration/phase3_orchestrator_test.go`)**:
  - End-to-end integration test of the `pkg/orchestrator` pipeline with 50 KB context chunks across Anthropic XML, Meta Llama-3, and OpenAI Standard dialects.
  - Dual-mode execution verification (Production vs Diagnostic modes).
  - Outage test: kill registry and Redis; verify orchestrator renders cached templates continuously with 0 errors and $P99 \le 0.5$ms.
- **Benchmark Suite**:
  - Run benchmark verifying 0 allocs/op during orchestrator rendering and multi-instance concurrent invalidation.

---

## 2. Files to Create / Modify

- `pkg/promptsdk/client.go`:
  - Enhance Redis Pub/Sub subscriber with exponential backoff and jitter reconnect loop.
  - Add tenant boundary check to invalidation processing.
  - Add `Client.Purge()` method for forced cache wipe (Runbook 2).
- `pkg/promptsdk/cache.go`:
  - Add `PurgeTags()` method clearing all mutable tag pointers in L1_tag while preserving immutable L1_version entries.
- `pkg/orchestrator/types.go`:
  - Request/response contracts, `RetrievedChunk`, `ExecutionMode` (Production vs Diagnostic), `ContextRetriever`, `InferenceDispatcher`, and `DiagnosticTrace`.
- `pkg/orchestrator/pipeline.go`:
  - Production-grade RAG search orchestrator pipeline integrating context assembly, zero-allocation `PromptSDK.RenderToBuffer`, tokenizer boundary guarding, and inference dispatch.
- `pkg/orchestrator/metrics.go`:
  - Prometheus metrics for the orchestrator pipeline (`rag_orchestrator_pipeline_duration_seconds`, `rag_orchestrator_context_bytes`, `rag_orchestrator_requests_total`).
- `pkg/orchestrator/pipeline_test.go`:
  - Unit tests for the pipeline: 50 KB context chunks, dialect wrapping, token truncation, and dual-mode traces.
- `test/integration/phase3_invalidation_bus_test.go`:
  - Multi-instance cache invalidation tests verifying $<500$ms global propagation across 3+ SDK nodes, tenant isolation, Runbook 1 pointer rollbacks, and Runbook 2 connection loss fallback.
- `test/integration/phase3_orchestrator_test.go`:
  - End-to-end orchestrator pipeline integration tests with control-plane outage resilience verification.
- `openapi/openapi.yaml` & `.ai/doc-api.md`:
  - Synchronize documented contracts, orchestrator architecture, and metrics matrix.
- `.ai/progress-tracker.md`:
  - Update roadmap status and completed Phase 3 deliverables.

---

## 3. Security & Operational Invariants Enforcement

- **Decoupled Architecture (ADR-001)**: The orchestrator pipeline imports `pkg/promptsdk` directly. Large 50 KB context payloads are rendered in-process without network gRPC serialization. Network inference is decoupled via `InferenceDispatcher`.
- **Instant Cache Coherence (ADR-002)**: Pointer updates publish to `channel:prompt:invalidation`, synchronizing all distributed nodes in $<500$ms.
- **PgBouncer Simple Protocol (ADR-003)**: Relational operations during tag promotion run in Simple Protocol Mode.
- **Transactional Internal Audit Ledger (ADR-004)**: Tag promotion updates and audit entries commit atomically in PostgreSQL.
- **Zero-Allocation Hot Path (Invariant 4)**: 50 KB context rendering in the orchestrator uses pooled byte buffers and `RenderToBuffer` yielding `0 allocs/op` and $P99 \le 0.5$ms.
- **Secured Logging (Invariant 2)**: All logging uses structured JSON (`log/slog`), strictly redacting raw context, prompt templates, and credentials.
- **Continuous Doc API Sync (Invariant 1)**: Sync `.ai/doc-api.md` and `openapi/openapi.yaml` on every change.
- **Git Commit Standards**: Minimum two conventional commits (`feat:`, `tests:`, `docs:`) during Phase 3.

---

## 4. Verification Steps

1. `gofmt -w -s .` (zero diff).
2. `go vet ./...` (zero warnings).
3. `go test -v -race ./pkg/promptsdk/... ./pkg/orchestrator/...` (unit tests passing).
4. `go test -v -race ./test/integration/phase3_invalidation_bus_test.go` (multi-instance $<500$ms propagation test passing).
5. `go test -v -race ./test/integration/phase3_orchestrator_test.go` (orchestrator end-to-end and control plane outage tests passing).
6. `go test -v -bench=. -benchmem ./pkg/promptsdk/... ./test/sdk/... ./pkg/orchestrator/...` (validating zero heap allocations on hot path and $P99 \le 0.5$ms).
7. Secured logging audit: verify zero leaks of raw context or prompt data.
8. Validate Doc API & OpenAPI synchronization.
