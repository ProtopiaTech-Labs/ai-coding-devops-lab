#!/usr/bin/env bash
# Deploy loadgen into lab-loadgen.
#
#   scripts/deploy-loadgen.sh --version 1.0.N [--dry-run]
set -euo pipefail

DEPLOY_DIR="$(cd "$(dirname "$0")/../deploy" && pwd)"
version="" dry_run=false
while [ $# -gt 0 ]; do
  case "$1" in
    --version) [ $# -ge 2 ] || { echo "--version needs a value" >&2; exit 2; }; version="$2"; shift 2 ;;
    --dry-run) dry_run=true; shift ;;
    *) echo "usage: $0 --version 1.0.N [--dry-run]" >&2; exit 2 ;;
  esac
done
[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "deploy-loadgen.sh: --version 1.0.N is required" >&2; exit 1; }

tmp="$(mktemp -d "$DEPLOY_DIR/.overlay.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT
cat > "$tmp/kustomization.yaml" <<EOT
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../loadgen
images:
  - name: ghcr.io/protopiatech-labs/shop
    newTag: "$version"
EOT

if $dry_run; then
  kubectl kustomize "$tmp"
else
  kubectl apply -k "$tmp"
fi
