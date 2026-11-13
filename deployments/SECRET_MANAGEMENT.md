# Secret Management & CI/CD Secret Injection

This document defines how secrets are separated from non-sensitive configuration, injected via GitHub Actions during deployments, and managed safely in Kubernetes for **`rag-prompt-registry-service`**.

---

## 1. Configuration vs. Secrets Boundary

| Source / Artifact | Contents | Tracked in Git? | Kubernetes Target |
| :--- | :--- | :--- | :--- |
| `.env` | Local development overrides (non-production) | No (`.gitignore`) | N/A (local binary only) |
| `.env.example` | Safe template for local development | Yes | N/A (documentation) |
| `.env.prod` | Production single source of truth for non-sensitive env vars (and local operator reference) | No (`.gitignore`) | Non-sensitive keys synced to `values.yaml` -> `ConfigMap` |
| `deployments/helm/rag-prompt-registry/values.yaml` (`env:`) | Non-sensitive 12-factor variables synced via `scripts/sync_env_to_deployments.py --sync` | Yes | `ConfigMap` ([configmap.yaml](helm/rag-prompt-registry/templates/configmap.yaml)) |
| GitHub Environment Secrets / Vault / ESO | Sensitive credentials (`POSTGRES_DSN`, `JWT_HMAC_SECRET`, `REDIS_PASSWORD`) | No | Kubernetes `Secret` (`rag-prompt-registry-service-secret`) |

### Automated Sensitive Key Exclusion

`scripts/sync_env_to_deployments.py` automatically excludes any key whose name contains:

- `PASSWORD`
- `SECRET`
- `TOKEN`
- `KEY`
- `DSN`
- `CREDENTIAL`
- `PRIVATE`

Accordingly, **`POSTGRES_DSN`**, **`REDIS_PASSWORD`**, and **`JWT_HMAC_SECRET`** are never written into `deployments/helm/rag-prompt-registry/values.yaml` or rendered into `ConfigMap`.

> **Important**: Do **not** add `.env.prod` synchronization jobs to `.github/workflows/`, because `.env.prod` is gitignored and intentionally absent from CI runners. Syncing `values.yaml` from `.env.prod` is enforced locally via `.git/hooks/pre-commit`.

---

## 2. Required Runtime Secrets

| Secret Key | Consumer | Requirement & Security Notes |
| :--- | :--- | :--- |
| `POSTGRES_DSN` | `cmd/prompt-registry-server/main.go` | PostgreSQL 16+ / PgBouncer URI with embedded credentials (e.g. `postgres://user:pass@pgbouncer:6432/prompt_registry?sslmode=require`). Mandatory unless `ALLOW_IN_MEMORY_STORE=true`. |
| `JWT_HMAC_SECRET` | `internal/transport/http/router.go`, `internal/transport/grpc/prompt_admin_server.go`, `pkg/promptsdk/client.go` | High-entropy HS256 signing/verification secret (at least 32 random bytes). Leaving it empty falls back to `DefaultJWTSecret`, which must **never** be used outside local dev. |
| `REDIS_PASSWORD` | `cmd/prompt-registry-server/main.go`, `internal/infrastructure/cache/redis/cluster_client.go` | Redis 7.2 Cluster / Standalone `AUTH` password. May be empty only if Redis authentication is disabled inside an isolated VPC. |

---

## 3. CI/CD Secret Injection via GitHub Actions

Deployment is handled by [`.github/workflows/deploy.yml`](../.github/workflows/deploy.yml), which injects secrets directly into the cluster out-of-band without storing plaintext credentials in `values.yaml` or Helm release history.

### Required GitHub Environment Secrets

Configure the following secrets under **Settings → Environments** for both `staging` and `production` (enable **Required reviewers** on `production`):

| GitHub Secret | Description |
| :--- | :--- |
| `KUBECONFIG_B64` | Base64-encoded kubeconfig for a least-privilege deploy `ServiceAccount` scoped to the pre-provisioned `rag-platform` namespace |
| `POSTGRES_DSN` | Target environment PostgreSQL/PgBouncer connection URI |
| `JWT_HMAC_SECRET` | Target environment HS256 JWT signing/verification secret |
| `REDIS_PASSWORD` | Target environment Redis `AUTH` password (optional if Redis auth is disabled) |

### How Injection Works in `deploy.yml`

1. **Validate Namespace & Secrets**: Fails fast if the pre-provisioned target namespace does not exist or if `KUBECONFIG_B64`, `POSTGRES_DSN`, or `JWT_HMAC_SECRET` is missing.
2. **Decode Kubeconfig**: Writes `KUBECONFIG` to `$RUNNER_TEMP/kubeconfig` with `0600` permissions.
3. **Out-of-Band Secret Upsert**:
   - Writes `POSTGRES_DSN`, `REDIS_PASSWORD`, and `JWT_HMAC_SECRET` to a `0600` temporary file (`$RUNNER_TEMP/secret.env`) without echoing values to stdout or passing credentials in CLI flags.
   - Runs `kubectl create secret generic rag-prompt-registry-service-secret --from-env-file="$RUNNER_TEMP/secret.env" --dry-run=client -o yaml | kubectl apply -f -`.
   - Deletes `$RUNNER_TEMP/secret.env` immediately.
4. **Helm Release Deployment**:
   - Invokes `helm upgrade --install` with `--set env.secretName=rag-prompt-registry-service-secret` and `--set secrets.existingSecret=rag-prompt-registry-service-secret` (passing validated `image.tag` via `--set-string` when provided).
   - Because `existingSecret` / `env.secretName` is set, Helm skips rendering [secret.yaml](helm/rag-prompt-registry/templates/secret.yaml) and stores zero credentials inside Helm release Secret objects. Chart-managed Secrets include `"helm.sh/resource-policy": keep`, and `secret.yaml` blocks switching `existingSecret` to a Helm-owned Secret until that keep policy handoff is recorded.
   - When `canary_enabled=true`, the workflow waits up to 15 minutes for the Argo `Rollout` to reach `Healthy` and fails on `Degraded`/abort or timeout.
5. **Secret Rotation Rollout**:
   - If `kubectl apply` reports that the Secret was `created` or `configured`, or if the workload/pods have not yet reconciled the Secret's `resourceVersion` annotation (`rag-prompt-registry/secret-resource-version`), the workflow triggers a restart (`deployment/rag-prompt-registry-service` or `rollout.argoproj.io/rag-prompt-registry-service` when `canary_enabled=true`) and waits for readiness before recording the reconciled `resourceVersion`.
6. **Always Clean Up**:
   - An `if: always()` step removes `$RUNNER_TEMP/kubeconfig` and `$RUNNER_TEMP/secret.env`.

---

## 4. Alternative Production Secret Backends

### External Secrets Operator (ESO) / HashiCorp Vault

When using External Secrets Operator instead of GitHub Actions secret upsert, create an `ExternalSecret` targeting `rag-prompt-registry-service-secret` and set `env.secretName: "rag-prompt-registry-service-secret"` in `values.yaml` (preserved by `scripts/sync_env_to_deployments.py --sync`):

```yaml
apiVersion: external-secrets.io/v1beta1
kind: ExternalSecret
metadata:
  name: rag-prompt-registry-service-secret
  namespace: rag-platform
spec:
  refreshInterval: 1h
  secretStoreRef:
    name: vault-backend
    kind: ClusterSecretStore
  target:
    name: rag-prompt-registry-service-secret
    creationPolicy: Owner
  data:
    - secretKey: POSTGRES_DSN
      remoteRef:
        key: secret/data/rag-platform/prompt-registry
        property: postgres_dsn
    - secretKey: REDIS_PASSWORD
      remoteRef:
        key: secret/data/rag-platform/prompt-registry
        property: redis_password
    - secretKey: JWT_HMAC_SECRET
      remoteRef:
        key: secret/data/rag-platform/prompt-registry
        property: jwt_hmac_secret
```

### Manual Cluster Bootstrap (Air-Gapped / Emergency)

```bash
kubectl create secret generic rag-prompt-registry-service-secret \
  --namespace rag-platform \
  --from-env-file=.env.secrets \
  --dry-run=client -o yaml | kubectl apply -f -
```

---

## 5. Secret Rotation & Logging Guardrails

- **Rotating `JWT_HMAC_SECRET`**: Coordinate with calling `go-prompt-sdk` orchestrators (`JWT_HMAC_SECRET` / `ClientConfig.JWTSecret`) before rolling the control plane pods so minted 24-hour service JWTs remain valid across the rollout window.
- **Rotating `POSTGRES_DSN`**:
  1. Provision or rotate the database/PgBouncer credentials on the PostgreSQL/PgBouncer side (using staged/dual credentials when zero downtime is required).
  2. Update `POSTGRES_DSN` in GitHub Environment Secrets (or Vault/ESO) and re-run `.github/workflows/deploy.yml`.
  3. Confirm the control plane rollout completes and `/ready` returns `200 OK` (`postgres: connected`).
- **Rotating `REDIS_PASSWORD` (Coordinated Cluster & Consumer Rotation)**:
  - `bitnami/redis-cluster` reads `existingSecret` (`rag-prompt-registry-service-secret`, key `REDIS_PASSWORD`) only when Redis containers start; updating the Kubernetes Secret alone does **not** rotate authentication on running Redis Cluster pods.
  - Perform rotation in the following coordinated sequence:
    1. Update `REDIS_PASSWORD` in GitHub Environment Secrets / Vault and apply the updated `rag-prompt-registry-service-secret` in `rag-platform`.
    2. Rotate server authentication across all Redis Cluster nodes (via `CONFIG SET requirepass` / `masterauth` + `CONFIG REWRITE` during a staged transition, or a coordinated rolling restart / `helm upgrade` of `rag-prompt-registry-service-redis` so StatefulSet pods boot with the updated `existingSecret` `REDIS_PASSWORD`).
    3. Roll all application consumers (`rag-prompt-registry-service` control plane pods and `rag-search-orchestrator` data plane pods running `go-prompt-sdk` Pub/Sub subscribers) so they authenticate with the new `REDIS_PASSWORD`.
    4. Consider rotation complete **only after** the new credentials are active on all Redis nodes and consumers, `redis-cli cluster info` reports `cluster_state:ok`, control plane `/ready` returns `200 OK` (`postgres: connected`, `redis: reachable`), and `prompt_sdk_pubsub_connected == 1`.
- **Secured Logging Invariant**: Application logs (`log/slog` JSON) and `postgres.SanitizeDSNError` mask DSNs and passwords; CI enforces `security-and-logging-audit` in [`.github/workflows/ci.yml`](../.github/workflows/ci.yml) to block accidental secret logging.

