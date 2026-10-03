#!/usr/bin/env bash
# Deploy loadgen into lab-loadgen.
#
#   scripts/deploy-loadgen.sh --version 1.0.N [--dry-run]
set -euo pipefail

# shellcheck source-path=SCRIPTDIR source=lib.sh
source "$(dirname "$0")/lib.sh"

version="" dry_run=false
while [ $# -gt 0 ]; do
  case "$1" in
    --version) [ $# -ge 2 ] || { echo "--version needs a value" >&2; exit 2; }; version="$2"; shift 2 ;;
    --dry-run) dry_run=true; shift ;;
    *) echo "usage: $0 --version 1.0.N [--dry-run]" >&2; exit 2 ;;
  esac
done
check_version "$version"

new_overlay
cat > "$tmp/kustomization.yaml" <<EOT
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../loadgen
images:
  - name: ghcr.io/protopiatech-labs/shop
    newTag: "$version"
EOT

run_overlay "$dry_run"
