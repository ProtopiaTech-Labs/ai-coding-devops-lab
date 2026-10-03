#!/usr/bin/env bash
# Installs Traefik (default IngressClass, static public IP), cert-manager and the `letsencrypt` ClusterIssuer.
# Idempotent: a second run changes nothing. Needs the kubeconfig from infra/aks.sh.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
# shellcheck source=/dev/null
source "$ROOT/.env"

: "${AZURE_SUBSCRIPTION_ID:?set in .env}" "${ACME_EMAIL:?set in .env}"
export RESOURCE_GROUP PUBLIC_IP_NAME ACME_EMAIL
TRAEFIK_CHART_VERSION=41.6.1
CERT_MANAGER_CHART_VERSION=v1.21.2

log() { echo "==> $*"; }

# $1 release (= namespace), $2 chart, $3 version, $4 values file (envsubst is applied)
ensure_release() {
  local values current_chart current_values
  values=$(envsubst <"$4")
  current_chart=$(helm list -n "$1" -f "^$1\$" --deployed -o json | jq -r '.[0].chart // empty')
  current_values=$(helm get values "$1" -n "$1" -o json 2>/dev/null | jq -S . || true)
  if [[ "$current_chart" == "$1-$3" && "$current_values" == "$(yq -o json <<<"$values" | jq -S .)" ]]; then
    log "helm release $1 $3: unchanged"
  else
    log "helm release $1 $3: install or upgrade"
    helm upgrade --install "$1" "$2" --version "$3" -n "$1" --create-namespace \
      -f <(echo "$values") --wait --timeout 10m >/dev/null
  fi
}

helm repo add traefik https://traefik.github.io/charts --force-update >/dev/null
ensure_release traefik traefik/traefik "$TRAEFIK_CHART_VERSION" "$ROOT/infra/values/traefik.yaml"
ensure_release cert-manager oci://quay.io/jetstack/charts/cert-manager "$CERT_MANAGER_CHART_VERSION" \
  "$ROOT/infra/values/cert-manager.yaml"

log "ClusterIssuer letsencrypt: $(envsubst <"$ROOT/infra/manifests/cluster-issuer.yaml" | kubectl apply -f - | sed 's/.* //')"
kubectl wait clusterissuer letsencrypt --for=condition=Ready --timeout=2m >/dev/null
log "ClusterIssuer letsencrypt: ready"

IP=$(az network public-ip show --subscription "$AZURE_SUBSCRIPTION_ID" -g "$RESOURCE_GROUP" -n "$PUBLIC_IP_NAME" \
  --query ipAddress -o tsv)
for _ in $(seq 60); do
  SVC_IP=$(kubectl -n traefik get svc traefik -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
  [[ "$SVC_IP" == "$IP" ]] && break
  sleep 5
done
[[ "$SVC_IP" == "$IP" ]] || { echo "Traefik Service IP '$SVC_IP' is not the static IP $IP" >&2; exit 1; }
log "Traefik public IP: $IP (add DNS record A *.lab.patoarchitekci.io -> $IP, DNS only)"
log "done"
