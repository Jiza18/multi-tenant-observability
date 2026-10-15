# Operations

## Day-to-day tasks

| Task | How |
|---|---|
| Change a tenant's limits or retention | Edit `tenants/tenants.yaml`, `make generate`, merge. Mimir reloads its runtime config without restarting. |
| Add or change an alert | Edit `rules/`, add a test in `rules/tests/`, `make test-rules`, merge. CI or `make rules-sync` applies it. |
| Preview a rule change against production | `MIMIR_ADDRESS=... MIMIR_AUTH_TOKEN=... make rules-diff` |
| Issue a token | `make token NAME=... TENANT=... SCOPES=...` (see [onboarding](onboarding.md#2-issue-a-token)) |
| Revoke a token | Remove its entry from the token file in the secret store. Effective once the gateways reload it: External Secrets refresh (5 min) + kubelet sync (~1 min) + gateway poll (10 s). To revoke at once: `kubectl -n observability annotate externalsecret tenant-gateway-tokens force-sync=$(date +%s) --overwrite`, then `kubectl -n observability rollout restart deployment/tenant-gateway`. |
| Rotate a token | Add the new entry, let the tenant switch, then remove the old one. Both work in between. |
| Update upstream dashboards/rules | Bump the version in `scripts/vendor-upstream.sh`, `make vendor`, review the diff. Renovate bumps `MIMIR_VERSION` together with the Mimir images; CI fails on that PR until someone runs `make vendor` and commits the result. |

## Applying changes

The overlays are applied with server-side apply (some dashboards are larger
than the annotation client-side apply needs):

```bash
kubectl apply --server-side -k deploy/kubernetes/overlays/production
```

Rules are not part of the Kubernetes manifests; they live in Mimir's ruler
storage and are synced through the gateway:

```yaml
# e.g. a GitHub Actions job on main, after the manifests are applied
- run: make rules-sync
  env:
    MIMIR_ADDRESS: https://metrics.example.com
    MIMIR_AUTH_TOKEN: ${{ secrets.OBSERVABILITY_OPS_TOKEN }}
```

The sync is declarative per tenant: rule groups deleted from Git are deleted
from Mimir.

## Scaling

- **More load**: raise the StatefulSet's resources first. Adding replicas
  works without config changes (memberlist discovers them through DNS), but
  keep at least 3 for replication factor 3.
- **Beyond a few million active series** or very uneven read/write load,
  move to Mimir's microservices mode with the official `mimir-distributed`
  Helm chart. The gateway, Grafana, rules and tenant configuration stay the
  same: they only talk to Mimir's HTTP API. See [ADR 0004](adr/0004-monolithic-mimir.md).

## Backups

| Data | Where | Backup |
|---|---|---|
| Metrics, rules, Alertmanager state | Object storage | Bucket versioning and replication at the provider. |
| Tenants, limits, rules, dashboards | Git | Already versioned. |
| Grafana users and preferences | PostgreSQL | The database's own backups. Everything else in Grafana is provisioned. |
| Gateway tokens | Secret store | The store's own backups. Tokens can always be reissued. |

Mimir's local disks hold only the WAL and caches; losing a PVC loses at most
the unflushed data of one replica, which the other two also hold.

## Troubleshooting

Gateway status codes tell which check failed:

| Code | Meaning | Usual cause |
|---|---|---|
| 401 | Missing or unknown token | Typo, revoked token, token file not reloaded yet |
| 403 | Token lacks the scope, or asks for a tenant it is not bound to | Wrong token for the job; `X-Scope-OrgID` set to another tenant |
| 400 | Non-canonical path, missing or invalid tenant, multi-tenant write | Multi-tenant token without `X-Scope-OrgID` |
| 404 / 405 | Endpoint not exposed | Client calling a Mimir admin endpoint, or wrong path prefix |
| 429 / 5xx | From Mimir | Tenant over its ingestion rate (see *Tenants overview → Discarded samples*) |

Every denial is logged as JSON with the reason and the token **name**
(never the token) and counted in `tenant_gateway_denied_requests_total{reason}`.

Common alerts:

| Alert | First check |
|---|---|
| `TenantNoData` | Is the agent running? `curl` the gateway from the tenant's host with its token. |
| `TenantSamplesDiscarded` | The `reason` label: `rate_limited`, `per_user_series_limit`, `sample_out_of_order`... Raise limits in `tenants.yaml` or fix the sender. |
| `TenantNearSeriesLimit` | Find the high-cardinality metric with *Mimir / Tenants* or `topk(10, count by (__name__) ({__name__=~".+"}))` for that tenant. |
| `TenantGatewayConfigRejected` | Gateway logs: `token file rejected` with the validation error. |
