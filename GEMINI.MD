# GEMINI.MD — Master Agent Directive: RAG Prompt Registry Service

You are operating as a **Principal Go Distributed Systems & Backend Platform Engineer** responsible for implementing, maintaining, and scaling the **`rag-prompt-registry-service`**.

---

## 1. Primary References & Context System

Before implementing, refactoring, or making any architectural decision, you MUST read and reference the system context in the following sequence:

1. **[`.ai/agents.md`](.ai/agents.md)** — Agent Persona, Execution Protocols, and System Role.
2. **[`.ai.md`](.ai.md)** — Master Application Building Context and Invariants Overview.
3. **[`.ai/project-overview.md`](.ai/project-overview.md)** — Product Definition, Decoupled Topology (Control Plane vs. In-Process Data Plane SDK), and Calibrated SLIs/SLOs.
4. **[`.ai/architecture.md`](.ai/architecture.md)** — 5 Backend Layers (aligned with `Production Grade Backend Concepts.pdf`), PgBouncer Simple Protocol, Redis Pub/Sub Invalidation Bus, and Buffer Pooling.
5. **[`.ai/doc-api.md`](.ai/doc-api.md)** — Documented APIs (OpenAPI 3.1 HTTP endpoints `/docs`, `/redoc`, `/api/docs`, `/openapi.yaml`, gRPC contracts `proto/v1/prompt_service.proto`, PostgreSQL schema, and Go SDK interfaces).
6. **[`.ai/code-standards.md`](.ai/code-standards.md)** — Go 1.22+ conventions, `golangci-lint`, `jackc/pgx/v5` `PreferSimpleProtocol`, `sync.Pool` zero-allocation compiler, and error handling.
7. **[`.ai/security.md`](.ai/security.md)** — Threat Model, Secured Logging Invariants, Tenant Isolation, and Distroless Container Hardening.
8. **[`.ai/ai-workflow-rules.md`](.ai/ai-workflow-rules.md)** — Spec-Driven Workflow, Human-in-the-Loop Rules, Continuous Doc API sync, and Pre-Commit Checklist.
9. **[`.ai/progress-tracker.md`](.ai/progress-tracker.md)** — 4-Phase Roadmap, Completed Units, ADR-001 through ADR-004, and Operational Runbooks.

---

## 2. Core Operational Invariants

### Invariant 1: Continuous Doc API Updates
The [`.ai/doc-api.md`](.ai/doc-api.md) file and OpenAPI 3.1 specifications (`openapi/openapi.yaml`, `/docs`, `/api/docs`) **MUST be kept updated on every code change or addition**, no matter how little or small the created or modified code is. Any addition of endpoints, data models, gRPC messages, or database tables must be reflected immediately in `doc-api.md` and `openapi/openapi.yaml`.

### Invariant 2: Secured Logging
All logging across the entire service, embedded SDK, and utilities must be structured JSON (using Go `log/slog` with `slog.NewJSONHandler`).
- **Strict Redaction**: Never log raw prompt templates, raw context passages, golden-set text spans, customer secrets, authorization headers, presigned URLs, or database connection credentials.
- **Allowed Attributes**: Emit only operational metadata (`trace_id`, `tenant_id`, `template_id`, `version`, `tag`, `latency_ms`, and error codes) or truncated SHA-256 digests (`sha256(ast)[:16]`).

### Invariant 3: Decoupled Control vs. Data Plane (ADR-001)
Administrative lifecycle governance (CRUD, append-only versioning $v_1 \dots v_n$, mutable pointer tags `@production`, JSON schema validation) resides exclusively in the Go control plane daemon (`cmd/prompt-registry-server`). Runtime variable binding, dialect compilation, and token budgeting run strictly in-process within calling orchestrators (`rag-search-orchestrator`) via `go-prompt-sdk` (`pkg/promptsdk`). Never transmit raw context blocks over gRPC for string concatenation.

### Invariant 4: Zero-Allocation In-Process Compilation
String interpolation in `pkg/promptsdk/compiler.go` must draw byte buffers from a global `sync.Pool`. Buffers must be reset before returning to the pool. Hot path execution must yield **0 heap escapes** and $P99 \le 0.5$ms latency across 50 KB context blocks.

### Invariant 5: Instant Cache Coherence via Redis Pub/Sub (ADR-002)
Pointer promotions (`@production`, `@staging`, `@canary`) emit eviction events to Redis Pub/Sub (`channel:prompt:invalidation`), invalidating all distributed in-memory SDK caches within $<100$ms ($<500$ms global propagation SLO).

### Invariant 6: PgBouncer Simple Protocol Mode (ADR-003)
PostgreSQL connection pooling via `jackc/pgx/v5` must explicitly configure `PreferSimpleProtocol = true` to prevent prepared statement failures across PgBouncer transaction pooling.

### Invariant 7: Transactional Internal Audit Ledger (ADR-004)
Administrative template mutations and `prompt_audit_ledger` entries must commit atomically in the exact same database transaction, removing external Debezium/Kafka CDC requirements.

### Invariant 8: Tokenizer Budget Safety Guard
Markup transformers must calculate the structural overhead of vendor-specific framing (e.g. Anthropic XML tags, Meta Llama-3 special headers) using the tokenizer budget guard before injecting context, ensuring downstream inference calls never breach model context limits.

---

## 3. Git Commit Standards

- **Git Commit Frequency**: Commit code as you build. Every working session or phase must contain **at least two git commits**.
- **Conventional Commit Patterns**: All git commit messages must strictly adhere to the following prefixes:
  - `feat:` for new features 
  - `fix:` for bug fixes
  - `refactor:` for code refactoring
  - `docs:` for documentation updates
  - `tests:` for tests addition and modification
  - `chore:` for maintenance tasks and environment setup

---

## 4. Pre-Commit Verification Checklist

Before finalizing any unit or marking any task complete:

1. **Format Code**: Run `gofmt -w -s .`.
2. **Lint & Vet**: Run `go vet ./...` and `golangci-lint run`.
3. **Run Tests & Benchmarks**: Ensure unit tests (`test/unit/...`), integration tests (`test/integration/...`), and SDK allocation benchmarks (`go test -bench=. -benchmem ./pkg/promptsdk/...`) pass cleanly.
4. **Audit Secured Logging**: Verify zero leaks of raw prompt/context text, secrets, or credentials in log output.
5. **Verify Doc API & OpenAPI Sync**: Ensure `.ai/doc-api.md` and `openapi/openapi.yaml` match the current code state.
6. **Update Progress Tracker**: Update `.ai/progress-tracker.md` with session notes and phase progress.
