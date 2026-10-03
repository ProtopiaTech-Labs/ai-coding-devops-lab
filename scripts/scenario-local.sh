#!/usr/bin/env bash
# Given/When/Then scenario for the shop (T05) and loadgen (T06) through docker compose.
# Usage: [ORDERS_PORT=18080] [INVENTORY_PORT=8081] [PAYMENTS_PORT=8082] scripts/scenario-local.sh
# Needs docker (compose v2), curl, jq. Exits non-zero on the first failed step.
set -euo pipefail
cd "$(dirname "$0")/.."

ORDERS="http://localhost:${ORDERS_PORT:-8080}"
INVENTORY="http://localhost:${INVENTORY_PORT:-8081}"
PAYMENTS="http://localhost:${PAYMENTS_PORT:-8082}"

dc() { docker compose "$@"; }
cleanup() { dc down --remove-orphans >/dev/null 2>&1 || true; }
trap cleanup EXIT

step() { # step "description" command...
  local name=$1; shift
  if "$@"; then echo "PASS  $name"; else echo "FAIL  $name"; exit 1; fi
}
retry() { # retry N command...: try once a second, N times
  local n=$1; shift
  for ((i = 0; i < n; i++)); do "$@" && return 0; sleep 1; done
  return 1
}
code() { curl -s -o /dev/null -w '%{http_code}' -X "$1" "$2" || true; } # 000 on connection error
is() { [[ "$(code "$2" "$3")" == "$1" ]]; }                            # is STATUS METHOD URL
all_ready() { is 200 GET "$ORDERS/readyz" && is 200 GET "$INVENTORY/readyz" && is 200 GET "$PAYMENTS/readyz"; }
all_healthy() { is 200 GET "$ORDERS/healthz" && is 200 GET "$INVENTORY/healthz" && is 200 GET "$PAYMENTS/healthz"; }
logs() { dc logs --no-log-prefix "$@" 2>/dev/null; } # logs [--since T] [service]
now() { date -u +%Y-%m-%dT%H:%M:%SZ; }
cid() { dc ps -aq "$1"; }
state() { docker inspect -f "$2" "$(cid "$1")"; } # state SERVICE GO-TEMPLATE
exited() { [[ "$(state "$1" '{{.State.Status}}')" == exited ]]; }

order_201_with_versions() {
  local out
  out=$(curl -s -w '\n%{http_code}' -X POST "$ORDERS/orders") || return 1
  [[ "$(tail -n1 <<<"$out")" == 201 ]] || return 1
  sed '$d' <<<"$out" | jq -e '[.versions.orders, .versions.inventory, .versions.payments]
    | all(type == "string" and length > 0)' >/dev/null
}
every_log_line_is_json() { # every line of every service is a JSON object
  local out
  out=$(logs) && [[ -n "$out" ]] || return 1
  jq -R -e 'fromjson | type == "object"' <<<"$out" >/dev/null 2>&1
}
loadgen_shows() { # loadgen_shows SINCE JQ-FILTER
  logs --since "$1" loadgen | jq -s -e "any(.[]; .msg == \"order\" and ($2))" >/dev/null 2>&1
}
log_has() { # log_has SERVICE SINCE JQ-FILTER
  logs --since "$2" "$1" | jq -s -e "any(.[]; $3)" >/dev/null 2>&1
}

cleanup
echo "Given the stack is built and started (orders $ORDERS, inventory $INVENTORY, payments $PAYMENTS)"
dc up -d --build --quiet-pull >/dev/null 2>&1 || { echo "FAIL  docker compose up"; exit 1; }
start=$(now)

step "Given the three services run, when I poll /readyz (30 s), then all return 200" retry 30 all_ready
step "Then /healthz of orders, inventory, payments returns 200" all_healthy

step "Image shop:local is under 15 MB ($(($(docker image inspect shop:local -f '{{.Size}}') / 1024)) KiB)" \
  test "$(docker image inspect shop:local -f '{{.Size}}')" -lt $((15 * 1024 * 1024))

step "When I send POST /orders, then I get 201 with the versions of orders, inventory, payments" order_201_with_versions
step "Then the loadgen log shows 201s with three versions" \
  retry 10 loadgen_shows "$start" '.status == 201 and (.versions | length) == 3 and .latency_ms >= 0'
logs loadgen | jq -c 'select(.msg == "order")' | tail -n1 | sed 's/^/      /'
step "Then every log line of every service parses as JSON" every_log_line_is_json

echo "When payments stops"
since=$(now)
dc stop payments >/dev/null 2>&1
step "Then POST /orders returns 502" is 502 POST "$ORDERS/orders"
step "Then the loadgen log shows 502s" retry 10 loadgen_shows "$since" '.status == 502'
logs --since "$since" loadgen | jq -c 'select(.msg == "order" and .status == 502)' | tail -n1 | sed 's/^/      /'
dc start payments >/dev/null 2>&1
step "When payments starts again, then orders returns 201 again" retry 30 order_201_with_versions

echo "When inventory is slow (POST /chaos/slow)"
step "Then /chaos/slow returns 202" is 202 POST "$INVENTORY/chaos/slow"
since=$(now)
curl -s -m 1 -o /dev/null -X POST "$INVENTORY/reserve" || true
step "Then a client that gives up after 1 s is logged by inventory as 499, canceled" \
  retry 5 log_has inventory "$since" '.path == "/reserve" and .status == 499 and .canceled == true'
step "Then POST /orders returns 504 (2 s upstream timeout)" is 504 POST "$ORDERS/orders"
dc restart inventory >/dev/null 2>&1
step "When inventory restarts, then all are ready again" retry 30 all_ready

echo "When payments is unready (POST /chaos/unready)"
step "Then /chaos/unready returns 202" is 202 POST "$PAYMENTS/chaos/unready"
step "Then payments /readyz returns 503" is 503 GET "$PAYMENTS/readyz"
step "Then payments /healthz still returns 200" is 200 GET "$PAYMENTS/healthz"
dc restart payments >/dev/null 2>&1
step "When payments restarts, then all are ready again" retry 30 all_ready

echo "When inventory crashes (POST /chaos/crash)"
step "Then /chaos/crash returns 202" is 202 POST "$INVENTORY/chaos/crash"
step "Then the inventory container exits" retry 10 exited inventory
step "Then its exit code is 1" test "$(state inventory '{{.State.ExitCode}}')" = 1

echo "When payments runs out of memory (POST /chaos/oom, mem_limit 128m)"
step "Then /chaos/oom returns 202" is 202 POST "$PAYMENTS/chaos/oom"
step "Then the payments container exits" retry 30 exited payments
step "Then Docker reports OOMKilled=true (exit $(state payments '{{.State.ExitCode}}'))" \
  test "$(state payments '{{.State.OOMKilled}}')" = true

step "Then every log line of every service still parses as JSON" every_log_line_is_json
echo "All steps passed."
