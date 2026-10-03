#!/usr/bin/env bash
# Participant access: for every id in participants.txt, the pXX-portal namespace
# (ServiceAccounts portal and deployer, quota, tokens), the registered pXX-demo
# namespace, and a card directory with two kubeconfigs and card.md.
#
#   scripts/participants.sh --participants <file> [--cards <dir>] [--vap-mode warn|deny] [--delete]
#
# Idempotent: everything goes through `kubectl apply`, a second run prints `unchanged`.
# Tokens are written only into the card files, never to stdout.
# --vap-mode defaults to deny. warn is for testing only: cards are written in deny mode only.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DIR="$ROOT/deploy/participants"
DOMAIN=lab.patoarchitekci.io

usage() {
  echo "usage: $0 --participants <file> [--cards <dir>] [--vap-mode warn|deny] [--delete]" >&2
  exit 2
}
die() { echo "participants.sh: $*" >&2; exit 1; }
log() { echo "==> $*"; }

file="" cards="$ROOT/../training-ai-coding-devops/instructor/cards" mode=deny delete=false
while [ $# -gt 0 ]; do
  case "$1" in
    --participants) [ $# -ge 2 ] || usage; file="$2"; shift 2 ;;
    --cards) [ $# -ge 2 ] || usage; cards="$2"; shift 2 ;;
    --vap-mode) [ $# -ge 2 ] || usage; mode="$2"; shift 2 ;;
    --delete) delete=true; shift ;;
    *) usage ;;
  esac
done
[ -n "$file" ] || usage
[ -f "$file" ] || die "no such file: $file"
case "$mode" in
  warn) actions="[Warn, Audit]" ;;
  deny) actions="[Deny]" ;;
  *) die "--vap-mode must be warn or deny" ;;
esac

# Lines "<id>,<display name>: <api key>"; comments and blank lines are skipped.
ids=() names=() keys=()
while IFS= read -r line || [ -n "$line" ]; do
  [[ "$line" =~ ^[[:space:]]*(#|$) ]] && continue
  [[ "$line" =~ ^[[:space:]]*(p[0-9]{2}),[[:space:]]*(.+):[[:space:]]*([^[:space:]]+)[[:space:]]*$ ]] \
    || die "bad line (expected '<pXX>,<name>: <key>'): ${line%%,*},..."
  id="${BASH_REMATCH[1]}"
  for seen in "${ids[@]+"${ids[@]}"}"; do [ "$seen" != "$id" ] || die "duplicate id $id"; done
  ids+=("$id") names+=("${BASH_REMATCH[2]}") keys+=("${BASH_REMATCH[3]}")
done < "$file"
[ "${#ids[@]}" -gt 0 ] || die "no participants in $file"

render() { local t; t="$(<"$DIR/participant.yaml")"; printf '%s\n' "${t//\$\{ID\}/$1}"; }

if $delete; then
  echo "This deletes ${ids[*]}: namespaces <id>-portal and <id>-demo with everything in them, and the portal bindings."
  read -r -p "Type yes to continue: " answer
  [ "$answer" = "yes" ] || die "aborted"
  for id in "${ids[@]}"; do
    log "$id: delete"
    render "$id" | kubectl delete --ignore-not-found -f -
  done
  exit 0
fi

log "policy and ClusterRole (vap mode: $mode)"
kubectl apply -f "$DIR/clusterrole-portal.yaml"
sed "s/^\(  validationActions:\).*/\1 $actions/" "$DIR/vap.yaml" | kubectl apply -f -

server="$(kubectl config view --minify --raw -o jsonpath='{.clusters[0].cluster.server}')"
ca="$(kubectl config view --minify --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')"
[ -n "$server" ] && [ -n "$ca" ] || die "current context has no server or certificate-authority-data"

# $1 id, $2 ServiceAccount, $3 output file; the token never leaves the file.
write_kubeconfig() {
  local token i
  for i in $(seq 1 30); do
    token="$(kubectl -n "$1-portal" get secret "$2-token" -o jsonpath='{.data.token}' | base64 -d)"
    [ -n "$token" ] && break
    sleep 1
  done
  [ -n "$token" ] || die "$1: no token in Secret $2-token"
  (umask 077; cat > "$3" <<EOF
apiVersion: v1
kind: Config
clusters:
  - name: lab
    cluster:
      server: $server
      certificate-authority-data: $ca
users:
  - name: $1-$2
    user:
      token: $token
contexts:
  - name: $1-$2
    context:
      cluster: lab
      user: $1-$2
      namespace: $1-portal
current-context: $1-$2
EOF
  )
}

if [ "$mode" != deny ]; then
  echo "WARNING: --vap-mode $mode: cards are only written in deny mode, skipping cards" >&2
fi

for i in "${!ids[@]}"; do
  id="${ids[$i]}" name="${names[$i]}" key="${keys[$i]}"
  log "$id: apply"
  render "$id" | kubectl apply -f -

  [ "$mode" = deny ] || continue
  out="$cards/$id"
  (umask 077; mkdir -p "$out")
  write_kubeconfig "$id" portal "$out/portal.kubeconfig"
  write_kubeconfig "$id" deployer "$out/deployer.kubeconfig"
  (umask 077; cat > "$out/card.md" <<EOF
# $id · $name

- Relay: https://relay.$DOMAIN, API key \`$key\`
- Portal host: \`$id.$DOMAIN\`, namespace \`$id-portal\`
- Demo shop: \`https://$id-demo.$DOMAIN\`, namespace \`$id-demo\`
- Your namespaces: \`$id-*\`, created by your portal with the label \`lab.protopia.tech/target=true\`
- \`deployer.kubeconfig\`: \`admin\` in \`$id-portal\` only; a secret in your fork for the portal deploy
- \`portal.kubeconfig\`: the portal's ServiceAccount; creates and labels \`$id-*\` namespaces, their quotas and limit ranges, reads deployments, pods and events
EOF
  )
  log "$id: card in $out"
done
