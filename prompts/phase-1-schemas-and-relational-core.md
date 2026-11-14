# Implementation Plan: Phase 1 — Schemas, Relational Core & Control Plane APIs

## 1. Objective & Scope

Complete **Phase 1: Schemas & Relational Core** of the `rag-prompt-registry-service` in strict alignment with `GEMINI.md`, `.ai/` normative specifications, and the reference blueprints in `tasks/` (`RAG Prompt Registry Service Plan.pdf`, `Production Grade Backend Concepts.pdf`, and `Disaggregated Hybrid Model Architecture.pdf`):

1. **Go Module & Build Configuration**:
   - Initialize `go.mod` (`rag-prompt-registry-service`, Go 1.22+), `Makefile`, and `.env.example`.

2. **PostgreSQL 16+ Relational Schema, Immutability Triggers & Migrations**:
   - Create SQL migrations in `internal/infrastructure/persistence/migrations/` (`000001_init_schema.up.sql`, `000001_init_schema.down.sql`, and `migrations.go` embedding `*.sql` via `//go:embed`) for:
     - `prompt_templates`: Aggregate root with `(tenant_id, name)` unique constraint.
     - `json_schemas`: Output schema registry with `(tenant_id, name)` unique constraint.
     - `prompt_versions`: Append-only immutable versions ($v_1 \dots v_n$) with `CHECK (version >= 1)`, `ast_hash VARCHAR(64) NOT NULL` for $O(1)$ idempotent digest lookups, `schema_id` FK (`ON DELETE RESTRICT`), and `(template_id, version)` unique constraint.
     - `prompt_tags`: Mutable semantic pointers (`@production`, `@staging`, `@canary`) with `CHECK (target_version >= 1)`, composite unique constraint `(tenant_id, template_id, tag)`, and composite foreign key `FOREIGN KEY (template_id, target_version) REFERENCES prompt_versions(template_id, version) ON DELETE RESTRICT`.
     - `prompt_audit_ledger`: Transactional compliance ledger (per ADR-004) recording `TEMPLATE_CREATED`, `VERSION_PUBLISHED`, `TAG_PROMOTED`, `TAG_ROLLED_BACK`, and `SCHEMA_REGISTERED` with redacted operational metadata in `payload_snapshot` (`ast_hash`, `schema_id`, `previous_version`, `target_version`, `variable_keys`, `default_dialect`).
     - **Database-Level Immutability Enforcement**: `BEFORE UPDATE OR DELETE` PostgreSQL triggers (`prevent_append_only_mutation()`) on both `prompt_versions` and `prompt_audit_ledger` raising an exception on any mutation or deletion attempt.

3. **PgBouncer Simple Protocol Persistence Layer (ADR-003 & ADR-004)**:
   - Implement `internal/infrastructure/persistence/postgres/pool.go` using `jackc/pgx/v5/pgxpool` with explicit `ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol` (the `pgx/v5` equivalent of `PreferSimpleProtocol`) to prevent prepared statement errors across PgBouncer transaction pooling.
   - Implement `internal/infrastructure/persistence/postgres/prompt_repo.go` and `audit_repo.go` enforcing:
     - **Strict Multi-Tenant Isolation**: Every SQL query includes `tenant_id` in its `WHERE` clause; `CreateTemplate` and `PublishVersion` verify that `schema_id` belongs to the same `tenant_id`.
     - **Dual Identifier Resolution (`UUID` vs. `name`)**: If `{id}` parses as a UUID, resolve by `(tenant_id, id)`; otherwise resolve by `(tenant_id, name)` (supporting Runbook 1 `PUT /v1/templates/{name}/tags/@production`).
     - **Serialized Monotonic Version Allocation & Idempotency**: Within the PostgreSQL transaction, acquire a row-level lock on the template aggregate root (`SELECT id FROM prompt_templates WHERE tenant_id = $1 AND id = $2 FOR UPDATE`), check `(template_id, ast_hash)` for idempotent version replay (returning the existing version without duplicate insertion when the canonical AST + `schema_id` digest matches), and otherwise allocate `COALESCE(MAX(version), 0) + 1`.
     - **Atomic ACID Audit Commits (ADR-004)**: Every template creation, version publication, tag promotion/rollback, and schema registration writes its `prompt_audit_ledger` record inside the exact same PostgreSQL transaction (`tx.Commit()`).
     - **Optimistic Pointer Locking**: `PromoteTag` validates `expected_version` (`0` when unassigned, or current `target_version`), returning conflict (`409 Conflict` / gRPC `ABORTED`) on mismatch, and classifies `target_version < expected_version` as `TAG_ROLLED_BACK` in `prompt_audit_ledger`.

4. **Redis 7.2 L2 Distributed Cache & Pub/Sub Invalidation Broadcaster (ADR-002)**:
   - Implement `internal/infrastructure/cache/redis/cluster_client.go` providing:
     - Read-through / write-through L2 caching for immutable AST versions (`l2:tenant:{tenant_id}:template:{id}:v:{version}`), JSON schemas (`l2:tenant:{tenant_id}:schema:{id}`), and mutable tag pointers (`l2:tenant:{tenant_id}:template:{id}:tag:{tag}` with 5-minute TTL and immediate eviction on `PromoteTag`) to achieve the $P99 \le 2\text{ms}$ Control Plane Registry Read Latency SLO.
   - Implement `internal/infrastructure/cache/redis/invalidation_pub.go` publishing JSON invalidation payloads (`event_id` ULID, `tenant_id`, `template_id`, `tag`, `new_version`, `timestamp`) to `channel:prompt:invalidation` immediately after transaction commit and incrementing `prompt_invalidation_events_published_total{status="success|error"}`.

5. **Control Plane Domain & Application Layers**:
   - Implement domain entities in `internal/domain/entity/`:
     - `prompt_template.go`: `PromptTemplate`, `PromptVersion`, `TemplateAST` (including `system_prompt`, `messages`, `variable_keys`, `default_dialect`, optional `persona_overrides`, and optional `few_shot_exemplars`), `MessageBlock`, and `JSONSchema`. Enforces fail-fast AST structural validation, balanced `{{variable}}` placeholder verification against `variable_keys`, JSON Schema draft validation, canonical SHA-256 digest computation (`ast_hash`), and truncated logging digest (`sha256(ast)[:16]`).
     - `prompt_tag.go`: `PromptTag` aggregate (`@production`, `@staging`, `@canary`) with tag name validation and optimistic `expected_version` transition checks.
     - `audit_entry.go`: `AuditEntry` and redacted `AuditPayloadSnapshot` models.
   - Implement repository interfaces in `internal/domain/repository/`:
     - `prompt_repository.go`: Template, Version, Tag, and JSON Schema persistence contracts.
     - `audit_repository.go`: Transactional audit ledger persistence contract.
   - Implement CQRS handlers in `internal/application/` mapped cleanly to the canonical directory tree:
     - `command/publish_version.go`: `CreateTemplate` (initial $v_1$), `PublishVersion` ($v_{n+1}$ with idempotency), and `RegisterSchema`, recording `prompt_registry_mutation_duration_seconds`.
     - `command/promote_tag.go`: `PromoteTag` (optimistic locking, transactional audit, L2 cache update, and Redis Pub/Sub invalidation broadcast), recording `prompt_registry_mutation_duration_seconds`.
     - `query/get_template_ast.go`: `GetTemplateVersion`, `ResolveTag`, and `GetSchema` (backed by Redis L2 read-through cache with PostgreSQL fallback).
     - `query/list_templates.go`: Cursor-paginated (`limit <= 100`) `ListTemplates` and `ListSchemas`.

6. **Protobuf v3 & Transport Layer (HTTP/REST + Doc API + gRPC)**:
   - Create `proto/v1/types.proto` and `proto/v1/prompt_service.proto` (including `tenant_id` scoping and `persona_overrides` / `few_shot_exemplars` on `TemplateAST`) along with Go gRPC service bindings in `proto/v1/`.
   - Implement `internal/transport/grpc/prompt_admin_server.go`:
     - Exposes `PromptRegistryService` (`CreateTemplate`, `PublishVersion`, `GetTemplateVersion`, `PromoteTag`, `ResolveTag`, `RegisterSchema`, `GetSchema`, `ListTemplates`, `ListSchemas`).
     - Enforces `grpc.MaxRecvMsgSize(1 << 20)` (1 MiB) and UnaryServerInterceptors for ULID `trace_id`, bearer JWT / `x-tenant-id` extraction & cross-tenant mismatch rejection, panic recovery, and secured `log/slog` JSON logging.
   - Implement `internal/transport/http/router.go` exposing:
     - Administrative REST endpoints (`/v1/templates`, `/v1/templates/{id}/versions`, `/v1/templates/{id}/versions/{version}`, `/v1/templates/{id}/tags/{tag}`, `/v1/schemas`, `/v1/schemas/{id}`).
     - Interactive OpenAPI 3.1 Doc API endpoints (`/docs` Swagger UI, `/redoc` ReDoc UI, `/api/docs`, `/openapi.yaml`).
     - Operational endpoints (`/healthz`, `/ready`, `/metrics` exposing `prompt_registry_mutation_duration_seconds`, `prompt_invalidation_events_published_total`, `prompt_sdk_render_duration_seconds`, `prompt_sdk_cache_hit_ratio`, and `prompt_sdk_pubsub_connected`).
     - Middleware: 1 MiB `http.MaxBytesReader` body guard (protecting the $\le 128\text{ MiB}$ RSS pod footprint), ULID `request_id`/`trace_id` correlation, bearer JWT & `X-Tenant-ID` extraction, `Idempotency-Key` support, token-bucket write rate limiting, panic recovery, and secured `log/slog` JSON logging.
   - Implement `cmd/prompt-registry-server/main.go` with 12-factor environment configuration and graceful `SIGINT`/`SIGTERM` shutdown (15s drain timeout).

7. **SDK Package Scaffold & Phase 1 Verification Suites**:
   - Provide `pkg/promptsdk/doc.go` and `pkg/promptsdk/sdk_test.go` matching `.ai/doc-api.md` Section 7 so CI's `go test -v -bench=. -benchmem ./pkg/promptsdk/...` target runs and passes benchmarks cleanly as soon as `go.mod` is introduced (full Phase 2 engine follows in Phase 2).
   - Implement unit tests in `test/unit/...`:
     - AST validation, `{{variable}}` balanced delimiter & key checking, `persona_overrides`, and JSON Schema validation.
     - Idempotent version publishing (`ast_hash` match) and monotonic $v_{n+1}$ allocation.
     - Optimistic tag locking (`expected_version` mismatch $\to$ `409 Conflict` / gRPC `ABORTED`) and rollback classification (`TAG_ROLLED_BACK`).
     - PgBouncer Simple Protocol configuration verification (`ConnConfig.DefaultQueryExecMode == pgx.QueryExecModeSimpleProtocol`).
     - HTTP & gRPC transport handlers, dual `{id}` (UUID vs. template `name`) resolution, cross-tenant isolation, Doc API routes (`/docs`, `/redoc`, `/api/docs`, `/openapi.yaml`), and secured logging redaction audit.
   - Implement integration tests in `test/integration/...` covering the 3 mandatory blueprint scenarios from Section 6 of `RAG Prompt Registry Service Plan.pdf`:
     1. **PgBouncer Simple Protocol Concurrency**: High-concurrency writes verifying zero `"prepared statement does not exist"` errors under transaction pooling semantics.
     2. **Transactional Audit Ledger Commits (ADR-004)**: Verifying atomic commit and rollback of template/version/tag/schema writes together with `prompt_audit_ledger` rows.
     3. **Redis L2 Cache & Pub/Sub Invalidation Delivery (ADR-002)**: Verifying L2 AST/schema caching and immediate `channel:prompt:invalidation` broadcast when `@production` is promoted from $v_1 \to v_2$.

---

## 2. Files to Create / Modify

- `go.mod` & `go.sum`
- `Makefile` & `.env.example`
- `proto/v1/types.proto` & `proto/v1/prompt_service.proto` (+ Go protobuf/gRPC bindings in `proto/v1/`)
- `internal/infrastructure/persistence/migrations/000001_init_schema.up.sql`
- `internal/infrastructure/persistence/migrations/000001_init_schema.down.sql`
- `internal/infrastructure/persistence/migrations/migrations.go`
- `internal/domain/entity/prompt_template.go`
- `internal/domain/entity/prompt_tag.go`
- `internal/domain/entity/audit_entry.go`
- `internal/domain/repository/prompt_repository.go`
- `internal/domain/repository/audit_repository.go`
- `internal/application/command/publish_version.go`
- `internal/application/command/promote_tag.go`
- `internal/application/query/get_template_ast.go`
- `internal/application/query/list_templates.go`
- `internal/infrastructure/persistence/postgres/pool.go`
- `internal/infrastructure/persistence/postgres/prompt_repo.go`
- `internal/infrastructure/persistence/postgres/audit_repo.go`
- `internal/infrastructure/cache/redis/cluster_client.go`
- `internal/infrastructure/cache/redis/invalidation_pub.go`
- `internal/transport/http/router.go`
- `internal/transport/grpc/prompt_admin_server.go`
- `cmd/prompt-registry-server/main.go`
- `pkg/promptsdk/doc.go` & `pkg/promptsdk/sdk_test.go`
- `test/unit/...` & `test/integration/...`
- `.ai/doc-api.md`, `openapi/openapi.yaml`, `.ai/architecture.md`, and `.ai/progress-tracker.md`

---

## 3. Security & Operational Invariants Enforcement

- **Secured Logging (Invariant 2)**: Only emit structured JSON via `slog.NewJSONHandler` with operational metadata (`trace_id`, `tenant_id`, `template_id`, `version`, `tag`, `latency_ms`, `template_hash = sha256(ast)[:16]`). Never log raw prompt templates, context blocks, persona override strings, secrets, bearer tokens, or DB URIs.
- **PgBouncer Simple Protocol (Invariant 6 / ADR-003)**: `pool.go` explicitly sets `cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol`.
- **Transactional Audit Ledger (Invariant 7 / ADR-004)**: Every template creation, version publication, tag promotion/rollback, and schema registration writes to `prompt_audit_ledger` within the same PostgreSQL transaction, enforced against tampering by a `BEFORE UPDATE OR DELETE` trigger.
- **Control Plane Pod Footprint Guard**: Enforce 1 MiB request payload ceilings on HTTP and gRPC ingress and bounded pagination (`limit <= 100`) to remain strictly within the $\le 128\text{ MiB}$ RSS limit.
- **Continuous Doc API Sync (Invariant 1)**: Synchronize `.ai/doc-api.md` and `openapi/openapi.yaml` with all schema, Protobuf, and endpoint refinements.
- **Git Commit Standards**: Make at least two atomic conventional commits (`feat:`, `tests:`, `docs:`) during the phase after passing all pre-commit checks.

---

## 4. Verification Steps

1. `gofmt -w -s .` (zero diff).
2. `go vet ./...` (zero warnings/errors).
3. `go test -v -race ./test/unit/...`
4. `go test -v -bench=. -benchmem ./pkg/promptsdk/...`
5. `go test -v ./test/integration/...`
6. Secured logging grep audit from `.github/workflows/ci.yml`.
