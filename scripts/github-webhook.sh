#!/usr/bin/env bash
# Create or update the deployment_status webhook of GITHUB_REPO that points at
# the relay, with GITHUB_WEBHOOK_SECRET from .env. Idempotent: the hook is found
# by its URL and updated in place. Prints the hook id.
#
#   scripts/github-webhook.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
# shellcheck source=/dev/null
source "$ROOT/.env"
: "${GITHUB_REPO:?set in .env}" "${GITHUB_WEBHOOK_SECRET:?set in .env}"
URL=https://relay.lab.patoarchitekci.io/webhook/github

id=$(gh api "repos/$GITHUB_REPO/hooks" --paginate --jq ".[] | select(.config.url == \"$URL\") | .id" | head -1)
# The secret goes through stdin, never through argv.
body=$(URL="$URL" GITHUB_WEBHOOK_SECRET="$GITHUB_WEBHOOK_SECRET" jq -n '{active: true, events: ["deployment_status"],
  config: {url: env.URL, content_type: "json", insecure_ssl: "0", secret: env.GITHUB_WEBHOOK_SECRET}}')
if [ -n "$id" ]; then
  gh api -X PATCH "repos/$GITHUB_REPO/hooks/$id" --input - <<<"$body" >/dev/null
  echo "==> hook $id updated"
else
  id=$(gh api -X POST "repos/$GITHUB_REPO/hooks" --input - --jq .id <<<"$(jq '. + {name: "web"}' <<<"$body")")
  echo "==> hook $id created"
fi
echo "$id"
