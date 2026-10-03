#!/usr/bin/env bash
# --cluster: delete the AKS cluster only (the public IP and the identity stay).
# --all: delete the whole resource group, after confirmation.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
# shellcheck source=/dev/null
source "$ROOT/.env"
: "${AZURE_SUBSCRIPTION_ID:?set in .env}"
SUB=(--subscription "$AZURE_SUBSCRIPTION_ID")

case "${1:-}" in
  --cluster)
    az aks delete "${SUB[@]}" -g "$RESOURCE_GROUP" -n "$AKS_NAME" --yes
    ;;
  --all)
    read -r -p "Delete resource group $RESOURCE_GROUP and everything in it? Type 'yes': " answer
    [[ "$answer" == "yes" ]] || { echo "Aborted."; exit 1; }
    az group delete "${SUB[@]}" -n "$RESOURCE_GROUP" --yes
    ;;
  *)
    echo "Usage: $0 --cluster | --all" >&2
    exit 2
    ;;
esac
