# AI Workflow Rules - RAG Prompt Registry Service

## Development Approach

Build this microservice incrementally following a spec-driven workflow:
- The context files in `.ai/` (distilled from reference blueprints in `tasks/` when present locally) define system invariants, data structures, and operational boundaries.
- Adhere strictly to the decoupled architecture: Control Plane (`rag-prompt-registry-service` daemon in Go) managing immutable versions and pointers, and Data Plane (`go-prompt-sdk` in `pkg/promptsdk`) executing zero-allocation string interpolation in-process.
- Implement against these specs without inferring undocumented dependencies or deviating from architectural decision records (ADR-001 through ADR-004).

---

## Human-in-the-Loop Rules

- **Plan First**: Propose an implementation plan or prompt in `prompts/<task-name>.md` before making substantial code changes, and await human confirmation.
- **Ambiguity Escalation**: If dialect syntax, database schemas, or SDK interface boundaries are ambiguous, prompt for clarification before proceeding.
- **Destructive Action Guards**: Always request human confirmation before running destructive operations, table drops, or major schema mutations.
- **Demonstrate & Verify**: Provide clear verification steps, unit test output, benchmark results, and linting checks after completing a unit.

---

## Non-Negotiable Invariants

1. **Continuous Doc API & OpenAPI Maintenance**: The [`.ai/doc-api.md`](doc-api.md) file and OpenAPI 3.1 specifications (`openapi/openapi.yaml`) MUST be updated on **every code change or addition**, regardless of how small the modification is.
2. **Secured Logging**: All logs emitted by servers, SDKs, or utilities must be structured JSON (`log/slog`). Strictly redact raw prompt templates, raw context passages, customer secrets, authorization tokens, and database credentials before emission.
3. **Decoupled Architecture (ADR-001)**: The registry service handles metadata and pointers over low-frequency APIs. String interpolation and dialect compilation run in-process within calling services via `go-prompt-sdk`. Never transmit raw context blocks over gRPC for string concatenation.
4. **Zero-Allocation In-Process Interpolation**: String interpolation hot path must use `sync.Pool` byte buffers yielding 0 heap escapes and $P99 \le 0.5$ms across 50 KB context blocks.
5. **Instant Invalidation Bus (ADR-002)**: Semantic pointer mutations broadcast immediately across Redis Pub/Sub (`channel:prompt:invalidation`), evicting SDK in-memory cache keys within $<100$ms ($<500$ms global SLO).
6. **PgBouncer Simple Protocol Compatibility (ADR-003)**: Relational access via `jackc/pgx/v5` must configure `ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol` to avoid prepared statement failures in transaction pooling mode.
7. **Transactional Internal Audit Ledger (ADR-004)**: Administrative changes write directly to `prompt_audit_ledger` inside the same database transaction.
8. **Git Commit Standards**:
   - **Git Commit Frequency**: Commit code as you build. Every working session or phase must contain **at least two git commits**.
   - **Conventional Commit Patterns**: All git commit messages must strictly adhere to the following prefixes:
     - `feat:` for new features 
     - `fix:` for bug fixes
     - `refactor:` for code refactoring
     - `docs:` for documentation updates
     - `tests:` for tests addition and modification
     - `chore:` for maintenance tasks and environment setup
9. **Quality Gates**: Go formatting (`gofmt`), static analysis (`go vet`, `golangci-lint`), and test suites must pass cleanly before any commit.

---

## Pre-Commit Verification Checklist

Before committing code, submitting changes, or marking a task complete:

1. **Formatting**: Run `gofmt -w -s .` (zero diff).
2. **Lint & Vet**: Once `go.mod` exists, run `go vet ./...` and `golangci-lint run` (zero errors or warnings; skip until the Go module exists, consistent with CI).
3. **Automated Tests & Benchmarks**: Once `go.mod` exists, ensure unit tests (`test/unit/...`), integration tests (`test/integration/...`), and SDK benchmarks (`go test -bench=. -benchmem ./pkg/promptsdk/...`) pass cleanly (skip until the Go module exists, consistent with CI).
4. **Secured Logging Audit**: Verify no raw prompt templates, context spans, secrets, or passwords are leaked in logging statements.
5. **Doc API & OpenAPI Sync**: Confirm [`.ai/doc-api.md`](doc-api.md) and `openapi/openapi.yaml` reflect any new or modified structs, endpoints, parameters, or database schemas.
6. **Progress Tracker**: Update [`.ai/progress-tracker.md`](progress-tracker.md) with session notes and completed items.
