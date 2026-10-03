# Shared helpers, sourced by the lab scripts (not executable on its own).
# shellcheck shell=bash

DEPLOY_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../deploy" && pwd)"

# Filled by load_participants and new_overlay.
ids=() names=() keys=() tmp=""

die() { echo "$(basename "$0"): $*" >&2; exit 1; }
log() { echo "==> $*"; }

# load_participants <file>: fills the arrays ids, names, keys from lines
# "<pXX>,<display name>: <api key>"; comments and blank lines are skipped.
load_participants() {
  local line id
  [ -f "$1" ] || die "no such file: $1"
  ids=() names=() keys=()
  while IFS= read -r line || [ -n "$line" ]; do
    [[ "$line" =~ ^[[:space:]]*(#|$) ]] && continue
    [[ "$line" =~ ^[[:space:]]*(p[0-9]{2}),[[:space:]]*(.+):[[:space:]]*([^[:space:]]+)[[:space:]]*$ ]] \
      || die "bad line (expected '<pXX>,<name>: <key>'): ${line%%,*},..."
    id="${BASH_REMATCH[1]}"
    for seen in "${ids[@]+"${ids[@]}"}"; do [ "$seen" != "$id" ] || die "duplicate id $id"; done
    ids+=("$id") names+=("${BASH_REMATCH[2]}") keys+=("${BASH_REMATCH[3]}")
  done < "$1"
  [ "${#ids[@]}" -gt 0 ] || die "no participants in $1"
}

# check_version <value>: dies unless it looks like 1.0.N.
check_version() {
  [[ "$1" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "--version 1.0.N is required (got '${1:-}')"
}

# new_overlay: creates the temporary kustomize overlay dir in $tmp and removes it on exit.
# It lives under deploy/ because kustomize accepts only relative paths.
new_overlay() {
  tmp="$(mktemp -d "$DEPLOY_DIR/.overlay.XXXXXX")"
  trap 'rm -rf "$tmp"' EXIT
}

# run_overlay <dry_run: true|false>: prints the rendered overlay or applies it.
run_overlay() {
  if [ "$1" = true ]; then kubectl kustomize "$tmp"; else kubectl apply -k "$tmp"; fi
}
