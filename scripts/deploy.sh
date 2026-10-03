#!/usr/bin/env bash
# Deploy the shop into one registered namespace, healthy or with one breakage.
#
#   scripts/deploy.sh <namespace> --version 1.0.N [--break <type>] [--dry-run]
#
# Writes a temporary kustomize overlay (Namespace with the registered label,
# Ingress host <namespace>.lab.patoarchitekci.io, image tag, breakage
# component) and runs `kubectl apply -k`. --dry-run prints the rendered
# manifests and does not contact the cluster.
set -euo pipefail

# shellcheck source-path=SCRIPTDIR source=lib.sh
source "$(dirname "$0")/lib.sh"

DOMAIN=lab.patoarchitekci.io
LABEL_KEY=lab.protopia.tech/target
IMAGE=ghcr.io/protopiatech-labs/shop
BREAKS="$(cd "$DEPLOY_DIR/components" && echo *)"

usage() {
  echo "usage: $0 <namespace> --version 1.0.N [--break <type>] [--dry-run]" >&2
  echo "break types: $BREAKS" >&2
  exit 2
}

ns="" version="" brk="" dry_run=false
while [ $# -gt 0 ]; do
  case "$1" in
    --version) [ $# -ge 2 ] || usage; version="$2"; shift 2 ;;
    --break) [ $# -ge 2 ] || usage; brk="$2"; shift 2 ;;
    --dry-run) dry_run=true; shift ;;
    -h|--help) usage ;;
    -*) echo "unknown option: $1" >&2; usage ;;
    *) [ -z "$ns" ] || usage; ns="$1"; shift ;;
  esac
done
[ -n "$ns" ] || usage
check_version "$version"
[[ "$ns" =~ ^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$ ]] || die "invalid namespace '$ns' (not a DNS label)"
case "$ns" in
  default|kube-*|lab-*|cert-manager|traefik) die "namespace '$ns' is reserved" ;;
esac
if [ -n "$brk" ] && [ ! -d "$DEPLOY_DIR/components/$brk" ]; then
  die "unknown break type '$brk' (one of: $BREAKS)"
fi

if ! $dry_run; then
  # The namespace must be new or already registered.
  existing="$(kubectl get namespace "$ns" --ignore-not-found -o name)"
  if [ -n "$existing" ]; then
    label="$(kubectl get namespace "$ns" -o jsonpath="{.metadata.labels.${LABEL_KEY//./\\.}}")"
    [ "$label" = "true" ] || die "namespace '$ns' exists and is not labelled $LABEL_KEY=true"
  fi
fi

# The image tag is set one level below the component, so bad-image keeps its tag.
new_overlay
mkdir "$tmp/version"

cat > "$tmp/version/kustomization.yaml" <<EOF
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../../base
images:
  - name: $IMAGE
    newTag: "$version"
EOF

cat > "$tmp/namespace.yaml" <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: $ns
  labels:
    $LABEL_KEY: "true"
EOF

host="$ns.$DOMAIN"
{
  cat <<EOF
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: $ns
resources:
  - namespace.yaml
  - version
patches:
  - target:
      kind: Ingress
      name: orders
    patch: |-
      - op: replace
        path: /spec/rules/0/host
        value: $host
      - op: replace
        path: /spec/tls/0/hosts/0
        value: $host
EOF
  if [ -n "$brk" ]; then
    printf 'components:\n  - ../components/%s\n' "$brk"
  fi
} > "$tmp/kustomization.yaml"

$dry_run || echo "deploying $IMAGE:$version to $ns (break: ${brk:-none})"
run_overlay "$dry_run"
