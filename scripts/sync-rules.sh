#!/usr/bin/env bash
# Sync recording and alerting rules to Mimir, one tenant at a time.
#
# The plan (which rule files each tenant gets) is rendered from
# tenants/tenants.yaml into rules/sync-plan.tsv by `make generate`. Rule files
# are plain Prometheus rule files; the Mimir namespace is the rule set name.
#
# The sync is declarative: rule groups that exist in Mimir but not in Git are
# deleted from that tenant.
#
# Usage:
#   MIMIR_ADDRESS=https://metrics.example.com MIMIR_AUTH_TOKEN=... scripts/sync-rules.sh [--diff]
#
#   --diff   show what would change without applying it
#
# The token needs the rules:read and rules:write scopes for every tenant.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
plan="$root/rules/sync-plan.tsv"
action=sync
[[ "${1:-}" == "--diff" ]] && action="diff"

: "${MIMIR_ADDRESS:?set MIMIR_ADDRESS to the gateway URL}"
: "${MIMIR_AUTH_TOKEN:?set MIMIR_AUTH_TOKEN to a token with rules:write}"
MIMIRTOOL_IMAGE="${MIMIRTOOL_IMAGE:-grafana/mimirtool:3.2.2}"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# Use a local mimirtool when available, otherwise the container image.
mimirtool() {
  if type -P mimirtool >/dev/null; then
    command mimirtool "$@"
  else
    docker run --rm --network "${MIMIRTOOL_NETWORK:-host}" \
      -e MIMIR_ADDRESS -e MIMIR_AUTH_TOKEN \
      -v "$work:$work:ro" "$MIMIRTOOL_IMAGE" "$@"
  fi
}

# Build one directory per tenant with a mimirtool rule file per namespace.
while IFS=$'\t' read -r tenant namespace file; do
  [[ -z "$tenant" || "$tenant" == \#* ]] && continue
  mkdir -p "$work/$tenant"
  { printf 'namespace: %s\n' "$namespace"; cat "$root/rules/$file"; } > "$work/$tenant/$namespace.yaml"
done < "$plan"

failed=0
for dir in "$work"/*/; do
  tenant="$(basename "$dir")"
  echo "==> $tenant ($action)"
  if ! mimirtool rules "$action" --id="$tenant" --rule-dirs="$dir"; then
    echo "!! $action failed for tenant $tenant" >&2
    failed=$((failed + 1))
  fi
done

if (( failed > 0 )); then
  echo "rule $action failed for $failed tenant(s)" >&2
  exit 1
fi
