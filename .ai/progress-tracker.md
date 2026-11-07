# Progress Tracker - RAG Prompt Registry Service

## Current Phase

- **Phase 1: Schemas & Relational Core (Weeks 1–2)** (🟡 Initializing Foundation)

## Current Goal

- Establish microservice foundation for `rag-prompt-registry-service`: `.gitignore`, GitHub Actions CI pipeline, OpenAPI 3.1 & Doc API contracts, master agent directives (`GEMINI.md`), and core architecture context files.

---

## Calibrated 4-Phase Roadmap & Milestones

| Phase | Timeline | Focus Area | Deliverables & Exit Criteria | Status |
| :--- | :--- | :--- | :--- | :--- |
| **Phase 1: Schemas & Relational Core** | Weeks 1–2 | Control Plane Database & Immutability | • `.gitignore` ignoring `tasks/` and build artifacts.<br>• Comprehensive GitHub Actions CI workflow (lint-and-typecheck, security-and-logging-audit, OpenAPI 3.1 sync, test-suites).<br>• Full `.ai` context overhaul and `GEMINI.md` aligned with backend concepts.<br>• PostgreSQL 16 schema with strict tenant isolation, immutable version tables, tag pointers, and an internal audit ledger.<br>• PgBouncer integration verifying Simple Protocol Mode via `jackc/pgx/v5`.<br>• Core gRPC/REST management APIs for prompt versioning and schema registration. | 🟡 In Progress |
| **Phase 2: In-Process Engine (`go-prompt-sdk`)** | Weeks 3–4 | Compiler, Buffer Pools & Dialects | • Implementation of `go-prompt-sdk` featuring `sync.Pool` zero-allocation string interpolation.<br>• Dialect adapters for Anthropic XML, Meta Llama-3 special tokens, and standard JSON formats.<br>• Tokenizer budgeting hooks calculating framing overhead before interpolation.<br>• Unit test suite & memory benchmark validation ($P99 \le 0.5$ms, 0 heap escapes across 50 KB context blocks). | ⚪ Pending |
| **Phase 3: Cache Invalidation & Orchestrator Wiring** | Weeks 5–6 | Redis Pub/Sub & SDK Synchronization | • Multi-instance cache invalidation bus via Redis Pub/Sub (`channel:prompt:invalidation`).<br>• Embedded SDK integration into `rag-search-orchestrator` pipeline.<br>• Benchmark validation: $<0.5$ms rendering times and $<500$ms pointer propagation. | ⚪ Pending |
| **Phase 4: Resilience, Chaos & Production Rollout** | Weeks 7–8 | Fault Tolerance & Release Gating | • Chaos testing: killing registry pods while validating uninterrupted SDK in-memory rendering.<br>• Allocation profiling verifying zero memory accumulation on 10,000 RPS orchestrator loads.<br>• Production deployment via Helm and canary cutover. | ⚪ Pending |

---

## Completed Units

- **Phase 1 Foundation Setup**:
  - **Git Ignore Configuration** (`.gitignore`):
    - Configured comprehensive ignore rules ignoring `tasks/` reference folder, Go binaries, test caches, OS artifacts, and secrets.
  - **GitHub Actions CI/CD Pipeline** (`.github/workflows/ci.yml`):
    - Established CI workflow with dedicated jobs:
      - `lint-and-typecheck`: `gofmt` check, `go vet`, and `golangci-lint`.
      - `security-and-logging-audit`: `govulncheck`, `gosec`, and a `grep`-based log keyword pattern check screening for common sensitive identifiers on single-line log calls (heuristic regex check; does not perform AST analysis or guarantee zero sensitive-value leaks).
      - `openapi-doc-api-contract-sync`: OpenAPI 3.1 validation via `openapi-spec-validator` and a `.ai/doc-api.md` file-presence and two-string marker check (verifies presence of the file and two required text markers, not full contract equivalence).
      - `test-suites`: Unit tests, SDK allocation benchmarks, and integration tests.
  - **Context & Architecture System Overhaul** (`.ai/`, `.ai.md`):
    - Synchronized all `.ai` context files (`project-overview.md`, `architecture.md`, `doc-api.md`, `code-standards.md`, `security.md`, `ai-workflow-rules.md`, `agents.md`, `progress-tracker.md`) to the `rag-prompt-registry-service` Go microservice specification.
    - Integrated core 5-layer concepts from `Production Grade Backend Concepts.pdf` into architecture and code standards.
  - **Master Agent Directive** (`GEMINI.md`):
    - Created comprehensive system prompt referencing `.ai/agents.md` and all `.ai/` context files, enforcing non-negotiable invariants, secured logging, continuous Doc API / OpenAPI updates, and Git commit standards.
  - **Pressure Testing & Contract Hardening**:
    - Pressure-tested full system architecture, directives, and invariants against `tasks/RAG Prompt Registry Service Plan.pdf` and backend standards.
    - Synchronized `openapi/openapi.yaml` and `.ai/doc-api.md` to 100% path and model parity (adding schemas endpoints, version AST lookups, and pointer resolution).
    - Hardened PostgreSQL DDL with explicit `tenant_id` defense-in-depth on `prompt_tags` and `prompt_audit_ledger` with composite constraints and indices.
    - Unified operational rollback runbook parameters and contracts.
  - **Code Review Remediation & Contract Hardening**:
    - Standardized master agent directive filename to `GEMINI.md` and updated all references.
    - Updated `jackc/pgx/v5` PgBouncer Simple Protocol configuration references to `ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol`.
    - Enforced required `schema_id` on `CreateTemplateRequest` and `PublishVersionRequest` (REST, gRPC, and PostgreSQL foreign key) and optimistic locking `expected_version` with `409 Conflict` / gRPC `ABORTED` on `PromoteTagRequest`.
    - Added global JWT `bearerAuth` security scheme (`security: []` on probe and documentation endpoints) and `maxItems: 100` bounds to OpenAPI 3.1 list responses.
    - Hardened `.github/workflows/ci.yml` (`permissions: contents: read`, `persist-credentials: false`, pinned `govulncheck@v1.1.3` and `gosec@v2.21.4`, `pipx` OpenAPI validator, case-insensitive `grep -qi`, and strict test failure propagation).

---

## Architectural Decision Records (ADRs)

- **ADR-001: Separation of Registry Control Plane and SDK Data Plane**: The registry service exclusively manages metadata, versioning, and schemas over low-volume APIs. String interpolation and dialect compilation run in-process within calling services via `go-prompt-sdk` to eliminate network serialization bottlenecks.
- **ADR-002: Invalidation via Redis Pub/Sub**: Eliminates static TTL split-brain windows by broadcasting tag pointer updates (`@production`) to all distributed SDK caches over Redis Pub/Sub (`channel:prompt:invalidation`).
- **ADR-003: Simple Protocol Mode for PgBouncer**: Configures the PostgreSQL connection pool (`jackc/pgx/v5`) to use Simple Protocol Mode (`ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol`), avoiding prepared statement failures in transaction pooling mode.
- **ADR-004: Lightweight Audit Ledger**: Administrative changes write directly to an internal `prompt_audit_ledger` table within the same transaction, deprecating external Debezium/Kafka CDC requirements for this service.

---

## Operational Runbooks

### Runbook 1: Immediate Pointer Rollback
1. Identify application regressions triggered by a new prompt version.
2. Run the pointer mutation command via CLI or admin endpoint:
   `PUT /v1/templates/{id}/tags/@production` with payload: `{"target_version": <previous_version_int>, "expected_version": <current_version_int>, "updated_by": "admin-rollback"}`.
   *(Note: `{id}` is either the template UUID or tenant-unique template name)*.
3. The service updates PostgreSQL and emits an invalidation event to Redis Pub/Sub.
4. Confirm via Prometheus that orchestrator nodes pick up the target version within $<500$ms.

### Runbook 2: Resolving Pub/Sub Invalidation Drops
1. If an orchestrator instance loses its Redis Pub/Sub connection, it falls back to the 5-minute safety TTL on mutable pointer tags.
2. Check the `prompt_sdk_pubsub_connected` gauge on the orchestrator.
3. If disconnected, restart the client connection pool or force a local cache wipe via the orchestrator's diagnostic endpoint.
