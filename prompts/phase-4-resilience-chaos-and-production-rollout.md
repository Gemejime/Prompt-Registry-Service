# Implementation Plan: Phase 4 — Resilience, Chaos & Production Rollout

## 1. Objective & Scope

Implement **Phase 4: Resilience, Chaos & Production Rollout (Weeks 7–8)** of the `rag-prompt-registry-service` in strict compliance with `GEMINI.md`, `.ai/` system specifications (`project-overview.md`, `architecture.md`, `doc-api.md`, `code-standards.md`, `security.md`, `ai-workflow-rules.md`), and the reference blueprints in `tasks/` (`RAG Prompt Registry Service Plan.pdf`, `Production Grade Backend Concepts.pdf`, `My Naming Convention.pdf`, and `Go and Python Microservices Architecture.pdf`):

### 1. Control Plane Metrics Hardening for Production Alert Parity
- **Pointer Rollback Metric Exposition (`internal/application/command/`, `internal/transport/http/router.go`)**:
  - Track `isRollback` inside `MutationMetricsRecorder` when `PromoteTagHandler` executes a semantic tag rollback (`isRollback == true`).
  - Expose `prompt_registry_pointer_rollbacks_total` counter on the `/metrics` endpoint alongside `prompt_registry_mutation_duration_seconds` and `prompt_invalidation_events_published_total`.
  - Ensure the production Prometheus alert rule `ElevatedPointerRollbacks` (`increase(prompt_registry_pointer_rollbacks_total[15m]) > 0`) evaluates against real exported telemetry.
- **Port Standardization Alignment (`cmd/prompt-registry-server/main.go`, `.env.example`)**:
  - Align daemon default `GRPC_PORT` to `9090` (matching the Kubernetes Service specification and Dockerfile EXPOSE directives) while preserving environment variable override capability.

### 2. Chaos Engineering & Outage Resilience Suite (`test/integration/phase4_chaos_test.go`)
- **Pod & Service Outage Injection**:
  - Test sustained registry control plane daemon and Redis server crash/termination while concurrent orchestrator worker routines continuously render cached `@production` templates via `pkg/orchestrator` and `pkg/promptsdk`.
  - Assert 100% request success rate, 0 dropped queries, and $P99 \le 0.5\text{ms}$ rendering latency during complete control plane and cache backing outages.
- **Thundering Herd Recovery & Singleflight Barrier**:
  - Simulate sudden control plane restoration following an outage where 1,000 concurrent goroutines request refreshed tag pointers.
  - Verify that singleflight coalescing (`singleflight.Group`) allows exactly 1 gRPC network fetch per unique key to PostgreSQL/Redis without thread starvation, CPU spikes, or socket leaks.
- **Pub/Sub Connection Flapping & Reconnect Jitter**:
  - Inject rapid network disconnects/reconnects on the Redis Pub/Sub invalidation bus.
  - Verify that exponential backoff with ±20% jitter operates cleanly, the `prompt_sdk_pubsub_connected` gauge accurately flips between 0 and 1, and no orphan background subscriber goroutines remain.
- **Rollback Under Adverse Network Conditions (Runbook 1 & Runbook 2)**:
  - Verify pointer rollback (`PUT /v1/templates/{id}/tags/@production`) under partial Redis failures: confirm PostgreSQL commits atomically, `broadcast_emitted: false` is accurately returned in `PromoteTagResponse`, and subsequent republish or cache wipe (`Client.Purge()`) recovers coherence.

### 3. High-Throughput Allocation Profiling & 10,000 RPS Load Test (`test/sdk/phase4_load_benchmark_test.go`)
- **10,000 RPS Sustained Load Benchmark**:
  - Execute a benchmark running 10,000 parallel renders/sec simulating heavy `rag-search-orchestrator` production load with 50 KB context blocks.
  - Validate that `sync.Pool` buffer reuse sustains **0 heap allocations (`0 B/op`, `0 allocs/op`)** on the compilation hot path.
  - Verify latency SLO: $P99 \le 0.5\text{ms}$ across all dialects (Anthropic XML, Meta Llama-3, OpenAI Standard JSON).
- **Zero Memory Accumulation / Leak Profiling**:
  - Measure runtime memory stats (`runtime.ReadMemStats`, `HeapAlloc`, `HeapObjects`, `NumGC`) across repeated 10,000 request batches.
  - Verify flat heap allocation without runaway buffer growth (ensuring oversized context buffers are either reset or bounded before return to `sync.Pool`).
- **Control Plane RSS Memory Profiling**:
  - Verify control plane footprint invariants: steady-state memory $\le 64\text{ MiB}$ RSS, peak $\le 128\text{ MiB}$ RSS under administrative load.

### 4. Hardened Distroless Containerization (`deployments/docker/Dockerfile`)
- **Multi-Stage Build Pipeline**:
  - Builder stage: `golang:1.26-alpine` (aligned with `go.mod` requiring `go 1.26.0`) compiling `cmd/prompt-registry-server/main.go` with `CGO_ENABLED=0`, `-trimpath`, `-ldflags="-s -w -X main.version=..."`.
  - Final runtime stage: `gcr.io/distroless/static-debian12:nonroot` (minimal attack surface, zero shell, zero package manager, zero utility binaries).
- **Static Asset Packaging**:
  - Copy `openapi/openapi.yaml` to `/app/openapi/openapi.yaml` and configure working directory `/app` to guarantee native OpenAPI 3.1 contract serving (`/docs`, `/redoc`, `/openapi.yaml`).
- **Security & Privilege Hardening (Layer 4 & Layer 5)**:
  - Run as non-root user `USER 65532:65532` (`nonroot`).
  - Read-only root filesystem compatible (`readOnlyRootFilesystem: true`).
  - Expose ports: `8080` (HTTP REST, Doc API, Probes, Metrics) and `9090` (gRPC).
  - Health check definitions targeting `/healthz` and `/ready`.

### 5. Production-Grade Helm Chart (`deployments/helm/rag-prompt-registry/`)
- **Chart Hierarchy (`deployments/helm/rag-prompt-registry/`)**:
  - `Chart.yaml`: Helm v2 API, standard naming `rag-prompt-registry-service` per `tasks/My Naming Convention.pdf`.
  - `values.yaml`: Production-calibrated defaults conforming to Section 8 of `RAG Prompt Registry Service Plan.pdf`:
    - Control Plane Pod Sizing: `cpu: {requests: 100m, limits: 500m}`, `memory: {requests: 64Mi, limits: 128Mi}`.
    - Replicas: `2` (High Availability).
    - Data Plane Sizing Reference: `cpu: {requests: 1000m, limits: 4000m}`, `memory: {requests: 1Gi, limits: 2Gi}`.
    - Networking: HTTP port `8080`, gRPC port `9090` (`GRPC_PORT: 9090`).
    - Probes: Liveness `/healthz`, Readiness `/ready` (initialDelaySeconds: 5, periodSeconds: 10).
    - Database configuration for PgBouncer Simple Protocol mode (`CONN_EXEC_MODE=simple_protocol`, ADR-003).
    - Pod Anti-Affinity: configurable soft (default: `preferredDuringSchedulingIgnoredDuringExecution`) allowing single-node CI/dev operation, with toggle for production hard anti-affinity.
    - Ephemeral Storage: `/tmp` `emptyDir` mount (`sizeLimit: 16Mi`) ensuring compatibility with `readOnlyRootFilesystem: true`.
  - `templates/deployment.yaml`:
    - Kubernetes deployment conditionally rendered when `canary.enabled: false`.
    - Hardened `securityContext`: `runAsNonRoot: true`, `runAsUser: 65532`, `runAsGroup: 65532`, `readOnlyRootFilesystem: true`, `allowPrivilegeEscalation: false`, `capabilities: {drop: ["ALL"]}`, `seccompProfile: {type: "RuntimeDefault"}`.
  - `templates/service.yaml`: Dual-port Service exposing HTTP (8080) and gRPC (9090).
  - `templates/poddisruptionbudget.yaml`: `minAvailable: 1` guaranteeing quorum and zero downtime during node drains.
  - `templates/configmap.yaml` & `templates/secret.yaml`: Environment variables and secret references.
  - `templates/servicemonitor.yaml`: Prometheus Operator ServiceMonitor scraping `/metrics` on port 8080 every 15s.
  - `templates/prometheusrule.yaml`: The 3 core alerting rules specified in Section 8 of `RAG Prompt Registry Service Plan.pdf`:
    1. `SDKHighRenderLatency`: P99 render duration > 2ms over a 5-minute window.
    2. `PubSubInvalidationFailure`: Redis invalidation publish error rate > 0.
    3. `ElevatedPointerRollbacks`: Tag promotions rolled back within 15 minutes of release (`increase(prompt_registry_pointer_rollbacks_total[15m]) > 0`).

### 6. Approved GitOps ArgoCD & Argo Rollouts Integration
- **GitOps ArgoCD Application Manifest (`deployments/gitops/argocd-application.yaml`)**:
  - Declarative ArgoCD `Application` CRD managing the `rag-prompt-registry-service` Helm release.
  - Automated sync policy with `prune: true`, `selfHeal: true`, and automated retry backoff.
- **Canary Cutover Manifest & Strategy (`templates/rollout.yaml` & `templates/analysistemplate.yaml`)**:
  - Conditionally rendered when `canary.enabled: true` (replacing `deployment.yaml` with an Argo Rollout).
  - Progressive traffic migration steps: `10%` canary -> pause for analysis -> `50%` promotion -> pause for analysis -> `100%` full cutover.
  - Automated analysis via `templates/analysistemplate.yaml` querying Prometheus:
    - Step 1: Invalidation publish error rate `rate(prompt_invalidation_events_published_total{status="error"}[2m]) == 0`.
    - Step 2: Mutation error rate `rate(prompt_registry_mutation_duration_seconds_count{status="error"}[2m]) == 0`.
    - Automated abort criteria: if Prometheus alerts trigger or metrics exceed threshold, rollback to stable ReplicaSet is executed immediately without human intervention.

### 7. Documentation & Runbook 3 Alignment
- **Runbook 3 ("Progressive Canary Rollout & Automated Abort via Argo Rollouts")**:
  - Formally document Runbook 3 in `.ai/progress-tracker.md` and `.ai/doc-api.md`.
  - Operational steps for launching canary deployments, monitoring Argo Rollout status via `kubectl argo rollouts get rollout rag-prompt-registry-service`, evaluating Prometheus canary metrics, and manual abort via `kubectl argo rollouts abort`.

### 8. Spec Synchronization & Quality Gates
- **Doc API & OpenAPI Parity**:
  - Synchronize `.ai/doc-api.md` with deployment specifications, Dockerfile details, Helm values, Prometheus alert rules, and Runbook 3.
  - Verify `openapi/openapi.yaml` contract consistency.
- **Pre-Commit Verification**:
  - `gofmt -w -s .`, `go vet ./...`, `golangci-lint run`.
  - Pass all test suites (`test/unit/...`, `test/integration/...`, `test/sdk/...`).
  - Update `.ai/progress-tracker.md` to reflect Phase 4 completion and milestone sign-off.

---

## 2. Implementation Deliverables

1. `internal/application/command/mutation_metrics.go` & `internal/transport/http/router.go`:
   - Instrumentation exposing `prompt_registry_pointer_rollbacks_total` on `/metrics` when `isRollback` is true.
2. `cmd/prompt-registry-server/main.go` & `.env.example`:
   - Align gRPC default port to `9090` (consistent with Helm/Docker specs).
3. `deployments/docker/Dockerfile`:
   - Multi-stage distroless Dockerfile using `golang:1.26-alpine` and `gcr.io/distroless/static-debian12:nonroot`, copying `openapi/openapi.yaml`.
4. `deployments/docker/.dockerignore`:
   - Ignore `.git`, `tasks/`, test caches, binaries, and local artifacts.
5. `deployments/helm/rag-prompt-registry/Chart.yaml`:
   - Production Helm chart metadata (`rag-prompt-registry-service`).
6. `deployments/helm/rag-prompt-registry/values.yaml`:
   - Calibrated values for control plane sizing, PgBouncer simple protocol, Redis pubsub, ports, anti-affinity, and canary parameters.
7. `deployments/helm/rag-prompt-registry/templates/deployment.yaml`:
   - 2-replica hardened deployment with anti-affinity, security context, emptyDir `/tmp`, and probes.
8. `deployments/helm/rag-prompt-registry/templates/service.yaml`:
   - Dual HTTP (8080) / gRPC (9090) cluster service.
9. `deployments/helm/rag-prompt-registry/templates/poddisruptionbudget.yaml`:
   - `minAvailable: 1` PDB.
10. `deployments/helm/rag-prompt-registry/templates/configmap.yaml` & `templates/secret.yaml`:
    - ConfigMap and Secret manifests.
11. `deployments/helm/rag-prompt-registry/templates/servicemonitor.yaml`:
    - Prometheus metrics scraping configuration (every 15s).
12. `deployments/helm/rag-prompt-registry/templates/prometheusrule.yaml`:
    - Prometheus alert rules (`SDKHighRenderLatency`, `PubSubInvalidationFailure`, `ElevatedPointerRollbacks`).
13. `deployments/helm/rag-prompt-registry/templates/rollout.yaml`:
    - Argo Rollout manifest for canary deployments when `canary.enabled: true`.
14. `deployments/helm/rag-prompt-registry/templates/analysistemplate.yaml`:
    - Automated Prometheus metric analysis template for canary gating.
15. `deployments/gitops/argocd-application.yaml`:
    - Declarative GitOps ArgoCD Application manifest.
16. `test/integration/phase4_chaos_test.go`:
    - Chaos testing suite: pod failure, Redis outage, singleflight barrier, reconnect backoff, and rollback resilience.
17. `test/sdk/phase4_load_benchmark_test.go`:
    - 10,000 RPS load benchmark and zero memory accumulation profiling.
18. `.ai/progress-tracker.md`:
    - Update roadmap to record Runbook 3 and Phase 4 completion.
19. `.ai/doc-api.md`:
    - Document deployment specifications, Docker configuration, Prometheus alerting matrix, and Runbook 3.

---

## 3. Human-in-the-Loop Confirmation Gate

In accordance with `.ai/ai-workflow-rules.md`, this plan has been pressure-tested against all reference blueprints in `tasks/` and confirmed for execution.
