#!/usr/bin/env bash
# End-to-end checks against a running demo (`make demo`). Used by CI.
#
# Verifies that the gateway enforces authentication, scopes and tenant
# isolation, and that data and alerts flow from the simulated tenants.
set -euo pipefail

GATEWAY="${GATEWAY:-http://127.0.0.1:8080}"
GRAFANA="${GRAFANA:-http://127.0.0.1:3000}"
TIMEOUT="${TIMEOUT:-180}"   # seconds to wait for data to show up

# Demo tokens, see deploy/compose/.env.
ACME_AGENT=demo-acme-prod-agent
ACME_READER=demo-acme-prod-reader
OPS=demo-ops

failures=0
pass() { printf '  \033[32mok\033[0m    %s\n' "$1"; }
fail() { printf '  \033[31mFAIL\033[0m  %s\n' "$1"; failures=$((failures + 1)); }

# status METHOD PATH [TOKEN] [TENANT] -> prints the HTTP status code
status() {
  local args=(-s -o /dev/null -w '%{http_code}' -X "$1" "$GATEWAY$2")
  [[ -n "${3:-}" ]] && args+=(-H "Authorization: Bearer $3")
  [[ -n "${4:-}" ]] && args+=(-H "X-Scope-OrgID: $4")
  curl "${args[@]}"
}

expect_status() {
  local want=$1 desc=$2; shift 2
  local got
  got="$(status "$@")"
  if [[ "$got" == "$want" ]]; then pass "$desc"; else fail "$desc (got $got, want $want)"; fi
}

# query TOKEN TENANT PROMQL -> number of series in the result
query() {
  curl -s -H "Authorization: Bearer $1" ${2:+-H "X-Scope-OrgID: $2"} \
    --data-urlencode "query=$3" "$GATEWAY/prometheus/api/v1/query" \
    | jq -r '.data.result | length' 2>/dev/null || echo 0
}

# eventually DESC TOKEN TENANT PROMQL -> waits until the query returns series
eventually() {
  local desc=$1 deadline=$((SECONDS + TIMEOUT))
  while (( SECONDS < deadline )); do
    if (( $(query "$2" "$3" "$4") > 0 )); then pass "$desc"; return; fi
    sleep 5
  done
  fail "$desc (no data after ${TIMEOUT}s)"
}

echo "Gateway: authentication and routing"
expect_status 401 "push without a token is rejected"          POST /api/v1/push
expect_status 401 "push with an unknown token is rejected"    POST /api/v1/push not-a-token
expect_status 404 "Mimir admin endpoints are not exposed"     POST /ingester/shutdown "$OPS" acme-prod
expect_status 404 "Mimir config endpoint is not exposed"      GET  /config "$OPS" acme-prod

echo "Gateway: scopes and tenant isolation"
expect_status 403 "an agent token cannot read"                GET /prometheus/api/v1/query?query=up "$ACME_AGENT"
expect_status 403 "a tenant cannot read another tenant"       GET /prometheus/api/v1/query?query=up "$ACME_READER" globex-prod
expect_status 403 "a reader cannot change rules"              POST /prometheus/config/v1/rules/test "$ACME_READER"
expect_status 200 "a reader can query its own tenant"         GET /prometheus/api/v1/query?query=up "$ACME_READER"

echo "Data flow"
eventually "ACME host metrics are ingested"        "$ACME_READER" ""  'node_uname_info'
eventually "Globex alerts are forwarded"           "$OPS" globex-prod 'ALERTS{alertname="PaymentsQueueBacklog"}'
eventually "platform self-monitoring is ingested"  "$OPS" platform    'up{job="observability/tenant-gateway"} == 1'
eventually "central rules fire for a silent tenant" "$OPS" acme-staging 'ALERTS{alertname="TenantNoData", alertstate="firing"}'
eventually "federated query spans tenants"         "$OPS" 'acme-staging|globex-prod' 'count by (__tenant_id__) (ALERTS)'

echo "Grafana"
if [[ "$(curl -s "$GRAFANA/api/health" | jq -r .database)" == "ok" ]]; then
  pass "Grafana is healthy"
else
  fail "Grafana is not healthy"
fi
datasources="$(curl -s -u "admin:${GRAFANA_ADMIN_PASSWORD:-admin}" "$GRAFANA/api/datasources" | jq length)"
if (( datasources >= 9 )); then pass "tenant datasources are provisioned ($datasources)"; else fail "datasources provisioned: $datasources"; fi

echo
if (( failures > 0 )); then
  echo "$failures check(s) failed"
  exit 1
fi
echo "All checks passed"
