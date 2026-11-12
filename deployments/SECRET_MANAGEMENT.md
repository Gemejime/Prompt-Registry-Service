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
| `KUBECONFIG_B64` | Base64-encoded kubeconfig for a least-privilege deploy `ServiceAccount` scoped to the `rag-platform` namespace |
| `POSTGRES_DSN` | Target environment PostgreSQL/PgBouncer connection URI |
| `JWT_HMAC_SECRET` | Target environment HS256 JWT signing/verification secret |
| `REDIS_PASSWORD` | Target environment Redis `AUTH` password (optional if Redis auth is disabled) |

### How Injection Works in `deploy.yml`

1. **Validate Secrets**: Fails fast if `KUBECONFIG_B64`, `POSTGRES_DSN`, or `JWT_HMAC_SECRET` is missing.
2. **Decode Kubeconfig**: Writes `KUBECONFIG` to `$RUNNER_TEMP/kubeconfig` with `0600` permissions.
3. **Out-of-Band Secret Upsert**:
   - Writes `POSTGRES_DSN`, `REDIS_PASSWORD`, and `JWT_HMAC_SECRET` to a `0600` temporary file (`$RUNNER_TEMP/secret.env`) without echoing values to stdout or passing credentials in CLI flags.
   - Runs `kubectl create secret generic rag-prompt-registry-service-secret --from-env-file="$RUNNER_TEMP/secret.env" --dry-run=client -o yaml | kubectl apply -f -`.
   - Deletes `$RUNNER_TEMP/secret.env` immediately.
4. **Helm Release Deployment**:
   - Invokes `helm upgrade --install` with `--set env.secretName=rag-prompt-registry-service-secret` and `--set secrets.existingSecret=rag-prompt-registry-service-secret`.
   - Because `existingSecret` / `env.secretName` is set, Helm skips rendering [secret.yaml](helm/rag-prompt-registry/templates/secret.yaml) and stores zero credentials inside Helm release Secret objects.
5. **Secret Rotation Rollout**:
   - If `kubectl apply` reports that the Secret was `created` or `configured`, the workflow triggers `kubectl rollout restart deployment/rag-prompt-registry-service` (when `canary_enabled=false`) and waits for readiness.
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
- **Rotating `POSTGRES_DSN` / `REDIS_PASSWORD`**: Update the GitHub Environment secret (or Vault path), re-run `.github/workflows/deploy.yml`, and verify `/ready` returns `200 OK` (`postgres: connected`, `redis: reachable`).
- **Secured Logging Invariant**: Application logs (`log/slog` JSON) and `postgres.SanitizeDSNError` mask DSNs and passwords; CI enforces `security-and-logging-audit` in [`.github/workflows/ci.yml`](../.github/workflows/ci.yml) to block accidental secret logging.

