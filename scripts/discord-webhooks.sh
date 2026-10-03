#!/usr/bin/env bash
# Discord webhooks for the lab: one webhook `lab-<id>` per participant on one channel.
#
#   scripts/discord-webhooks.sh --participants <file> --out <file> [--delete]
#
# Needs DISCORD_BOT_TOKEN and DISCORD_CHANNEL_ID (from .env or the environment); the bot needs
# View Channel and Manage Webhooks on the channel.
# Idempotent: existing `lab-<id>` webhooks are reused. Lines `<id> <webhook url>` go only into
# --out (mode 600), never to stdout. --delete removes `lab-<id>` for the listed ids.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
API=https://discord.com/api/v10

usage() {
  echo "usage: $0 --participants <file> --out <file> [--delete]" >&2
  exit 2
}
die() { echo "discord-webhooks.sh: $*" >&2; exit 1; }
log() { echo "==> $*"; }

file="" out="" delete=false
while [ $# -gt 0 ]; do
  case "$1" in
    --participants) [ $# -ge 2 ] || usage; file="$2"; shift 2 ;;
    --out) [ $# -ge 2 ] || usage; out="$2"; shift 2 ;;
    --delete) delete=true; shift ;;
    *) usage ;;
  esac
done
[ -n "$file" ] || usage
[ -f "$file" ] || die "no such file: $file"
$delete || [ -n "$out" ] || usage

if [ -f "$ROOT/.env" ]; then
  set -a
  # shellcheck disable=SC1091
  . "$ROOT/.env"
  set +a
fi
: "${DISCORD_BOT_TOKEN:?DISCORD_BOT_TOKEN is not set}"
: "${DISCORD_CHANNEL_ID:?DISCORD_CHANNEL_ID is not set}"

# Ids from lines "<id>,<anything>" (the participants.sh format); comments and blanks skipped.
ids=()
while IFS= read -r line || [ -n "$line" ]; do
  [[ "$line" =~ ^[[:space:]]*(#|$) ]] && continue
  [[ "$line" =~ ^[[:space:]]*(p[0-9]{2})[[:space:]]*, ]] || die "bad line (expected '<pXX>,...'): ${line%%,*}"
  id="${BASH_REMATCH[1]}"
  for seen in "${ids[@]+"${ids[@]}"}"; do [ "$seen" != "$id" ] || die "duplicate id $id"; done
  ids+=("$id")
done < "$file"
[ "${#ids[@]}" -gt 0 ] || die "no participants in $file"

# api METHOD PATH [JSON]: body on stdout; retries on 429; dies with the Discord message otherwise.
# The token goes through a curl config on stdin, not through argv.
api() {
  local method="$1" path="$2" data="${3:-}" resp code body wait
  local args=(-sS -X "$method" -K - -w '\n%{http_code}' "$API$path")
  [ -z "$data" ] || args+=(-H 'Content-Type: application/json' -d "$data")
  while :; do
    resp="$(printf 'header = "Authorization: Bot %s"\n' "$DISCORD_BOT_TOKEN" | curl "${args[@]}")" \
      || die "curl failed: $method $path"
    code="${resp##*$'\n'}"
    body="${resp%$'\n'*}"
    case "$code" in
      429)
        wait="$(jq -r '.retry_after // 1' <<<"$body")"
        echo "rate limited, sleeping ${wait}s" >&2
        sleep "$wait" ;;
      2??) printf '%s' "$body"; return 0 ;;
      *) die "$method $path: HTTP $code: $(jq -r '.message // .' <<<"$body" 2>/dev/null || echo "$body")" ;;
    esac
  done
}

existing="$(api GET "/channels/$DISCORD_CHANNEL_ID/webhooks")"
# id of the webhook named $1, empty if none
find_id() { jq -r --arg n "$1" '[.[] | select(.name == $n)][0].id // empty' <<<"$existing"; }

if $delete; then
  echo "This deletes the webhooks ${ids[*]/#/lab-} from channel $DISCORD_CHANNEL_ID."
  read -r -p "Type yes to continue: " answer
  [ "$answer" = "yes" ] || die "aborted"
  for id in "${ids[@]}"; do
    wid="$(find_id "lab-$id")"
    if [ -z "$wid" ]; then log "$id: none"; continue; fi
    api DELETE "/webhooks/$wid" >/dev/null
    log "$id: deleted"
  done
  exit 0
fi

(umask 077; : > "$out")
chmod 600 "$out"
created=0 reused=0
for id in "${ids[@]}"; do
  name="lab-$id"
  hook="$(jq -c --arg n "$name" '[.[] | select(.name == $n)][0] // empty' <<<"$existing")"
  if [ -n "$hook" ]; then
    reused=$((reused + 1)); state=reused
  else
    hook="$(api POST "/channels/$DISCORD_CHANNEL_ID/webhooks" "{\"name\":\"$name\"}")"
    created=$((created + 1)); state=created
  fi
  wid="$(jq -r '.id' <<<"$hook")" tok="$(jq -r '.token // empty' <<<"$hook")"
  [ -n "$tok" ] || die "$name: webhook has no token"
  echo "$id https://discord.com/api/webhooks/$wid/$tok" >> "$out"
  log "$id: $state"
done
log "created $created, reused $reused; URLs in $out"
