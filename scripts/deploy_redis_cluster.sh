#!/usr/bin/env bash
set -euo pipefail

SERVICE_NAME="${SERVICE_NAME:-rag-prompt-registry-service}"
NAMESPACE="${NAMESPACE:-rag-platform}"
RELEASE_NAME="${RELEASE_NAME:-${SERVICE_NAME}-redis}"
SECRET_NAME="${SECRET_NAME:-${SERVICE_NAME}-secret}"
VALUES_FILE="${VALUES_FILE:-deployments/helm/redis-cluster/values.yaml}"

for cmd in helm kubectl; do
  if ! command -v "$cmd" >/dev/null 2>&1; then
    echo "ERROR: Required CLI '$cmd' is not installed or not on PATH." >&2
    exit 1
  fi
done

if [ ! -f "$VALUES_FILE" ]; then
  echo "ERROR: Helm values file not found: $VALUES_FILE" >&2
  exit 1
fi

echo "[1/5] Ensuring Kubernetes namespace '$NAMESPACE' exists..."
kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -

echo "[2/5] Verifying shared Kubernetes Secret '$SECRET_NAME'..."
if ! kubectl get secret "$SECRET_NAME" --namespace "$NAMESPACE" >/dev/null 2>&1; then
  echo "ERROR: Shared secret '$SECRET_NAME' not found in namespace '$NAMESPACE'." >&2
  echo "Provision it via GitHub Actions (.github/workflows/deploy.yml), External Secrets Operator, or kubectl (see deployments/SECRET_MANAGEMENT.md)." >&2
  exit 1
fi

echo "[3/5] Configuring Bitnami Helm repository and deploying '$RELEASE_NAME'..."
helm repo add bitnami https://charts.bitnami.com/bitnami --force-update
helm repo update bitnami
helm upgrade --install "$RELEASE_NAME" bitnami/redis-cluster \
  --namespace "$NAMESPACE" \
  --version "11.*" \
  -f "$VALUES_FILE" \
  --set "fullnameOverride=$RELEASE_NAME" \
  --set "existingSecret=$SECRET_NAME" \
  --set "existingSecretPasswordKey=REDIS_PASSWORD" \
  --wait \
  --timeout 10m

echo "[4/5] Waiting for all Redis Cluster StatefulSet pods to become Ready..."
kubectl wait --for=condition=ready pod \
  --selector="app.kubernetes.io/instance=${RELEASE_NAME}" \
  --namespace "$NAMESPACE" \
  --timeout=300s

echo "[5/5] Verifying 16,384 Redis Cluster slots via 'redis-cli cluster info'..."
REDIS_PW="$(kubectl get secret "$SECRET_NAME" --namespace "$NAMESPACE" -o jsonpath='{.data.REDIS_PASSWORD}' | base64 -d)"
CLUSTER_INFO="$(kubectl exec "${RELEASE_NAME}-0" --namespace "$NAMESPACE" -- env REDISCLI_AUTH="$REDIS_PW" redis-cli --no-auth-warning cluster info)"
unset REDIS_PW

echo "$CLUSTER_INFO"
if ! echo "$CLUSTER_INFO" | grep -q "cluster_state:ok" || ! echo "$CLUSTER_INFO" | grep -q "cluster_slots_assigned:16384"; then
  echo "ERROR: Redis Cluster is not healthy or does not have all 16,384 slots assigned." >&2
  exit 1
fi

echo "SUCCESS: Bitnami Redis 7.2 Cluster '$RELEASE_NAME' is Ready in namespace '$NAMESPACE' with 16,384 slots assigned."

