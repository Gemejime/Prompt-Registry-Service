[CmdletBinding()]
param(
    [string]$ServiceName = "rag-prompt-registry-service",
    [string]$Namespace = "rag-platform",
    [string]$ReleaseName = "",
    [string]$SecretName = "",
    [string]$ValuesFile = "deployments/helm/redis-cluster/values.yaml"
)

$ErrorActionPreference = "Stop"
if ([string]::IsNullOrWhiteSpace($ReleaseName)) { $ReleaseName = "$ServiceName-redis" }
if ([string]::IsNullOrWhiteSpace($SecretName)) { $SecretName = "$ServiceName-secret" }

foreach ($cmd in @("helm", "kubectl")) {
    if (-not (Get-Command $cmd -ErrorAction SilentlyContinue)) {
        throw "Required CLI '$cmd' is not installed or not on PATH."
    }
}

if (-not (Test-Path $ValuesFile)) {
    throw "Helm values file not found: $ValuesFile"
}

Write-Host "[1/5] Ensuring Kubernetes namespace '$Namespace' exists..."
$nsYaml = kubectl create namespace $Namespace --dry-run=client -o yaml
if ($LASTEXITCODE -ne 0) { throw "Failed to generate namespace manifest." }
$nsYaml | kubectl apply -f -
if ($LASTEXITCODE -ne 0) { throw "Failed to apply namespace '$Namespace'." }

Write-Host "[2/5] Verifying shared Kubernetes Secret '$SecretName'..."
kubectl get secret $SecretName --namespace $Namespace *> $null
if ($LASTEXITCODE -ne 0) {
    throw "Shared secret '$SecretName' not found in namespace '$Namespace'. Provision it first (see deployments/SECRET_MANAGEMENT.md)."
}

Write-Host "[3/5] Configuring Bitnami Helm repository and deploying '$ReleaseName'..."
helm repo add bitnami https://charts.bitnami.com/bitnami --force-update
if ($LASTEXITCODE -ne 0) { throw "Failed to add Bitnami Helm repository." }
helm repo update bitnami
if ($LASTEXITCODE -ne 0) { throw "Failed to update Bitnami Helm repository." }

helm upgrade --install $ReleaseName bitnami/redis-cluster `
    --namespace $Namespace `
    --version "11.*" `
    -f $ValuesFile `
    --set "fullnameOverride=$ReleaseName" `
    --set "existingSecret=$SecretName" `
    --set "existingSecretPasswordKey=REDIS_PASSWORD" `
    --wait `
    --timeout 10m
if ($LASTEXITCODE -ne 0) { throw "Helm deployment failed for '$ReleaseName'." }

Write-Host "[4/5] Waiting for all Redis Cluster StatefulSet pods to become Ready..."
kubectl wait --for=condition=ready pod `
    --selector="app.kubernetes.io/instance=$ReleaseName" `
    --namespace $Namespace `
    --timeout=300s
if ($LASTEXITCODE -ne 0) { throw "Timed out waiting for Redis Cluster pods to reach Ready state." }

Write-Host "[5/5] Verifying 16,384 Redis Cluster slots via 'redis-cli cluster info'..."
$b64Pw = kubectl get secret $SecretName --namespace $Namespace -o "jsonpath={.data.REDIS_PASSWORD}"
if ($LASTEXITCODE -ne 0) { throw "Failed to read REDIS_PASSWORD from secret '$SecretName'." }
$redisPw = ""
if (-not [string]::IsNullOrWhiteSpace($b64Pw)) {
    $redisPw = [System.Text.Encoding]::UTF8.GetString([System.Convert]::FromBase64String($b64Pw))
}

$clusterInfo = kubectl exec "$ReleaseName-0" --namespace $Namespace -- env "REDISCLI_AUTH=$redisPw" redis-cli --no-auth-warning cluster info
$redisPw = $null
if ($LASTEXITCODE -ne 0) { throw "Failed to execute 'redis-cli cluster info' on '$ReleaseName-0'." }

$clusterInfoText = $clusterInfo -join "`n"
Write-Host $clusterInfoText

if (($clusterInfoText -notmatch "cluster_state:ok") -or ($clusterInfoText -notmatch "cluster_slots_assigned:16384")) {
    throw "Redis Cluster is not healthy or does not have all 16,384 slots assigned."
}

Write-Host "SUCCESS: Bitnami Redis 7.2 Cluster '$ReleaseName' is Ready in namespace '$Namespace' with 16,384 slots assigned."

