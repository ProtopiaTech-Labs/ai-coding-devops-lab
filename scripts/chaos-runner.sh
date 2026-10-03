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
# Before each random pick, every registered namespace without deployment/orders gets a
# healthy deploy (break=none, newest tag), logged as `bootstrap`; such a namespace is
# not picked again in that iteration. --dry-run with --namespaces cannot see the
# cluster, so it assumes no bootstrap is needed.
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
  echo "Namespaces without deployment/orders get a healthy deploy (bootstrap) before each random pick." >&2
  echo "--dry-run with --namespaces cannot see the cluster, so it assumes no bootstrap is needed." >&2
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

# $1 namespace, $2 version, $3 break, $4 label; prints one log line.
run_deploy() {
  local out
  # gh prints the run URL on success; keep the last line only.
  if out="$(gh workflow run deploy.yml -R "$REPO" -f namespace="$1" -f version="$2" -f break="$3" 2>&1)"; then
    echo "$now ${4}namespace=$1 version=$2 break=$3 ${out##*$'\n'}"
  else
    echo "$now ${4}namespace=$1 version=$2 break=$3 FAILED: ${out//$'\n'/ }"
  fi
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
    # Bootstrap: namespaces without a shop get a healthy deploy (newest tag) first.
    newest="${versions[${#versions[@]} - 1]}"
    cands=()
    for n in "${nss[@]}"; do
      if ! { $dry_run && [ -n "$namespaces" ]; } && ! kubectl -n "$n" get deployment orders >/dev/null 2>&1; then
        if $dry_run; then
          echo "$i bootstrap namespace=$n version=$newest break=none (dry run)"
        else
          run_deploy "$n" "$newest" none "bootstrap "
        fi
      else
        cands+=("$n")
      fi
    done

    if [ "${#cands[@]}" -eq 0 ]; then
      echo "$now every namespace was bootstrapped, no random pick"
    else
      ns="${cands[RANDOM % ${#cands[@]}]}"
      version="${versions[RANDOM % ${#versions[@]}]}"
      brk=none
      if [ $((RANDOM % 1000)) -lt "$permille" ]; then brk="${BREAKS[RANDOM % ${#BREAKS[@]}]}"; fi

      if $dry_run; then
        echo "$i namespace=$ns version=$version break=$brk (dry run)"
      else
        run_deploy "$ns" "$version" "$brk" ""
      fi
    fi
  fi

  if ! $dry_run && { [ -z "$iterations" ] || [ "$i" -lt "$iterations" ]; }; then
    # Background sleep + wait, so a TERM stops the runner at once.
    sleep "$interval" & wait $!
  fi
done
