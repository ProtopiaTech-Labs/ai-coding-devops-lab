#!/usr/bin/env bash
# Chaos runner: every --interval seconds, deploy a random existing version into a
# random registered namespace through the deploy workflow, sometimes with a breakage.
#
#   scripts/chaos-runner.sh [--interval S] [--iterations N] [--break-probability P]
#                           [--seed N] [--namespaces a,b] [--dry-run]
#
# Registered namespaces come from `kubectl get ns -l lab.protopia.tech/target=true`
# (or --namespaces). Versions are the tags of ghcr.io/protopiatech-labs/shop, read
# from the public registry API with an anonymous token (no gh token scope needed).
# --dry-run prints the choices, never runs the workflow or sleeps, and stops after
# --iterations (default 5). Ctrl+C stops it.
set -euo pipefail

REPO=ProtopiaTech-Labs/ai-coding-devops-lab
LABEL=lab.protopia.tech/target=true
PACKAGE=protopiatech-labs/shop
# Same list as the `break` input of .github/workflows/deploy.yml.
BREAKS=(bad-image crash dependency-down missing-config oom quota slow unready)

usage() {
  echo "usage: $0 [--interval S] [--iterations N] [--break-probability P] [--seed N] [--namespaces a,b] [--dry-run]" >&2
  exit 2
}
die() { echo "chaos-runner.sh: $*" >&2; exit 1; }

interval=60 iterations="" probability=0.3 seed="" namespaces="" dry_run=false
while [ $# -gt 0 ]; do
  case "$1" in
    --interval) [ $# -ge 2 ] || usage; interval="$2"; shift 2 ;;
    --iterations) [ $# -ge 2 ] || usage; iterations="$2"; shift 2 ;;
    --break-probability) [ $# -ge 2 ] || usage; probability="$2"; shift 2 ;;
    --seed) [ $# -ge 2 ] || usage; seed="$2"; shift 2 ;;
    --namespaces) [ $# -ge 2 ] || usage; namespaces="$2"; shift 2 ;;
    --dry-run) dry_run=true; shift ;;
    -h|--help) usage ;;
    *) echo "unknown option: $1" >&2; usage ;;
  esac
done

[[ "$interval" =~ ^[1-9][0-9]*$ ]] || die "--interval must be a positive integer"
[ -z "$iterations" ] || [[ "$iterations" =~ ^[1-9][0-9]*$ ]] || die "--iterations must be a positive integer"
[ -z "$seed" ] || [[ "$seed" =~ ^[0-9]+$ ]] || die "--seed must be a non-negative integer"
[[ "$probability" =~ ^(0(\.[0-9]+)?|1(\.0*)?|\.[0-9]+)$ ]] || die "--break-probability must be between 0 and 1"
# Probability in thousandths, compared with RANDOM % 1000.
permille="$(awk -v p="$probability" 'BEGIN { printf "%d", p * 1000 + 0.5 }')"
if $dry_run && [ -z "$iterations" ]; then iterations=5; fi
if [ -n "$seed" ]; then RANDOM=$seed; fi

list_namespaces() {
  if [ -n "$namespaces" ]; then
    tr ',' '\n' <<< "$namespaces" | sed '/^$/d'
  else
    kubectl get ns -l "$LABEL" -o name | sed 's|^namespace/||'
  fi
}

list_versions() {
  local token
  token="$(curl -fsS "https://ghcr.io/token?scope=repository:$PACKAGE:pull" | jq -r .token)"
  curl -fsS -H "Authorization: Bearer $token" "https://ghcr.io/v2/$PACKAGE/tags/list" \
    | jq -r '.tags[]' | grep -E '^[0-9]+\.[0-9]+\.[0-9]+$' | sort -V
}

trap 'echo "$(date "+%F %T") stopped"; exit 0' INT TERM

i=0
while [ -z "$iterations" ] || [ "$i" -lt "$iterations" ]; do
  i=$((i + 1))
  now="$(date "+%F %T")"
  nss=() versions=()
  while IFS= read -r line; do nss+=("$line"); done < <(list_namespaces)
  while IFS= read -r line; do versions+=("$line"); done < <(list_versions)

  if [ "${#nss[@]}" -eq 0 ]; then
    echo "$now no registered namespaces, waiting"
  elif [ "${#versions[@]}" -eq 0 ]; then
    echo "$now no versions in ghcr.io/$PACKAGE, waiting"
  else
    ns="${nss[RANDOM % ${#nss[@]}]}"
    version="${versions[RANDOM % ${#versions[@]}]}"
    brk=none
    if [ $((RANDOM % 1000)) -lt "$permille" ]; then brk="${BREAKS[RANDOM % ${#BREAKS[@]}]}"; fi

    if $dry_run; then
      echo "$i namespace=$ns version=$version break=$brk (dry run)"
    else
      # gh prints the run URL on success; keep the last line only.
      if out="$(gh workflow run deploy.yml -R "$REPO" -f namespace="$ns" -f version="$version" -f break="$brk" 2>&1)"; then
        echo "$now namespace=$ns version=$version break=$brk ${out##*$'\n'}"
      else
        echo "$now namespace=$ns version=$version break=$brk FAILED: ${out//$'\n'/ }"
      fi
    fi
  fi

  if ! $dry_run && { [ -z "$iterations" ] || [ "$i" -lt "$iterations" ]; }; then
    # Background sleep + wait, so a TERM stops the runner at once.
    sleep "$interval" & wait $!
  fi
done
