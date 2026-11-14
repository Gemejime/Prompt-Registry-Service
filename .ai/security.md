# Security Context - RAG Prompt Registry Service

## Threat Model & Trust Boundaries

- **Untrusted Template Payloads**: Prompt templates submitted via administrative APIs may contain prompt injection markers, unescaped dialect tags, or malformed JSON schema drafts. All inputs must be structurally parsed and sanitized before storage.
- **Unauthorized Tag Promotion**: Modifying `@production` or `@canary` tags changes live behavior across all calling orchestrators. Strict authorization (`tag:promote`) and tenant scoping must be verified before executing tag mutations.
- **Context Injection Boundary**: Runtime context payloads bound by `pkg/promptsdk` are untrusted user/document data. Dialect adapters (Anthropic XML, Llama-3 special headers) must sanitize and escape boundary markers to prevent delimiter-escape attacks.
- **Database & Cache Security**: PostgreSQL 16+ connections and Redis 7.2 Cluster communication must use TLS/mTLS with strict password authentication within private VPC networks.
- **Container Hardening**: Deployments use Google Container Distroless (`gcr.io/distroless/static-debian12:nonroot`) with UID 65532 (`runAsNonRoot: true`), read-only root filesystem (`readOnlyRootFilesystem: true`), and dropped Linux capabilities (`drop: ["ALL"]`).

---

## Secured Logging Invariants

1. **Strict Content Redaction**:
   - Prompt templates and runtime context containers contain proprietary IP, customer queries, and sensitive enterprise context.
   - **Raw prompt template strings and raw context passages must NEVER be logged to stdout or external collectors**.
2. **Secrets & Credentials Masking**:
   - PostgreSQL connection strings, Redis auth passwords, JWT signing keys, and authorization bearer tokens must be stripped or redacted from all log handlers.
3. **Structured Machine-Readable Output**:
   - All logs must be emitted as machine-parseable JSON lines via `log/slog`.
   - Log entries contain only operational identifiers (`tenant_id`, `template_id`, `version`, `tag`, `latency_ms`, `error_code`).
   - For auditing changes, log only the truncated SHA-256 hash of the template AST (`template_hash = sha256(ast)[:16]`).

---

## Tenant Boundary Isolation

- Multi-tenancy is strictly enforced across all database tables (`prompt_templates`, `json_schemas`) using composite unique constraints on `(tenant_id, name)`.
- All database queries must include `tenant_id` in `WHERE` clauses. Cross-tenant reads or pointer promotions are forbidden.

---

## Secrets & Configuration Management

- Environment variables and credentials must never be committed to source control.
- Configuration is loaded via 12-factor environment variables with sensible local defaults in `.env.example`.
- Production credentials are injected via Kubernetes Secrets or HashiCorp Vault sidecars.

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
