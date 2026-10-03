#!/usr/bin/env bash
# Creates the lab AKS cluster and the GitHub Actions identity. Idempotent: a second run changes nothing.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
# shellcheck source=/dev/null
source "$ROOT/.env"

: "${AZURE_SUBSCRIPTION_ID:?set in .env}" "${AZURE_TENANT_ID:?set in .env}"
SUB=(--subscription "$AZURE_SUBSCRIPTION_ID")

log() { echo "==> $*"; }

# $1 assignee object ID, $2 principal type, $3 role, $4 scope
ensure_role() {
  local count
  count=$(az role assignment list "${SUB[@]}" --role "$3" --scope "$4" \
    --query "length([?principalId=='$1'])" -o tsv)
  if [[ "$count" -gt 0 ]]; then
    log "role '$3' for $1: exists"
  else
    log "role '$3' for $1: create"
    az role assignment create "${SUB[@]}" --assignee-object-id "$1" --assignee-principal-type "$2" \
      --role "$3" --scope "$4" -o none
  fi
}

# $1 name, $2 value
ensure_gh_variable() {
  if [[ "$(gh variable get "$1" -R "$GITHUB_REPO" 2>/dev/null || true)" == "$2" ]]; then
    log "GitHub variable $1: unchanged"
  else
    log "GitHub variable $1: set"
    gh variable set "$1" -R "$GITHUB_REPO" --body "$2"
  fi
}

if az group show "${SUB[@]}" -n "$RESOURCE_GROUP" -o none 2>/dev/null; then
  log "resource group $RESOURCE_GROUP: exists"
else
  log "resource group $RESOURCE_GROUP: create"
  az group create "${SUB[@]}" -n "$RESOURCE_GROUP" -l "$LOCATION" -o none
fi
RG_ID=$(az group show "${SUB[@]}" -n "$RESOURCE_GROUP" --query id -o tsv)

if az network public-ip show "${SUB[@]}" -g "$RESOURCE_GROUP" -n "$PUBLIC_IP_NAME" -o none 2>/dev/null; then
  log "public IP $PUBLIC_IP_NAME: exists"
else
  log "public IP $PUBLIC_IP_NAME: create"
  az network public-ip create "${SUB[@]}" -g "$RESOURCE_GROUP" -n "$PUBLIC_IP_NAME" -l "$LOCATION" \
    --sku Standard --allocation-method Static -o none
fi

if az aks show "${SUB[@]}" -g "$RESOURCE_GROUP" -n "$AKS_NAME" -o none 2>/dev/null; then
  log "AKS $AKS_NAME: exists"
else
  log "AKS $AKS_NAME: create (about 10 minutes)"
  az aks create "${SUB[@]}" -g "$RESOURCE_GROUP" -n "$AKS_NAME" -l "$LOCATION" \
    --kubernetes-version "$K8S_VERSION" \
    --node-count "$NODE_COUNT" --node-vm-size "$NODE_VM_SIZE" \
    --tier free \
    --enable-aad --enable-azure-rbac --disable-local-accounts \
    --no-ssh-key -o none
fi
AKS_ID=$(az aks show "${SUB[@]}" -g "$RESOURCE_GROUP" -n "$AKS_NAME" --query id -o tsv)
AKS_PRINCIPAL_ID=$(az aks show "${SUB[@]}" -g "$RESOURCE_GROUP" -n "$AKS_NAME" --query identity.principalId -o tsv)

# $@ az get-access-token arguments; prints the `oid` claim of the token
token_oid() {
  az account get-access-token "$@" --query accessToken -o tsv \
    | cut -d. -f2 | tr '_-' '/+' | awk '{ while (length($0) % 4) $0 = $0 "="; print }' | base64 -d | jq -r .oid
}

# The owner is the account that owns the subscription: the `oid` of an az token for the subscription.
# `az ad signed-in-user show` reads the default tenant, which can differ from the lab tenant.
OWNER_ID=$(token_oid "${SUB[@]}")
ensure_role "$OWNER_ID" User "Azure Kubernetes Service RBAC Cluster Admin" "$AKS_ID"
# kubelogin (azurecli, lab tenant) can pick a different account: the `oid` of its token for the AKS server app.
KUBECTL_USER_ID=$(token_oid --tenant "$AZURE_TENANT_ID" --resource 6dae42f8-4368-4678-94ff-3960e28e3630)
if [[ "$KUBECTL_USER_ID" != "$OWNER_ID" ]]; then
  ensure_role "$KUBECTL_USER_ID" User "Azure Kubernetes Service RBAC Cluster Admin" "$AKS_ID"
fi
ensure_role "$AKS_PRINCIPAL_ID" ServicePrincipal "Network Contributor" "$RG_ID"

if az identity show "${SUB[@]}" -g "$RESOURCE_GROUP" -n "$IDENTITY_NAME" -o none 2>/dev/null; then
  log "managed identity $IDENTITY_NAME: exists"
else
  log "managed identity $IDENTITY_NAME: create"
  az identity create "${SUB[@]}" -g "$RESOURCE_GROUP" -n "$IDENTITY_NAME" -l "$LOCATION" -o none
fi
MI_CLIENT_ID=$(az identity show "${SUB[@]}" -g "$RESOURCE_GROUP" -n "$IDENTITY_NAME" --query clientId -o tsv)
MI_PRINCIPAL_ID=$(az identity show "${SUB[@]}" -g "$RESOURCE_GROUP" -n "$IDENTITY_NAME" --query principalId -o tsv)
ensure_role "$MI_PRINCIPAL_ID" ServicePrincipal "Azure Kubernetes Service RBAC Cluster Admin" "$AKS_ID"
ensure_role "$MI_PRINCIPAL_ID" ServicePrincipal "Azure Kubernetes Service Cluster User Role" "$AKS_ID"

if gh api "repos/$GITHUB_REPO/environments/$GITHUB_ENVIRONMENT" --silent 2>/dev/null; then
  log "GitHub environment $GITHUB_ENVIRONMENT: exists"
else
  log "GitHub environment $GITHUB_ENVIRONMENT: create"
  gh api -X PUT "repos/$GITHUB_REPO/environments/$GITHUB_ENVIRONMENT" --silent
fi
ensure_gh_variable AZURE_CLIENT_ID "$MI_CLIENT_ID"
ensure_gh_variable AZURE_TENANT_ID "$AZURE_TENANT_ID"
ensure_gh_variable AZURE_SUBSCRIPTION_ID "$AZURE_SUBSCRIPTION_ID"

# The subject comes from a real token: run the oidc-check workflow and copy its `sub` into .env.
if [[ -z "${GHA_OIDC_SUBJECT:-}" ]]; then
  log "federated credential: skipped, GHA_OIDC_SUBJECT is empty (run the oidc-check workflow)"
else
  FC_NAME="github-${GITHUB_ENVIRONMENT}"
  CURRENT_SUBJECT=$(az identity federated-credential show "${SUB[@]}" -g "$RESOURCE_GROUP" \
    --identity-name "$IDENTITY_NAME" -n "$FC_NAME" --query subject -o tsv 2>/dev/null || true)
  if [[ "$CURRENT_SUBJECT" == "$GHA_OIDC_SUBJECT" ]]; then
    log "federated credential $FC_NAME: exists"
  else
    log "federated credential $FC_NAME: create or update"
    az identity federated-credential create "${SUB[@]}" -g "$RESOURCE_GROUP" \
      --identity-name "$IDENTITY_NAME" -n "$FC_NAME" \
      --issuer "https://token.actions.githubusercontent.com" \
      --subject "$GHA_OIDC_SUBJECT" --audiences "api://AzureADTokenExchange" -o none
  fi
fi

log "kubeconfig: get credentials and convert for kubelogin"
az aks get-credentials "${SUB[@]}" -g "$RESOURCE_GROUP" -n "$AKS_NAME" --overwrite-existing --only-show-errors
kubelogin convert-kubeconfig -l azurecli --tenant-id "$AZURE_TENANT_ID"
log "done"
