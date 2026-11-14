# Architecture Reference: RAG Prompt Registry Service

## 1. System Overview & Decoupled Topology (ADR-001)

The **`rag-prompt-registry-service`** architecture separates administrative lifecycle governance from runtime context compilation. In large-scale RAG systems, transmitting 50 KB context passages across network boundaries for string concatenation creates severe network saturation and serialization overhead. 

To eliminate this bottleneck, the platform implements a decoupled two-plane design:

```
+-----------------------------------------------------------------------------+
| CONTROL PLANE: rag-prompt-registry-service (Go Daemon)                      |
|                                                                             |
|  - Manages templates, immutable versions (v1..vn), and JSON output schemas |
|  - Controls mutable semantic pointer tags (@production, @staging, @canary)  |
|  - Direct atomic transactional audit ledger in PostgreSQL (ADR-004)         |
|  - Broadcasts tag pointer mutations to Redis Pub/Sub (ADR-002)              |
+-----------------------------------------------------------------------------+
                                      |
                     Pub/Sub Pointer Invalidation Bus (< 100 ms)
                                      v
+-----------------------------------------------------------------------------+
| DATA PLANE: go-prompt-sdk (Embedded Go Library in Calling Orchestrators)    |
|                                                                             |
|  - Two-tier in-process memory cache (L1_version: unbounded, L1_tag: TTL/bus)|
|  - Zero-allocation string interpolation via sync.Pool byte buffers          |
|  - SWAR & SIMD-accelerated dialect framing (Anthropic XML, Llama-3, OpenAI) |
|  - Tokenizer safety guard calculating structural overhead before injection  |
|  - 100% resilient serving during control plane or database outages          |
+-----------------------------------------------------------------------------+
```

---

## 2. 5-Layer Backend Architecture

Aligned with production-grade backend engineering concepts:

```
[ Layer 1: API & Routing ]
   - HTTP/REST (:8080) & gRPC (:9090)
   - Tenant isolation, rate limiting, and optimistic tag promotion
            │
            ▼
[ Layer 2: Application / Domain Logic ]
   - Monotonic version sequencing (SELECT ... FOR UPDATE)
   - Semantic tag pointer governance with expected_version preconditions
   - Canonical AST hashing (SHA-256) & JSON schema validation
            │
            ▼
[ Layer 3: Persistence & State (PostgreSQL + PgBouncer) ]
   - Append-only immutable tables: prompt_versions, prompt_audit_ledger
   - Mutable pointer table: prompt_tags
   - Simple Protocol mode for PgBouncer compatibility (ADR-003)
            │
            ▼
[ Layer 4: Distributed Cache & Invalidation Bus (Redis 7.2 Cluster) ]
   - L2 Cache for raw ASTs and schemas
   - Pub/Sub invalidation bus (channel:prompt:invalidation)
            │
            ▼
[ Layer 5: In-Process Data Plane SDK (pkg/promptsdk) ]
   - Two-tier L1 memory cache (L1_version and L1_tag)
   - Zero-allocation compiler using sync.Pool byte buffers
   - SWAR bit manipulation and SIMD IndexByte dialect compilation
```

---

## 3. Architectural Decision Records (ADRs)

### ADR-001: Decoupled Control Plane vs. In-Process Data Plane SDK
- **Context**: RAG orchestrators retrieve 50 KB context chunks and compile prompts at 10,000+ RPS. Sending context blocks over gRPC to a centralized service for string interpolation causes network bandwidth saturation, high tail latencies, and unnecessary serialization overhead.
- **Decision**: Centralize administrative governance (CRUD, RBAC, schema contracts) in the Go daemon `rag-prompt-registry-service`. Delegate runtime variable binding, context formatting, and dialect adaptation to an embedded client library (`pkg/promptsdk`).
- **Consequences**: Zero network overhead for context concatenation; hot path execution runs in $\le 0.5$ ms P99.

### ADR-002: Distributed Instant Cache Coherence via Redis Pub/Sub
- **Context**: Pointer tags (`@production`, `@staging`) mutate when new prompt versions are promoted or rolled back. Calling orchestrators must not serve stale prompts.
- **Decision**: When a tag pointer promotion commits in PostgreSQL, the control plane publishes an eviction event containing `tenant_id`, `template_id`, `tag`, and `new_version` to Redis Pub/Sub (`channel:prompt:invalidation`). All running SDK instances listen in the background and evict their local tag cache.
- **Consequences**: Sub-second global cache coherence ($<100$ ms typical, $<500$ ms SLO) across distributed orchestrators while maintaining unbounded TTL for immutable versions.

### ADR-003: PgBouncer Simple Protocol Mode
- **Context**: High-concurrency relational access in Kubernetes relies on PgBouncer in transaction pooling mode. Extended protocol prepared statements (`PREPARE` / `EXECUTE`) fail in transaction pooling mode when successive statements execute across different backend connections.
- **Decision**: Configure PostgreSQL connection pooling using `jackc/pgx/v5` with:
  ```go
  connConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
  ```
- **Consequences**: Complete compatibility with PgBouncer transaction pooling without prepared statement name collisions.

### ADR-004: Transactional Internal Audit Ledger in PostgreSQL
- **Context**: Compliance and prompt governance require audit logging of all template modifications, version publications, and tag promotions. External CDC pipelines (Debezium + Kafka Connect) introduce significant operational complexity.
- **Decision**: Template mutations and `prompt_audit_ledger` entries commit atomically in the exact same PostgreSQL transaction using database triggers preventing updates or deletions.
- **Consequences**: Strict ACID compliance and zero external streaming infrastructure requirements for audit logging.

---

## 4. Zero-Allocation Compiler & In-Process Compilation Mechanics

The in-process compiler (`pkg/promptsdk/compiler.go`) achieves $P99 \le 0.5$ ms across 50 KB context blocks with **0 heap allocations on the hot path**:

1. **Global Buffer Pooling (`sync.Pool`)**:
   - Byte buffers are acquired via `promptsdk.AcquireBuffer()` and returned via `promptsdk.ReleaseBuffer(buf)`.
   - Buffers are reset before returning to the pool, preventing memory leaks and avoiding GC heap escapes.
2. **AVX2/SSE2 SIMD & SWAR Bit Scanning**:
   - **Meta Llama-3 (`pkg/promptsdk/dialects/llama.go`)**: Scans for control delimiters (`<|...|>`) using Go's AVX2/SSE2 assembly `strings.IndexByte(s, '<')`, processing 50 KB in $\approx 4.5$ µs (12x faster than scalar byte loops).
   - **Anthropic XML (`pkg/promptsdk/dialects/anthropic.go`)**: Scans 8 bytes per iteration using SWAR (SIMD Within A Register) bit manipulation to detect `&`, `<`, `>` in parallel:
     ```go
     w := uint64(s[i]) | ... | uint64(s[i+7])<<56
     xAmp := w ^ 0x2626262626262626
     xLt  := w ^ 0x3c3c3c3c3c3c3c3c
     xGt  := w ^ 0x3e3e3e3e3e3e3e3e
     if (((xAmp-0x0101010101010101)&^xAmp |
          (xLt -0x0101010101010101)&^xLt |
          (xGt -0x0101010101010101)&^xGt) & 0x8080808080808080) == 0 {
         i += 8 // Clean 8-byte fast-path jump
         continue
     }
     ```
   - **OpenAI Standard (`pkg/promptsdk/dialects/standard.go`)**: 8-byte SWAR scanning for `"`, `\`, and control characters $< \text{0x20}$ for JSON string escaping.
3. **Tokenizer Budget Safety Guard (`tokenizer.BudgetGuard`)**:
   - Calculates the structural byte and token overhead of vendor framing before context injection.
   - Truncates context at token boundaries if total payload exceeds model ceiling (`MaxTokens`).

---

## 5. Relational Schema & Immutability Guarantees

PostgreSQL 16+ schema managed by version-controlled migrations in `internal/infrastructure/persistence/migrations/`:

```sql
-- Core template aggregate
CREATE TABLE prompt_templates (
    template_id   VARCHAR(64) PRIMARY KEY,
    tenant_id     VARCHAR(64) NOT NULL,
    template_name VARCHAR(128) NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Strict monotonic, append-only versions (v1..vn)
CREATE TABLE prompt_versions (
    template_id   VARCHAR(64) NOT NULL REFERENCES prompt_templates(template_id) ON DELETE CASCADE,
    version       INT NOT NULL,
    schema_id     VARCHAR(64) NOT NULL,
    ast_json      JSONB NOT NULL,
    ast_hash      VARCHAR(64) NOT NULL,
    published_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (template_id, version)
);

-- Mutable semantic pointer tags (@production, @staging, @canary)
CREATE TABLE prompt_tags (
    tenant_id      VARCHAR(64) NOT NULL,
    template_id    VARCHAR(64) NOT NULL,
    tag            VARCHAR(64) NOT NULL,
    target_version INT NOT NULL,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, template_id, tag),
    FOREIGN KEY (template_id, target_version) REFERENCES prompt_versions(template_id, version)
);

-- Immutable audit compliance log
CREATE TABLE prompt_audit_ledger (
    audit_id    BIGSERIAL PRIMARY KEY,
    tenant_id   VARCHAR(64) NOT NULL,
    template_id VARCHAR(64) NOT NULL,
    action      VARCHAR(32) NOT NULL,
    version     INT,
    tag         VARCHAR(64),
    performed_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

**Database Immutability Triggers**: `BEFORE UPDATE OR DELETE` triggers on `prompt_versions` and `prompt_audit_ledger` strictly raise exceptions to prevent history modification.

---

## 6. Two-Tier Caching & Resilient Offline Serving

```
Caller -> L1_version Cache (Memory, Unbounded TTL) -> Hit: Return AST (0 allocs)
       -> L1_tag Cache (Memory, 5m fallback TTL)   -> Hit: Return Pointer
       -> On Pointer Miss: Fetch from Control Plane (gRPC / Redis L2 / Postgres)
       -> Redis Pub/Sub Event: Immediately evict mutated tag pointer from L1_tag
```

- **Stale-While-Offline Resilience**: If the control plane or Redis cluster experiences an outage, `pkg/promptsdk` continues serving the last-known-good cached template versions and tag pointers from local memory, guaranteeing 100% data plane uptime for calling orchestrators.

---

## 7. Secured Logging Invariant

All logging uses structured JSON (`log/slog`). Under no circumstances are raw prompt templates, raw context passages, customer secrets, authorization tokens, or database credentials emitted to log sinks. Logs emit strictly operational metadata (`trace_id`, `tenant_id`, `template_id`, `version`, `tag`, `latency_ms`, and truncated SHA-256 digests).

