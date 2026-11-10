# Implementation Plan: Phase 2 — In-Process Engine (`go-prompt-sdk`)

## 1. Objective & Scope

Implement **Phase 2: In-Process Engine (`go-prompt-sdk`)** of the `rag-prompt-registry-service` in strict alignment with `GEMINI.md`, `.ai/` normative specifications (`project-overview.md`, `architecture.md`, `doc-api.md`, `code-standards.md`, `security.md`, `ai-workflow-rules.md`), and the reference blueprints in `tasks/` (`RAG Prompt Registry Service Plan.pdf`, `Disaggregated Hybrid Model Architecture.pdf`, `Production Grade Backend Concepts.pdf`, and `Production-Grade Multimodal RAG Architectural Specification.pdf`):

1. **Zero-Allocation String Interpolation & Pre-Compiled AST Engine (`pkg/promptsdk/compiler.go` & `pkg/promptsdk/doc.go`)**:
   - Expand the `sync.Pool` byte buffer allocator (`renderBufferPool`, 64 KiB initial capacity) and zero-allocation interpolation engine in `pkg/promptsdk/compiler.go` while maintaining full backward compatibility with `pkg/promptsdk/doc.go` and `.ai/doc-api.md` Section 7.
   - **Pre-Compiled AST Representation (`CompiledTemplate`)**:
     - When an immutable template version is loaded into L1 cache, pre-parse `SystemPrompt`, `PersonaOverrides`, `FewShotExemplars`, and `Messages` into flat, immutable segment slices (`[]compiledSegment`, distinguishing static literal spans from `{{variable}}` bindings) and pre-serialize the `DIALECT_OPENAI_STANDARD` `response_format` JSON schema tail (`{"type":"json_schema","json_schema":{"name":...,"strict":true,"schema":...}}`).
     - Eliminates repeated template pattern scanning and JSON schema marshaling on the hot path.
   - **Full-AST Variable Binding & Persona Selection**:
     - Interpolate runtime `opts.Variables` across all four AST sections (`SystemPrompt`, active `PersonaOverrides[opts.Persona]`, `FewShotExemplars`, and `Messages`, supporting both retrieved 50 KB parent context containers and multi-turn conversation history variables).
     - Resolve `opts.Persona` against `TemplateAST.PersonaOverrides[opts.Persona]` when specified, falling back deterministically to `TemplateAST.SystemPrompt`.
   - **Zero-Allocation Hot Path (`0 allocs/op`, $P99 \le 0.5\text{ms}$)**:
     - Provide `Render(ctx, templateID, tagOrVersion, opts) ([]byte, error)` and zero-allocation pooled rendering `RenderToBuffer(ctx, dst, templateID, tagOrVersion, opts) error` / `CompileIntoBuffer(dst, compiled, opts, guard) error` so callers (`rag-search-orchestrator`) and benchmarks execute 50 KB context renders with **0 heap allocations (`0 allocs/op`)** and **$P99 \le 0.5\text{ms}$**.

2. **Deterministic Multi-Dialect AST Adapters (`pkg/promptsdk/dialects/`)**:
   - **`pkg/promptsdk/dialects/anthropic.go` (`DIALECT_ANTHROPIC_XML`)**:
     - Wraps the system prompt (or active persona override) inside `<instructions>...</instructions>`, optional few-shot exemplars inside `<examples>\n<example role="...">...</example>\n</examples>`, and message blocks with structured XML tags (`<context>...</context>`, `<query>...</query>`).
     - **Idempotent XML Tag Framing**: Detects whether a template's `content_pattern` already encloses `{{context}}` in `<context>...</context>` or `{{query}}` in `<query>...</query>` (as in `.ai/doc-api.md` examples) vs. dialect-neutral patterns (`{{context}}`), guaranteeing valid `<instructions>` and `<context>` XML tags in the output without emitting duplicate `<context><context>` wrappers.
     - **Context Injection Boundary Guard (`.ai/security.md`)**: Sanitizes and escapes untrusted runtime variable values directly into the pooled `*bytes.Buffer` with 0 heap allocations (escaping `&`, `<`, `>` so injected `</instructions>`, `</context>`, `</query>`, `</example>`, or `<script>` delimiters inside bound variables cannot break out of XML containers, while preserving template-authored XML markup).
   - **`pkg/promptsdk/dialects/llama.go` (`DIALECT_LLAMA_SPECIAL`)**:
     - Emits exact Meta Llama-3 special header tokens without HTML/XML escaping collisions on content:
       `<|begin_of_text|><|start_header_id|>system<|end_header_id|>\n\n{system_prompt}<|eot_id|>` followed by ordered exemplar and message blocks `<|start_header_id|>{role}<|end_header_id|>\n\n{content}<|eot_id|>` and trailing generation header `<|start_header_id|>assistant<|end_header_id|>\n\n`.
     - **Context Injection Boundary Guard (`.ai/security.md`)**: Neutralizes reserved Llama-3 control tokens (`<|begin_of_text|>`, `<|start_header_id|>`, `<|end_header_id|>`, `<|eot_id|>`, `<|end_of_text|>`, `<|eom_id|>`, `<|python_tag|>`) inside untrusted runtime variable values directly in the pooled buffer with 0 heap allocations so user-supplied context cannot forge role transitions.
   - **`pkg/promptsdk/dialects/standard.go` (`DIALECT_OPENAI_STANDARD` / `DialectStandardJSON`)**:
     - Writes a deterministic, valid JSON payload containing the ordered `messages` array (`{"role":"system","content":"..."}`, few-shot exemplar messages, and interpolated template messages) and attaches the version's registered JSON output schema under `response_format` (`{"type":"json_schema","json_schema":{"name":"...","strict":true,"schema":{...}}}`) directly into the pooled `*bytes.Buffer` using a zero-allocation streaming JSON string escaper (0 intermediate `encoding/json` heap allocations on the hot path).

3. **Tokenizer-Aware Context Framing & Budget Safety Guard (`pkg/promptsdk/tokenizer/budget_guard.go`)**:
   - Implement a zero-allocation streaming `Tokenizer` interface (defaulting to a deterministic BPE / TikToken `cl100k_base`-compatible subword estimator that operates over string bytes/runes without allocating `[]string` or `[]int` slices, while supporting custom pluggable tokenizers) and `BudgetGuard`:
     - Calculates exact structural token overhead of the target dialect wrapper tags (Anthropic XML tags, Meta Llama-3 `<|begin_of_text|>`, `<|start_header_id|>...<|end_header_id|>`, and `<|eot_id|>` markers, or OpenAI JSON framing + `response_format`), system prompt / persona override, few-shot exemplars, static message text, and non-context bound variables **before** injecting the `context` variable.
     - Accounts for dialect-specific escaping inflation (e.g., XML entity expansion `&lt;`/`&gt;`/`&amp;` or JSON string escaping `\"`/`\n`) and multiple `{{context}}` occurrences so that when `opts.MaxTokens > 0`, `context` is trimmed on clean token and valid UTF-8 rune boundaries (`utf8.RuneStart`) guaranteeing `Tokenizer.CountTokens(finalRenderedOutput) <= opts.MaxTokens`.
     - Specifically validates edge-case inputs sitting within 10 tokens of the target model's context ceiling (per Section 6 of `RAG Prompt Registry Service Plan.pdf`).
     - Returns a typed `ErrBudgetExceeded` if static template + dialect framing + non-context variable overhead alone exceeds `opts.MaxTokens` (or leaves 0 tokens for a non-empty `context`).

4. **Thread-Safe Two-Tier L1 In-Memory Cache (`pkg/promptsdk/cache.go`)**:
   - Implement a high-concurrency, read-through L1 memory cache aligned with Section 4 of `RAG Prompt Registry Service Plan.pdf`:
     - **Immutable Version Cache (`L1_version`, logical key `template:{id}:v{version}`)**: Unbounded TTL storing pre-compiled `CompiledTemplate` (AST + bound `JSONSchema`).
     - **Mutable Tag Pointer Cache (`L1_tag`, logical key `template:{id}:tag:{tag}`)**: Configurable safety fallback TTL (default `5 * time.Minute` per ADR-002) mapping `(template, tag) -> target_version` and resolving against `L1_version` so pointer switches and rollbacks ($v_2 \to v_1$) reuse cached immutable versions without redundant network fetches.
     - **Zero-Allocation Composite Struct Lookup Keys**: Uses value-typed struct keys (`versionCacheKey{templateID, version}`, `tagCacheKey{templateID, tag}`) internally so hot-path L1 cache hits perform `0 allocs/op` (avoiding `fmt.Sprintf` key allocations on every render), while exposing canonical key formatting helpers (`FormatVersionKey`, `FormatTagKey`).
     - **Bidirectional Template Alias (`name` $\leftrightarrow$ `UUID`) Resolution & Eviction**: Tracks template name-to-UUID aliases so `Invalidate(idOrName, tag)` and Redis Pub/Sub `InvalidationEvent` messages (which carry canonical `template_id` UUIDs on `channel:prompt:invalidation`) immediately evict matching `L1_tag` entries whether the caller queried by template UUID or by human-readable template name (Runbook 1).
     - **Cooldown-Protected Stale-While-Offline Resilience**: Retains last-known-good tag resolution across 5-minute TTL expiry when the control plane is unreachable and applies a short failure-retry cooldown so calling orchestrators continue rendering cached templates in memory with $P99 \le 0.5\text{ms}$ and 100% availability during control plane outages.
     - Tracks `L1_version` and `L1_tag` hit/miss atomic counters for the `prompt_sdk_cache_hit_ratio` Prometheus gauge.

5. **Resilient Control Plane gRPC Client, Singleflight Coalescing & Contract Parity (`pkg/promptsdk/client.go` & `ResolveTagResponse`)**:
   - **Control Plane `ResolveTagResponse` Schema Parity**:
     - Include `schema_id` on `ResolveTagResponse` across `proto/v1/prompt_service.proto`, `proto/v1/prompt_service_grpc.go`, `internal/transport/grpc/prompt_admin_server.go`, `internal/transport/http/router.go`, `openapi/openapi.yaml`, and `.ai/doc-api.md` so resolving a tag (`@production`) returns its bound `schema_id` directly alongside `version` and `ast`, matching `GetTemplateVersionResponse`.
   - **Embedded `Client` (`pkg/promptsdk/client.go`)**:
     - Implements `PromptSDK` (`Render`, `Invalidate`, `Close`) plus `RenderToBuffer`, offline/test cache seeding (`PreloadVersion`, `PreloadTag`), and Prometheus metrics snapshot/exposition (`prompt_sdk_render_duration_seconds`, `prompt_sdk_cache_hit_ratio`, `prompt_sdk_pubsub_connected`).
     - **Per-Key Singleflight Request Coalescing**: Coalesces concurrent L1 cache misses for the same `(templateID, tagOrVersion)` so a `@production` invalidation under 10,000 RPS orchestrator traffic issues only a single gRPC request to the control plane.
     - **Background Redis Pub/Sub Invalidation Subscriber (ADR-002 & Runbook 2)**: Subscribes to `channel:prompt:invalidation` (via Redis TCP RESP Pub/Sub or an injected channel/subscriber), immediately evicts invalidated tags in `<100ms`, and updates the `prompt_sdk_pubsub_connected` gauge (`1` when connected, `0` when disconnected and falling back to the 5-minute safety TTL).
     - Emits structured JSON logs via `log/slog` adhering strictly to Invariant 2 (Secured Logging: never logging raw prompt templates, raw context passages, or bearer tokens, and avoiding hot-path log allocations on successful cache-hit renders).

6. **Comprehensive Unit, SDK Benchmark & Concurrency Suites (`pkg/promptsdk/sdk_test.go`, `test/unit/phase2_sdk_test.go`, `test/sdk/sdk_benchmark_test.go`)**:
   - Validate all Phase 2 requirements across `pkg/promptsdk/`, `test/unit/`, and the canonical `test/sdk/` directory from Section 5 of `RAG Prompt Registry Service Plan.pdf`.

---

## 2. Files to Create / Modify

- `pkg/promptsdk/doc.go` — Package overview, `PromptSDK` interface, `DialectType` constants, expanded `RenderOptions` (`TargetDialect`, `Variables`, `MaxTokens`, `Persona`, `StrictVariables`), and error sentinels (`ErrMissingVariable`, `ErrBudgetExceeded`, `ErrUnsupportedDialect`).
- `pkg/promptsdk/compiler.go` — `sync.Pool` byte buffer allocator (`AcquireBuffer`, `ReleaseBuffer`), pre-compiled AST representation (`CompiledTemplate`), zero-allocation `InterpolateIntoBuffer` / `InterpolateIntoBufferChecked`, and dialect/budget compilation coordinator (`CompileIntoBuffer`).
- `pkg/promptsdk/dialects/anthropic.go` — Anthropic XML structural wrapper (`<instructions>`, `<examples>`, `<context>`, `<query>`), idempotent tag detector, and zero-allocation XML delimiter-escape sanitizer.
- `pkg/promptsdk/dialects/llama.go` — Meta Llama-3 special header formatter (`<|begin_of_text|>`, `<|start_header_id|>`, `<|end_header_id|>`, `<|eot_id|>`) and zero-allocation control-token injection sanitizer.
- `pkg/promptsdk/dialects/standard.go` — OpenAI/Mistral standard JSON `messages` and `response_format` schema envelope builder with zero-allocation streaming JSON string escaping.
- `pkg/promptsdk/tokenizer/budget_guard.go` — Zero-allocation BPE/TikToken-compatible subword token counter, dialect framing + escaping overhead calculator, and UTF-8-safe token-boundary context truncator.
- `pkg/promptsdk/cache.go` — Thread-safe two-tier L1 cache (`L1_version` unbounded TTL + `L1_tag` 5m fallback TTL), zero-allocation struct keys, bidirectional `name <-> UUID` alias eviction, Redis Pub/Sub invalidation handler, and stale-while-offline resilience.
- `pkg/promptsdk/client.go` — `Client` implementation of `PromptSDK`, gRPC loader with Bearer JWT auth & singleflight request coalescing, Redis Pub/Sub background listener, and SDK Prometheus metrics collector.
- `proto/v1/prompt_service.proto`, `proto/v1/prompt_service_grpc.go`, `internal/transport/grpc/prompt_admin_server.go`, & `internal/transport/http/router.go` — Add `schema_id` to `ResolveTagResponse` for full tag-to-schema resolution parity.
- `pkg/promptsdk/sdk_test.go` — SDK unit tests and 50 KB context zero-allocation benchmarks across all 3 dialects (`BenchmarkRender50KBContext`, `BenchmarkClientRender50KBAllDialects`).
- `test/unit/phase2_sdk_test.go` — Unit test suite covering compiler edge cases, Anthropic XML / Llama-3 / Standard JSON dialect integrity, prompt-injection delimiter escaping, tokenizer budget ceiling edge cases (within 10 tokens of limit), L1 cache TTL/alias invalidation, singleflight deduplication, and control-plane outage resilience.
- `test/sdk/sdk_benchmark_test.go` — Concurrent cache eviction, 10,000 RPS rendering allocation tests, and P99 latency verification suite matching Section 5 & Section 6 of `RAG Prompt Registry Service Plan.pdf`.
- `openapi/openapi.yaml`, `.ai/doc-api.md`, & `.ai/progress-tracker.md` — Synchronized OpenAPI 3.1 / Doc API contracts and Phase 2 progress tracking.

---

## 3. Security & Operational Invariants Enforcement

- **Zero-Allocation Hot Path (Invariant 4)**: Pre-compiled AST segments, composite struct cache keys, streaming token estimation, and pooled `*bytes.Buffer` instances from `sync.Pool` yield `0 allocs/op` on pooled renders and $P99 \le 0.5\text{ms}$ across 50 KB context blocks.
- **Tokenizer Budget Safety Guard (Invariant 8)**: Dialect framing and escaping token overhead is computed before binding `context`, truncating `context` on valid UTF-8 and token boundaries so `CountTokens(output) <= MaxTokens` always holds.
- **Context Injection Boundary Sanitization (`.ai/security.md`)**: Untrusted variable values are sanitized against dialect delimiter breakouts (`</context>`, `<|start_header_id|>`, `<|eot_id|>`, JSON control characters) without corrupting template markup.
- **Secured Logging (Invariant 2)**: Only emit `trace_id`, `tenant_id`, `template_id`, `version`, `tag`, `dialect`, `latency_ms`, and `template_hash` (`sha256(ast)[:16]`) via `slog.NewJSONHandler`.
- **Continuous Doc API Sync (Invariant 1)**: Keep `.ai/doc-api.md` and `openapi/openapi.yaml` synchronized with all code additions.
- **Git Commit Standards**: Create at least two atomic conventional commits (`feat:`, `tests:`, `docs:`) during Phase 2.

---

## 4. Verification Steps

1. `gofmt -w -s .` (zero diff).
2. `go vet ./...` (zero warnings or errors).
3. `go test -v -race ./test/unit/...` (all Phase 1 and Phase 2 unit tests passing).
4. `go test -v -bench=. -benchmem ./pkg/promptsdk/... ./test/sdk/...` (asserting 0 heap allocations on pooled 50 KB renders and $P99 \le 0.5\text{ms}$).
5. `go test -v ./test/integration/...` (all integration suites passing).
6. Secured logging audit (`grep -rnE '(log|slog|zap|logrus)\.(Print|Info|Debug|Warn|Error).*(raw_prompt|raw_context|password|secret|bearer|authorization)' .`).
