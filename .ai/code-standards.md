# Code Standards - RAG Prompt Registry Service

## General Principles

- **Single Responsibility**: Strictly decouple the Control Plane administrative daemon (`cmd/prompt-registry-server`) from the Data Plane embedded compiler (`pkg/promptsdk`).
- **Zero-Allocation Hot Path**: Runtime variable interpolation and dialect compilation must never allocate on the heap; use pooled byte buffers (`sync.Pool`) for 50 KB context blocks.
- **Fail-Fast on Corrupt Input**: Validate AST nodes, variable keys, and JSON schema drafts before persisting to PostgreSQL.
- **Root-Cause Focus**: Never patch race conditions or cache desynchronization with arbitrary sleep loops; rely on atomic Redis Pub/Sub events and transactional PostgreSQL commits.

---

## Go Code Quality Standards

- **Runtime Target**: Go 1.22+ (64-bit).
- **Formatting**: Strict standard formatting via `gofmt -w -s .`.
- **Linter**: `golangci-lint` is authoritative:
  - Enabled linters: `govet`, `errcheck`, `staticcheck`, `unused`, `gosec`, `ineffassign`, `revive`.
  - Zero linter warnings or errors permitted in production builds.
- **Error Handling**:
  - Always handle errors explicitly. Never ignore returned errors with `_`.
  - Wrap internal errors with context using `fmt.Errorf("failed to ...: %w", err)`.
  - Do not leak internal database connection strings, credentials, or SQL syntax traces in external API responses.
- **Package Encapsulation**:
  - `internal/` packages are private to the control plane binary.
  - `pkg/promptsdk/` is public, clean, and reusable by external orchestrators (`rag-search-orchestrator`) with minimal external dependencies.

---

## Storage & Connectivity Invariants

1. **PgBouncer Simple Protocol Compatibility (ADR-003)**:
   - When configuring `jackc/pgx/v5` connection pools in `internal/infrastructure/persistence/postgres/pool.go`, explicitly configure `ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol`.
   - Prepared statements must be disabled across PgBouncer transaction pooling mode to prevent "prepared statement does not exist" failures during high connection turnover.
2. **Transactional Compliance Ledger (ADR-004)**:
   - Every mutation to `prompt_versions` or `prompt_tags` must atomically insert a corresponding audit record into `prompt_audit_ledger` inside the exact same database transaction (`tx.Commit()`).
3. **Redis Pub/Sub Invalidation Engine**:
   - Tag pointer promotions must publish invalidation messages to Redis Pub/Sub (`channel:prompt:invalidation`) immediately following transaction commit.
   - If Redis publish fails, log an error with high-severity alert metric `prompt_invalidation_events_published_total{status="error"}`.

---

## Data Plane Compiler & Buffer Pool Invariants

1. **Byte Buffer Pooling (`sync.Pool`)**:
   - The string compiler in `pkg/promptsdk/compiler.go` must draw byte buffers from a global `sync.Pool`.
   - Buffers must be reset (`buf.Reset()`) before being returned to the pool via `defer pool.Put(buf)`.
   - Never retain references to pooled buffer memory across asynchronous boundaries.
2. **Tokenizer Budget Safety Guard**:
   - Calculate dialect structural tag overhead (e.g. `<context>` tags or Llama special tokens) using the tokenizer helper before inserting user context to prevent exceeding model context windows.

---

## Secured Logging Invariants

- **Structured JSON Logging**: All logs must be emitted as machine-parseable JSON lines using Go's standard `log/slog` library configured with `slog.NewJSONHandler`.
- **Mandatory Correlation IDs**: Every log entry must include:
  - `trace_id`
  - `tenant_id`
  - `template_id`
  - `version` (or `tag`)
- **Strict Redaction**:
  - **Never log raw prompt templates, raw context passages, customer secrets, authorization headers, or database credentials**.
  - Emit only lengths, truncated hashes (`template_hash = sha256(ast)[:16]`), or status codes.
  - Error logs must sanitize database connection URIs.

---

## Doc API Mandatory Maintenance

- **Continuous Synchronization**: Whenever any struct, endpoint, gRPC message, or database table is created or altered, [`.ai/doc-api.md`](doc-api.md) and OpenAPI 3.1 specifications (`openapi/openapi.yaml`) **MUST** be updated immediately in the same change step, no matter how small the code modification is.

---

## Git Commit Standards

- **Git Commit Frequency**: Commit code as you build. Every working session or phase must contain **at least two git commits**.
- **Conventional Commit Patterns**: All git commit messages must strictly adhere to the following prefixes:
  - `feat:` for new features 
  - `fix:` for bug fixes
  - `refactor:` for code refactoring
  - `docs:` for documentation updates
  - `tests:` for tests addition and modification
  - `chore:` for maintenance tasks and environment setup
