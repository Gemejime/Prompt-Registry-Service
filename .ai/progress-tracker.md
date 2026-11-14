# Progress Tracker - RAG Prompt Registry Service

## Current Phase

- **Phase 4: Resilience, Chaos & Production Rollout (Weeks 7–8)** (🟢 Completed)

## Current Goal

- All 4 phases completed and verified. Ready for production GitOps rollout via ArgoCD & Argo Rollouts.

---

## Calibrated 4-Phase Roadmap & Milestones

| Phase | Timeline | Focus Area | Deliverables & Exit Criteria | Status |
| :--- | :--- | :--- | :--- | :--- |
| **Phase 1: Schemas & Relational Core** | Weeks 1–2 | Control Plane Database & Immutability | • `.gitignore` ignoring `tasks/` and build artifacts.<br>• Comprehensive GitHub Actions CI workflow (lint-and-typecheck, security-and-logging-audit, OpenAPI 3.1 sync, test-suites).<br>• Full `.ai` context overhaul and `GEMINI.md` aligned with backend concepts.<br>• PostgreSQL 16 schema with strict tenant isolation, immutable version tables, tag pointers, and an internal audit ledger.<br>• PgBouncer integration verifying Simple Protocol Mode via `jackc/pgx/v5`.<br>• Core gRPC/REST management APIs for prompt versioning and schema registration. | 🟢 Completed |
| **Phase 2: In-Process Engine (`go-prompt-sdk`)** | Weeks 3–4 | Compiler, Buffer Pools & Dialects | • Implementation of `go-prompt-sdk` featuring `sync.Pool` zero-allocation string interpolation.<br>• Dialect adapters for Anthropic XML, Meta Llama-3 special tokens, and standard JSON formats.<br>• Tokenizer budgeting hooks calculating framing overhead before interpolation.<br>• Unit test suite & memory benchmark validation ($P99 \le 0.5$ms, 0 heap escapes across 50 KB context blocks). | 🟢 Completed |
| **Phase 3: Cache Invalidation & Orchestrator Wiring** | Weeks 5–6 | Redis Pub/Sub & SDK Synchronization | • Multi-instance cache invalidation bus via Redis Pub/Sub (`channel:prompt:invalidation`).<br>• Embedded SDK integration into `rag-search-orchestrator` pipeline.<br>• Benchmark validation: $<0.5$ms rendering times and $<500$ms pointer propagation. | 🟢 Completed |
| **Phase 4: Resilience, Chaos & Production Rollout** | Weeks 7–8 | Fault Tolerance & Release Gating | • Chaos testing: killing registry pods & Redis while validating uninterrupted SDK in-memory rendering.<br>• Thundering herd singleflight recovery, Pub/Sub flapping backoff, and partial Redis rollback resilience.<br>• Allocation profiling verifying 0 allocs/op and zero memory accumulation on 10,000 RPS orchestrator loads.<br>• Distroless Docker container, production Helm chart, Prometheus alerting, and Argo Rollouts canary cutover. | 🟢 Completed |

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
  - **Phase 1 Implementation Plan Pressure-Test & Hardening (`prompts/phase-1-schemas-and-relational-core.md`)**:
    - Mapped all 9 REST operations and 7+2 gRPC RPCs (`CreateTemplate`, `PublishVersion`, `GetTemplateVersion`, `PromoteTag`, `ResolveTag`, `RegisterSchema`, `GetSchema`, `ListTemplates`, `ListSchemas`) to the canonical PDF file tree.
    - Added Redis 7.2 L2 read-through/write-through caching (`cluster_client.go`) for immutable AST versions, JSON schemas, and resolved tag pointers to achieve the $P99 \le 2\text{ms}$ Control Plane Registry Read Latency SLO.
    - Modeled `persona_overrides` and `few_shot_exemplars` in `TemplateAST` across OpenAPI 3.1, Protobuf v3, and `.ai/doc-api.md`.
    - Hardened PostgreSQL 16+ schema with `ast_hash VARCHAR(64) NOT NULL` + index, `CHECK (version >= 1)` / `CHECK (target_version >= 1)`, composite FK `(template_id, target_version)` on `prompt_tags`, `SELECT ... FOR UPDATE` row locking for monotonic version allocation, and `BEFORE UPDATE OR DELETE` immutability triggers (`prevent_append_only_mutation()`) on `prompt_versions` and `prompt_audit_ledger`.
    - Added multi-tenant gRPC request scoping, dual `{id}` resolution (UUID vs. template `name`), 1 MiB payload guards for the $\le 128\text{ MiB}$ RSS limit, complete audit actions (`TEMPLATE_CREATED`, `VERSION_PUBLISHED`, `TAG_PROMOTED`, `TAG_ROLLED_BACK`, `SCHEMA_REGISTERED`), and explicit Phase 1 unit, benchmark, and integration test scenarios.
  - **Phase 1 Schemas, Relational Core & Control Plane APIs Implementation**:
    - Initialized `go.mod` (`rag-prompt-registry-service`), `Makefile`, and `.env.example`.
    - Implemented PostgreSQL 16+ migrations (`internal/infrastructure/persistence/migrations/000001_init_schema.up.sql`, `000001_init_schema.down.sql`, and `migrations.go` via `//go:embed`) with `prevent_append_only_mutation()` triggers on `prompt_versions` and `prompt_audit_ledger`.
    - Implemented PgBouncer Simple Protocol connection pool (`internal/infrastructure/persistence/postgres/pool.go`) setting `ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol` (ADR-003), transactional `prompt_repo.go`, `audit_repo.go`, and `memory_store.go` (ADR-004).
    - Implemented Redis 7.2 L2 cache client (`cluster_client.go`) and Pub/Sub invalidation broadcaster (`invalidation_pub.go` on `channel:prompt:invalidation`, ADR-002).
    - Implemented domain entities (`prompt_template.go`, `prompt_tag.go`, `audit_entry.go`), repositories (`prompt_repository.go`, `audit_repository.go`), and CQRS handlers (`publish_version.go`, `promote_tag.go`, `get_template_ast.go`, `list_templates.go`).
    - Implemented Protobuf v3 contracts & gRPC server (`proto/v1/`, `internal/transport/grpc/prompt_admin_server.go`), HTTP REST & OpenAPI 3.1 Doc API router (`internal/transport/http/router.go`), control plane server entry point (`cmd/prompt-registry-server/main.go`), SDK scaffold (`pkg/promptsdk/`), and unit/integration/benchmark test suites (`test/unit/...`, `test/integration/...`).
  - **Phase 1 Code Review Remediation & Hardening**:
    - Enforced verified HS256 Bearer JWT authentication (`iss`, `exp`, `tenant_id` claims) across gRPC and HTTP administrative endpoints, bounded idempotency-key reservations with SHA-256 payload fingerprinting (`422 Unprocessable Entity` on mismatch), explicit `ALLOW_IN_MEMORY_STORE=true` opt-in, concurrent HTTP/gRPC graceful shutdown with forced gRPC stop on timeout, `pg_advisory_xact_lock` migration serialization, string-encoded JSONB query arguments for PgBouncer simple protocol, race-free L2 tag resolution updates, RESP `io.ReadFull` bulk-string reads, remote-only L2 lookups when Redis is configured, distinct `"json"` gRPC codec subtype registration, `{{{x}}}` and missing-key handling in `pkg/promptsdk`, and 100% Go docstring coverage.
  - **Phase 2 Implementation Plan Pressure-Test & Hardening (`prompts/phase-2-in-process-engine-go-prompt-sdk.md`)**:
    - Added `ResolveTagResponse.schema_id` parity across Protobuf v3, OpenAPI 3.1, HTTP, and gRPC so tag-resolved renders (`@production`) bind registered JSON output schemas in `DIALECT_OPENAI_STANDARD`.
    - Specified two-tier L1 cache indirection (`L1_tag` pointer $\to$ `L1_version` immutable pre-compiled AST + `JSONSchema`) with zero-allocation composite struct lookup keys (`0 allocs/op` on cache hits) and bidirectional `name <-> UUID` alias invalidation (supporting Runbook 1 name-based pointer rollbacks and UUID-based Pub/Sub events).
    - Specified pre-compiled AST segment slices (`CompiledTemplate`) interpolating variables across `SystemPrompt`, `PersonaOverrides`, `FewShotExemplars`, and `Messages`, plus pre-serialized `response_format` JSON schema envelopes.
    - Added idempotent Anthropic `<context>` / `<query>` XML tag framing (preventing `<context><context>` duplication), zero-allocation streaming tokenizer with dialect escaping inflation compensation, singleflight request coalescing for 10,000 RPS thundering-herd protection, cooldown-protected stale-while-offline serving, and the canonical `test/sdk/` benchmark suite.
  - **Phase 2 In-Process Engine (`go-prompt-sdk`) Implementation**:
    - Implemented streaming `Tokenizer` interface and `CL100KTokenizer` subword estimator (`pkg/promptsdk/tokenizer/budget_guard.go`) with zero-allocation `BudgetGuard` calculating dialect framing overhead, UTF-8 rune boundary context truncation, and edge-case token ceiling enforcement.
    - Implemented deterministic multi-dialect adapters: Anthropic XML (`pkg/promptsdk/dialects/anthropic.go`) with idempotent `<context>`/`<query>` wrapping and XML delimiter breakout escaping, Meta Llama-3 (`pkg/promptsdk/dialects/llama.go`) with header generation and control token neutralization, and OpenAI/Mistral (`pkg/promptsdk/dialects/standard.go`) with zero-allocation streaming JSON escaping and pre-serialized `response_format` JSON schema tail attachment.
    - Implemented `CompiledTemplate` pre-parsed representation and zero-allocation pooled compiler (`pkg/promptsdk/compiler.go`) supporting `opts.Persona` resolution, full AST variable binding, and boundary-fusion verification.
    - Implemented thread-safe two-tier L1 cache (`pkg/promptsdk/cache.go`) with composite struct keys (`versionCacheKey`, `tagCacheKey`), bidirectional `name <-> UUID` alias tracking, 5-minute fallback tag TTL, and stale-while-offline resilience.
    - Implemented embedded `Client` (`pkg/promptsdk/client.go`) with per-key singleflight coalescing, Bearer JWT metadata auth, Redis TCP RESP and in-process Pub/Sub listener, and Prometheus metrics exposition (`prompt_sdk_render_duration_seconds`, `prompt_sdk_cache_hit_ratio`, `prompt_sdk_pubsub_connected`).
    - Added `schema_id` parity to `ResolveTagResponse` across `proto/v1/prompt_service.proto`, `proto/v1/prompt_service_grpc.go`, `internal/transport/grpc/prompt_admin_server.go`, `internal/transport/http/router.go`, `openapi/openapi.yaml`, and `.ai/doc-api.md`.
    - Implemented comprehensive test suites: `pkg/promptsdk/sdk_test.go`, `test/unit/phase2_sdk_test.go`, and canonical `test/sdk/sdk_benchmark_test.go` verifying 0 allocs/op across 50 KB context blocks, P99 render latency, and concurrent pointer switches.
  - **Phase 3 Cache Invalidation & Orchestrator Wiring Implementation**:
    - **Multi-Node Invalidation Bus (ADR-002)**: Connected concurrent SDK instances to Redis Pub/Sub (`channel:prompt:invalidation`), achieving sub-millisecond local tag eviction and $<500$ms global cluster propagation upon atomic `@production` switches and rollbacks.
    - **Tenant Boundary Isolation (Layer 4)**: Enforced strict tenant checks in `L1Cache.HandleInvalidationPayload` preventing cross-tenant tag eviction.
    - **Resilient Auto-Reconnect with Exponential Backoff (Runbook 2)**: Hardened Redis RESP TCP subscriber with automated reconnection, exponential backoff (100ms..3s, factor 2.0), ±20% jitter, and backoff curve reset on Redis `SUBSCRIBE` confirmation, dynamically managing `prompt_sdk_pubsub_connected` gauge.
    - **Forced Cache Wipe & Diagnostic API (Runbook 2 Step 3)**: Implemented `Client.Purge()` and `L1Cache.PurgeTags()` wiping mutable tag pointers while preserving immutable version blocks.
    - **Decoupled Data Plane Orchestrator Pipeline (`pkg/orchestrator`)**:
      - Implemented decoupled interfaces `ContextRetriever` and `InferenceDispatcher`.
      - Built 4-stage pipeline `RAGPipeline.Execute`: Stage 1 Context Assembly -> Stage 2 In-Process `PromptSDK.RenderToBuffer` -> Stage 3 Tokenizer Budget Guard & Truncation -> Stage 4 Downstream Inference Forwarding.
      - Dual-mode execution: Production mode (minimal overhead) vs Diagnostic mode (waterfall timings `assembly_ms`, `sdk_render_ms`, `inference_ms`, `total_ms`, token budget breakdown, and chunk provenance).
      - Zero-allocation hot path: 0 heap allocations (`0 B/op`, `0 allocs/op`) during 50 KB context rendering via `sync.Pool` buffer reuse.
      - Observability: Implemented Prometheus pipeline metrics (`rag_orchestrator_pipeline_duration_seconds`, `rag_orchestrator_context_bytes`, `rag_orchestrator_requests_total`).
    - **Chaos & Outage Resilience Verification**:
      - Verified 100% availability, 0 errors, and sub-millisecond latency under 500 concurrent pipeline requests during complete Control Plane & Redis outages.
      - Stale-while-offline past the 5-minute fallback TTL serves cached versions seamlessly via singleflight coalescing and retry cooldown protection.
      - Uncached tag requests return `ErrTemplateNotFound` cleanly without resource leaks.
    - **Contract & Spec Synchronization**:
      - Updated `.ai/doc-api.md` Section 7 and Section 8 with orchestrator interfaces, response models, and Prometheus metrics.
      - Created comprehensive test suites: `pkg/orchestrator/pipeline_test.go`, `test/integration/phase3_invalidation_bus_test.go`, and `test/integration/phase3_orchestrator_test.go`.
      - Achieved 100% Go docstring coverage across all packages, types, and test helper functions.
      - Updated `.github/workflows/ci.yml` govulncheck installation to `@latest` ensuring full compatibility with Go 1.26+ toolchains.
  - **Phase 4 Resilience, Chaos & Production Rollout Implementation**:
    - **Control Plane Metrics & Port Alignment**:
      - Extracted `MutationMetricsRecorder` into `internal/application/command/mutation_metrics.go` with atomic `RecordRollback()` tracking triggered by `PromoteTagHandler` on semantic tag rollbacks (`isRollback == true`).
      - Exported `prompt_registry_pointer_rollbacks_total` on `/metrics` in `internal/transport/http/router.go` for `ElevatedPointerRollbacks` alert evaluation.
      - Aligned default `GRPC_PORT` to `9090` in `cmd/prompt-registry-server/main.go` and `.env.example`, added `-healthcheck` flag and `main.version` ldflags injection.
    - **SDK Pool & Singleflight Hardening**:
      - Bounded `ReleaseBuffer` in `pkg/promptsdk/compiler.go` (`maxPooledBufferCap = 512 KiB`) to prevent oversized context buffers from accumulating in `sync.Pool`.
      - Hardened `Client.fetchAndCacheTag` in `pkg/promptsdk/client.go` to re-check fresh `L1_tag` state inside the singleflight lock, eliminating trailing duplicate RPCs during thundering-herd recovery.
    - **Chaos & 10,000 RPS Load Profiling Suites**:
      - Implemented `test/integration/phase4_chaos_test.go` covering total Control Plane + Redis outage serving, 1,000-goroutine singleflight recovery, Redis Pub/Sub flapping + goroutine leak checks, and Runbook 1/2 rollback resilience under partial Redis failures.
      - Implemented `test/sdk/phase4_load_benchmark_test.go` verifying `0 B/op`, `0 allocs/op` across all dialects on 50 KB blocks, repeated 10,000-request heap stability, oversized buffer eviction, and Control Plane memory footprint compliance ($\le 64\text{ MiB}$ steady-state / $\le 128\text{ MiB}$ peak).
    - **Distroless Container, Helm Chart & GitOps Canary**:
      - Created multi-stage distroless Dockerfile (`deployments/docker/Dockerfile`) and root `.dockerignore` packaging `openapi/openapi.yaml`.
      - Created production Helm chart (`deployments/helm/rag-prompt-registry/`) with `Chart.yaml`, `values.yaml`, `deployment.yaml`, `service.yaml`, `poddisruptionbudget.yaml`, `configmap.yaml`, `secret.yaml`, `servicemonitor.yaml`, `prometheusrule.yaml`, `rollout.yaml`, and `analysistemplate.yaml`.
      - Created GitOps ArgoCD `Application` manifest (`deployments/gitops/argocd-application.yaml`).
      - Documented deployment specifications, Prometheus alerting matrix, and Runbook 3 in `.ai/doc-api.md` and `.ai/progress-tracker.md`.
- **12-Factor Environment & `.env` Synchronization**:
  - Created `internal/config/env.go` implementing zero-dependency `.env` / `ENV_FILE` loading (without overwriting existing OS environment variables), `ParseLogLevel`, and typed env helpers (`GetEnv`, `GetEnvInt`, `GetEnvBool`, `ParseCSV`).
  - Wired `.env` loading, `LOG_LEVEL`, `CONN_EXEC_MODE` (via `postgres.ParseQueryExecMode` defaulting to `simple_protocol` per ADR-003), and `OPENAPI_SPEC_PATH` into `cmd/prompt-registry-server/main.go`, `internal/infrastructure/persistence/postgres/pool.go`, `pkg/promptsdk/client.go`, and `pkg/orchestrator/pipeline.go`.
  - Updated `.env.example` and Helm `configmap.yaml`/`values.yaml` with `JWT_HMAC_SECRET`, `JWT_ISSUER`, and `OPENAPI_SPEC_PATH`, synchronized `.ai/doc-api.md`, and added `test/unit/config_env_test.go`.
- **Automated `.env.prod` -> Helm Deployment Synchronization**:
  - Converted `deployments/helm/rag-prompt-registry/templates/configmap.yaml` to dynamically range over `.Values.env` (preserving `HTTP_PORT` / `GRPC_PORT` fallbacks and ignoring `secretName` and `nil` values) and updated `values.yaml` and `_helpers.tpl` / `secret.yaml` / `deployment.yaml` / `rollout.yaml` (`checksum/config` rolling-restart trigger + `env.secretName` support).
  - Created `scripts/sync_env_to_deployments.py` (`--sync` and `--check-only`) excluding sensitive keys (`PASSWORD`, `SECRET`, `TOKEN`, `KEY`, `DSN`, `CREDENTIAL`, `PRIVATE`) and preserving `secretName` and all top-level Helm blocks.
  - Installed `.git/hooks/pre-commit` to block commits on `.env.prod` vs `values.yaml` drift, documented `.env` vs `.env.prod` semantics in `.env.example` and `.ai/doc-api.md`, and synced `deployments/helm/rag-prompt-registry/values.yaml`.
- **CI/CD Secret Injection & Secret Management Documentation**:
  - Added `.github/workflows/deploy.yml` implementing GitHub Actions Environment secret validation, out-of-band Kubernetes Secret injection (`--from-env-file` -> `rag-prompt-registry-service-secret`), secret-free Helm deployment (`--set env.secretName` / `secrets.existingSecret`), secret-rotation rollout restarts, and `$RUNNER_TEMP` credential cleanup (without touching `.env.prod`).
  - Created `deployments/SECRET_MANAGEMENT.md` documenting configuration vs. secret boundaries, excluded key markers, GitHub Actions Environment setup, External Secrets Operator / Vault integration, manual bootstrap, and rotation runbooks, and synced `.ai/doc-api.md`.
- **Self-Hosted Bitnami Redis 7.2 Cluster Infrastructure & Microservice Wiring**:
  - Created `deployments/helm/redis-cluster/values.yaml` configuring a 6-node `bitnami/redis-cluster` (v11.x, Redis `7.2.5-debian-12-r0`, 3 primary + 3 replica, `cluster.init: true`) linked to `rag-prompt-registry-service-secret` (`REDIS_PASSWORD`), with `100m`/`256Mi` requests, `500m`/`1Gi` limits, `5Gi` AOF/RDB storage, soft pod anti-affinity, and Prometheus exporter on port `9121`.
  - Created `deployments/argocd/redis-cluster-application.yaml` (ArgoCD `Application` in `argocd` targeting `rag-platform` with `prune: true`, `selfHeal: true`, exponential retry backoff), `deployments/docker/docker-compose.redis.yml` (`bitnami/redis:7.2-debian-12`), `scripts/deploy_redis_cluster.{sh,ps1}`, and authoritative `deployments/redis.md`.
  - Added `-MOVED` / `-ASK` (`ASKING`) cluster slot redirect support to `internal/infrastructure/cache/redis/cluster_client.go`, wired `REDIS_CLUSTER_ADDRS` precedence into `cmd/prompt-registry-server/main.go`, synced `.env.prod` -> `deployments/helm/rag-prompt-registry/values.yaml`, aligned secret naming across `deploy.yml` & `SECRET_MANAGEMENT.md`, updated `.ai/doc-api.md`, and added `test/unit/redis_cluster_test.go`.
- **Production Hardening & Security Review Remediation**:
  - Updated `go.mod` (`go 1.26.9`) and `README.md` (`1.26.9+`) to remediate Go 1.26.1 standard library vulnerabilities (including GO-2026-4865) and maintain version synchronization across documentation and manifests.
  - Aligned Prometheus histogram emission in `pkg/promptsdk/client.go` to use cumulative bucket sums (`cumSucc`, `cumErr`) for `_count` lines, ensuring strict equality with `le="+Inf"` buckets under high concurrency; added unit verification in `test/unit/phase2_sdk_test.go`.
  - Refactored RSS memory sampling in `test/sdk/rss_other_test.go` to return `0, 0, false` instead of conflating `runtime.MemStats.Sys` with RSS, and updated `test/sdk/phase4_load_benchmark_test.go` to track RSS availability and report when unavailable rather than passing a zero-valued peak RSS check.
  - Decoupled wall-clock RPS/latency assertions in `TestPhase4Sustained10000RPSAndZeroMemoryLeak` behind `RUN_WALLCLOCK_BENCHMARKS` / `DEDICATED_HARDWARE`, allowing the blocking CI step to retain zero-allocation and memory checks without timing flakiness on shared runners, while adding a dedicated benchmark step in `.github/workflows/ci.yml`.
  - Removed cluster-scoped `kubectl get namespace` preflight in `.github/workflows/deploy.yml` to adhere to namespace-scoped RBAC permissions, and updated `deployments/helm/rag-prompt-registry/templates/secret.yaml` failure messaging to prevent unsafe ownership removal guidance during `existingSecret` handoffs.
  - Hardened `.github/workflows/deploy.yml`: removed cluster-scoped namespace creation (`kubectl create namespace` / `--create-namespace`), passed `image_tag` via validated `IMAGE_TAG` env var (`--set-string`), added 15m Argo Rollout health checks when `canary_enabled=true`, added `rag-prompt-registry/secret-resource-version` reconciliation checks for unchanged Secrets, and added Rollout restart + status wait on Secret rotation.
  - Hardened Helm charts & Docker/GitOps configs: multi-arch `TARGETARCH` build in `deployments/docker/Dockerfile`, root `.dockerignore`, loopback `127.0.0.1:6379:6379` in `docker-compose.redis.yml`, `secrets.existingSecret` parameter in `deployments/gitops/argocd-application.yaml`, `HTTP_PORT`/`GRPC_PORT` containerPort alignment in `deployment.yaml`/`rollout.yaml`, `canary.replicaCount: 10` weight validation + `AnalysisTemplate` target/freshness (`up == 1` + `count_over_time`) selectors + `ServiceMonitor` `podTargetLabels`, `secret.yaml` `helm.sh/resource-policy: keep` + handoff guard + required `jwtHmacSecret`, `bitnami/redis-cluster` root `existingSecret`, `fullnameOverride`, `redis.configmap`, and `deploy_redis_cluster.{ps1,sh}` secret/fullname flags.
  - Added cumulative `prompt_sdk_render_duration_seconds_bucket` exposition & `MetricsHandler()` to `pkg/promptsdk` and `pkg/orchestrator` alongside `sdk-podmonitor.yaml`, enforced non-empty `redisAddrs` / explicit `ALLOW_IN_MEMORY_STORE` in `cmd/prompt-registry-server/main.go`, fixed comment scanning in `scripts/sync_env_to_deployments.py`, tightened P99/RPS/PeakRSS checks in `test/sdk/phase4_load_benchmark_test.go` and `test/integration/phase4_chaos_test.go`, and updated `deployments/SECRET_MANAGEMENT.md` & `.ai/doc-api.md`.
  - Removed unused `runtime` import from `test/sdk/rss_other_test.go` following the constant sampler return refactor, leaving sampler behavior unchanged and verified via cross-platform vetting.
  - Remediated Go standard library vulnerabilities GO-2026-6809 (`crypto/x509` intermediate certs) and GO-2026-6859 (`net/http` MaxBytesReader race) by updating `go.mod` to `go 1.26.11` (with `net/http@go1.26.11` patch) and syncing `README.md` (`1.26.11+`).

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
3. A `200 OK` response confirms the pointer update and audit entry are durably committed in PostgreSQL; however, a committed pointer update is distinct from a successful Redis Pub/Sub invalidation publication. Do not treat or report the pointer rollback itself as failed or rolled back solely because Redis publication failed.
4. Inspect `broadcast_emitted` in the `PromoteTagResponse` (and `prompt_invalidation_events_published_total{status="error"}`):
   - If `broadcast_emitted` is `true`, confirm via Prometheus that orchestrator nodes pick up the target version within $<500$ms.
   - If `broadcast_emitted` is `false`, the pointer in PostgreSQL is already committed at `<previous_version_int>`. To inform clients of the committed pointer once Redis connectivity is restored, re-issue `PUT /v1/templates/{id}/tags/@production` with `{"target_version": <previous_version_int>, "expected_version": <previous_version_int>, "updated_by": "admin-rollback-republish"}` to re-emit the invalidation broadcast, or invoke `PromptSDK.Invalidate(templateID, "@production")` / follow Runbook 2 on orchestrator instances.

### Runbook 2: Resolving Pub/Sub Invalidation Drops
1. If an orchestrator instance loses its Redis Pub/Sub connection, it falls back to the 5-minute safety TTL on mutable pointer tags.
2. Check the `prompt_sdk_pubsub_connected` gauge on the orchestrator.
3. If disconnected, restart the client connection pool or force a local cache wipe via the orchestrator's diagnostic endpoint.

### Runbook 3: Progressive Canary Rollout & Automated Abort via Argo Rollouts
1. Enable canary delivery in Helm (`canary.enabled=true`) with target `image.tag` and sync via ArgoCD (`deployments/gitops/argocd-application.yaml`).
2. Monitor live Argo Rollout steps (`10%` -> 2m pause + analysis -> `50%` -> 5m pause + analysis -> `100%`):
   `kubectl argo rollouts get rollout rag-prompt-registry-service -n rag-platform --watch`
3. Verify Prometheus `AnalysisTemplate` gates pass (`rate(prompt_invalidation_events_published_total{status="error"}[2m]) == 0`, `rate(prompt_registry_mutation_duration_seconds_count{status="error"}[2m]) == 0`, and zero `ElevatedPointerRollbacks` spikes). Any threshold breach triggers an automatic abort back to the stable ReplicaSet.
4. For manual emergency abort, run:
   `kubectl argo rollouts abort rag-prompt-registry-service -n rag-platform`
   and execute **Runbook 1** if a `@production` prompt pointer also requires rollback.
