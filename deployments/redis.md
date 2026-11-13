# Self-Hosted Bitnami Redis 7.2 Cluster Guide

This document describes the dedicated, production-grade **Bitnami Redis 7.2 Cluster** deployment for **`rag-prompt-registry-service`**, supporting the **L2 AST/Schema/Tag Cache** and the **Redis Pub/Sub Cache Invalidation Bus** (`channel:prompt:invalidation`, **ADR-002**).

---

## 1. Cluster Topology & Latency Architecture

- **Topology**: 6-node Redis 7.2 Cluster (`3` Primary shards + `3` Secondary replicas, `cluster.replicas: 1`, `cluster.init: true`), distributing **16,384 hash slots** evenly across 3 primaries.
- **Namespace Co-Location**: Deployed directly inside namespace **`rag-platform`** alongside `rag-prompt-registry-service` and `rag-search-orchestrator` pods with soft pod anti-affinity (`podAntiAffinityPreset: soft`).
- **Why In-Cluster Redis Achieves $P99 < 1\text{ms}$**:
  - **Zero External VPC / NAT Egress**: Pod-to-pod traffic stays on the Kubernetes CNI overlay/VPC network within the same availability zone/namespace, avoiding external managed-cache hop overhead.
  - **L2 Read-Through + Pub/Sub Coherence**: Control plane reads hit local L2 cache (`l2:tenant:...`) at sub-millisecond latency ($P99 \le 2\text{ms}$ SLO), while `@production` tag promotions publish eviction events on `channel:prompt:invalidation` across all nodes in $<100\text{ms}$.
- **Shared Credential Binding**: Authenticates using `REDIS_PASSWORD` from the shared Secret **`rag-prompt-registry-service-secret`** (`existingSecret`), eliminating credential drift between the control plane and Redis.
- **Persistence & Sizing**: `5Gi` `ReadWriteOnce` PVC per node with AOF (`appendonly yes`, `appendfsync everysec`) + RDB snapshots, `100m`/`256Mi` requests, `500m`/`1Gi` limits, and Prometheus exporter on port **`9121`**.

---

## 2. Internal Headless DNS Routing

StatefulSet pods are addressed directly via the Kubernetes headless Service (`rag-prompt-registry-service-redis-headless`):

```text
<SERVICE_NAME>-redis-<ordinal>.<SERVICE_NAME>-redis-headless.<NAMESPACE>.svc.cluster.local:6379
```

Canonical seed addresses for **`rag-prompt-registry-service`** in **`rag-platform`**:

```env
REDIS_CLUSTER_ADDRS="rag-prompt-registry-service-redis-0.rag-prompt-registry-service-redis-headless.rag-platform.svc.cluster.local:6379,rag-prompt-registry-service-redis-1.rag-prompt-registry-service-redis-headless.rag-platform.svc.cluster.local:6379,rag-prompt-registry-service-redis-2.rag-prompt-registry-service-redis-headless.rag-platform.svc.cluster.local:6379"
```

The Go L2 client (`internal/infrastructure/cache/redis/cluster_client.go`) follows Redis Cluster `-MOVED` and `-ASK` slot redirects automatically across all cluster shards.

---

## 3. Local Development, Kubernetes & GitOps Deployment

### Local Development (Docker Compose)

Start a standalone Redis 7.2 container (`bitnami/redis:7.2-debian-12`) with AOF persistence and password authentication on `localhost:6379`:

```bash
# Set password matching your local .env (defaults to localdevpassword if unset)
export REDIS_PASSWORD=localdevpassword
docker compose -f deployments/docker/docker-compose.redis.yml up -d

# Verify container health
docker compose -f deployments/docker/docker-compose.redis.yml ps
```

Ensure `.env` has `REDIS_ADDRS=localhost:6379`, `REDIS_PASSWORD=localdevpassword`, and `REDIS_CLUSTER_ADDRS=` (empty so local development uses `REDIS_ADDRS`).

### One-Click Kubernetes Deployment (Helm)

Ensure `rag-prompt-registry-service-secret` exists in `rag-platform` (see [`deployments/SECRET_MANAGEMENT.md`](SECRET_MANAGEMENT.md)), then run:

- **Linux / macOS (Bash)**:
  ```bash
  ./scripts/deploy_redis_cluster.sh
  ```
- **Windows (PowerShell)**:
  ```powershell
  .\scripts\deploy_redis_cluster.ps1
  ```

### Declarative GitOps Sync (ArgoCD)

Apply [`deployments/argocd/redis-cluster-application.yaml`](argocd/redis-cluster-application.yaml):

```bash
kubectl apply -f deployments/argocd/redis-cluster-application.yaml
kubectl get application rag-prompt-registry-service-redis -n argocd
```

ArgoCD deploys `bitnami/redis-cluster` (`11.*`) using [`deployments/helm/redis-cluster/values.yaml`](helm/redis-cluster/values.yaml) with automated `prune: true`, `selfHeal: true`, and exponential retry backoff.

---

## 4. Day-2 Operational Runbooks

### Runbook A: Cluster Health & Slot Inspection

```bash
REDIS_PW=$(kubectl get secret rag-prompt-registry-service-secret -n rag-platform -o jsonpath='{.data.REDIS_PASSWORD}' | base64 -d)

# Verify cluster_state:ok and cluster_slots_assigned:16384
kubectl exec rag-prompt-registry-service-redis-0 -n rag-platform -- \
  env REDISCLI_AUTH="$REDIS_PW" redis-cli --no-auth-warning cluster info

# Inspect primary/replica shard topology
kubectl exec rag-prompt-registry-service-redis-0 -n rag-platform -- \
  env REDISCLI_AUTH="$REDIS_PW" redis-cli --no-auth-warning cluster nodes
```

### Runbook B: Real-Time Latency Verification

```bash
# Measure live round-trip ping latency inside the cluster (Ctrl+C to stop)
kubectl exec -it rag-prompt-registry-service-redis-0 -n rag-platform -- \
  env REDISCLI_AUTH="$REDIS_PW" redis-cli --latency --no-auth-warning -i 1

# Verify active Pub/Sub invalidation channel subscribers (ADR-002)
kubectl exec rag-prompt-registry-service-redis-0 -n rag-platform -- \
  env REDISCLI_AUTH="$REDIS_PW" redis-cli --no-auth-warning PUBSUB CHANNELS channel:prompt:invalidation
```

### Runbook C: Prometheus Metrics Scraping

The Bitnami Redis Exporter sidecar exposes Prometheus metrics on port **`9121`** with pod annotations `prometheus.io/scrape: "true"` and `prometheus.io/port: "9121"`:

```bash
kubectl port-forward svc/rag-prompt-registry-service-redis-metrics 9121:9121 -n rag-platform
curl -s http://127.0.0.1:9121/metrics | grep -E 'redis_up|redis_cluster_slots|redis_connected_clients'
```

