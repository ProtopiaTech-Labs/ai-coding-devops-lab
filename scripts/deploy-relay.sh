#!/usr/bin/env bash
# Deploy the relay into lab-relay.
#
#   scripts/deploy-relay.sh --version 1.0.N [--dry-run]
#
# Reads participants.txt (repo root) and RELAY_ADMIN_KEY, GITHUB_WEBHOOK_SECRET
# from .env, writes ConfigMap relay-participants and Secret relay-admin, applies
# deploy/relay with the image tag and waits for the rollout. A checksum of the
# ConfigMap and Secret content sits on the pod template, so a change restarts
# the pod and a second run with the same input changes nothing.
set -euo pipefail

# shellcheck source-path=SCRIPTDIR source=lib.sh
source "$(dirname "$0")/lib.sh"

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
NS=lab-relay
version="" dry_run=false
while [ $# -gt 0 ]; do
  case "$1" in
    --version) [ $# -ge 2 ] || { echo "--version needs a value" >&2; exit 2; }; version="$2"; shift 2 ;;
    --dry-run) dry_run=true; shift ;;
    *) echo "usage: $0 --version 1.0.N [--dry-run]" >&2; exit 2 ;;
  esac
done
check_version "$version"

# shellcheck source=/dev/null
source "$ROOT/.env"
: "${RELAY_ADMIN_KEY:?set in .env}" "${GITHUB_WEBHOOK_SECRET:?set in .env}"
PARTICIPANTS="$ROOT/participants.txt"
[ -s "$PARTICIPANTS" ] || die "$PARTICIPANTS is missing"

checksum=$( { cat "$PARTICIPANTS"; printf '%s\n%s\n' "$RELAY_ADMIN_KEY" "$GITHUB_WEBHOOK_SECRET"; } \
  | shasum -a 256 | cut -c1-16)

new_overlay
cat > "$tmp/kustomization.yaml" <<EOT
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../relay
images:
  - name: ghcr.io/protopiatech-labs/relay
    newTag: "$version"
patches:
  - target:
      kind: Deployment
      name: relay
    patch: |-
      - op: add
        path: /spec/template/metadata/annotations
        value:
          lab.protopia.tech/config-checksum: "$checksum"
EOT

if $dry_run; then
  run_overlay true
  exit 0
fi

# Namespace first: the ConfigMap and Secret live in it.
kubectl apply -f "$DEPLOY_DIR/relay/namespace.yaml"
kubectl -n "$NS" create configmap relay-participants --from-file=participants.txt="$PARTICIPANTS" \
  --dry-run=client -o yaml | kubectl apply -f -
# Values go through files (process substitution), never through argv.
kubectl -n "$NS" create secret generic relay-admin \
  --from-file=admin-key=<(printf '%s' "$RELAY_ADMIN_KEY") \
  --from-file=webhook-secret=<(printf '%s' "$GITHUB_WEBHOOK_SECRET") \
  --dry-run=client -o yaml | kubectl apply -f -
run_overlay false
kubectl -n "$NS" rollout status deploy/relay --timeout=5m
