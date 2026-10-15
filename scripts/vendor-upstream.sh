#!/usr/bin/env bash
# Vendor upstream dashboards and rules at pinned versions.
#
# Upstream content is copied verbatim, never edited by hand: to update it,
# bump a version below and run `make vendor`. Renovate proposes MIMIR_VERSION
# bumps; CI re-runs this script and fails until the vendored files match.
set -euo pipefail

MIMIR_VERSION="3.2.2"              # github.com/grafana/mimir
NODE_EXPORTER_FULL_REVISION="45"   # grafana.com/grafana/dashboards/1860

# Mixin dashboards that make sense for a monolithic Mimir deployment.
MIMIR_DASHBOARDS=(
  mimir-overview
  mimir-writes
  mimir-reads
  mimir-queries
  mimir-tenants
  mimir-top-tenants
  mimir-ruler
  mimir-alertmanager
  mimir-object-store
  mimir-compactor
)

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
dashboards="$root/deploy/kubernetes/base/grafana/dashboards"
mixin="https://raw.githubusercontent.com/grafana/mimir/mimir-${MIMIR_VERSION}/operations/mimir-mixin-compiled"

fetch() {
  curl --fail --silent --show-error --location --retry 3 "$1"
}

vendor_rules() {
  local src=$1 dest=$2
  {
    printf '# Vendored from grafana/mimir@mimir-%s (operations/mimir-mixin-compiled/%s)\n' "$MIMIR_VERSION" "$src"
    printf '# by scripts/vendor-upstream.sh. Do not edit by hand.\n'
    fetch "$mixin/$src"
  } > "$dest"
  echo "vendored $dest"
}

vendor_rules rules.yaml "$root/rules/mimir-mixin-rules.yaml"
vendor_rules alerts.yaml "$root/rules/mimir-mixin-alerts.yaml"

rm -f "$dashboards"/mimir/*.json
for name in "${MIMIR_DASHBOARDS[@]}"; do
  fetch "$mixin/dashboards/$name.json" > "$dashboards/mimir/$name.json"
done
echo "vendored ${#MIMIR_DASHBOARDS[@]} Mimir dashboards"

fetch "https://grafana.com/api/dashboards/1860/revisions/${NODE_EXPORTER_FULL_REVISION}/download" \
  > "$dashboards/hosts/node-exporter-full.json"
echo "vendored Node Exporter Full (revision $NODE_EXPORTER_FULL_REVISION)"
