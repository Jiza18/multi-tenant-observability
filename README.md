# Multi-tenant observability platform

[![CI](https://github.com/Jiza18/multi-tenant-observability/actions/workflows/ci.yml/badge.svg)](https://github.com/Jiza18/multi-tenant-observability/actions/workflows/ci.yml)

A self-hosted metrics and alerting platform for many tenants, built on
[Grafana Mimir](https://grafana.com/oss/mimir/). Each tenant (a client, a team,
an environment) ships its metrics and alerts with its own token. The platform
keeps tenants isolated, evaluates their alert rules from Git and gives operators
one console with every tenant's alerts.

This is an open-source rewrite of a platform I designed and ran in production
for a consultancy, monitoring several client environments from one place.

![Alerts console: firing alerts across all tenants](docs/images/alerts-console.png)

## What it does

- **Isolation by token.** [`tenant-gateway`](internal/gateway), a small Go reverse
  proxy, maps each token to its tenant(s) and scopes, only forwards an
  allow-list of Mimir endpoints and stores SHA-256 digests instead of tokens.
- **One console for every tenant's alerts.** Grafana queries all tenants at once
  through Mimir tenant federation, whether rules run centrally or in the
  tenant's own Prometheus.
- **Two ways to onboard.** Ship all metrics with Grafana Alloy and let the
  platform evaluate the rules, or keep your Prometheus and forward only your
  alerts. ([ADR 0003](docs/adr/0003-two-onboarding-patterns.md))
- **Tenants as code.** `tenants/tenants.yaml` is the only place a tenant exists;
  Mimir limits, Grafana datasources and rule assignments are generated from it,
  and CI rejects drift.
- **Tested alerting rules.** Rules are plain Prometheus files with `promtool`
  unit tests, synced declaratively to each tenant.
- **Monitors itself.** Mimir's official mixin, gateway alerts, per-tenant usage
  against limits and a Watchdog per tenant for a dead man's switch.
- **Production-shaped Kubernetes.** Kustomize base and overlays, Pod Security
  `restricted`, NetworkPolicies, PodDisruptionBudgets, Gateway API and External
  Secrets.

## Architecture

```mermaid
flowchart LR
    A[Alloy agents] -- metrics --> GW
    P[Tenant Prometheus] -- ALERTS + alerts --> GW
    CI[CI: rule sync] -- rules --> GW
    GW[tenant-gateway<br/>token → tenant, scopes] -- X-Scope-OrgID --> M
    subgraph M[Mimir · 3 replicas]
        direction TB
        D[ingest · query · ruler · alertmanager]
    end
    M --> S3[(Object storage)]
    G[Grafana] -- per-tenant and federated datasources --> M
    M -- notifications --> N[Slack, PagerDuty, ...]
```

Details, request flow and failure modes: [docs/architecture.md](docs/architecture.md).

## Quick start

Requirements: Docker with Compose v2 and `make`.

```bash
make demo
```

This starts three Mimir replicas, S3-compatible storage (SeaweedFS), the
gateway, Grafana and three simulated tenants:

| Tenant | Pattern | What you will see |
|---|---|---|
| `acme-prod` | Alloy agent, rules evaluated centrally | Host metrics in *Hosts / Node Exporter Full* |
| `acme-staging` | Declared but sends nothing | `TenantNoData` firing after the first rule evaluation |
| `globex-prod` | Own Prometheus, forwards alerts only | Two alerts in the console |

Open Grafana at <http://localhost:3000> (anonymous viewer; `admin`/`admin` to
edit) and run the end-to-end checks:

```bash
make smoke-test
```

Talk to the gateway like a tenant would (demo tokens are in
[`deploy/compose/.env`](deploy/compose/.env)):

```bash
# Read your own tenant's data
curl -H 'Authorization: Bearer demo-acme-prod-reader' \
  'http://localhost:8080/prometheus/api/v1/query?query=up'

# Another tenant's data: 403
curl -H 'Authorization: Bearer demo-acme-prod-reader' -H 'X-Scope-OrgID: globex-prod' \
  'http://localhost:8080/prometheus/api/v1/query?query=up'

# Mimir's admin endpoints are not exposed: 404
curl -X POST -H 'Authorization: Bearer demo-ops' http://localhost:8080/ingester/shutdown
```

`make down` stops everything and deletes the data. To run the Kubernetes
manifests on a local [kind](https://kind.sigs.k8s.io/) cluster instead:
`make kind-up`.

![Tenants overview: ingestion, usage against limits and gateway traffic](docs/images/tenants-overview.png)

## Repository layout

| Path | Contents |
|---|---|
| [`cmd/`](cmd), [`internal/`](internal) | [`tenant-gateway`](cmd/tenant-gateway) and [`tenantctl`](cmd/tenantctl) (tokens, config rendering), in Go |
| [`tenants/tenants.yaml`](tenants/tenants.yaml) | Tenants, their limits and rule sets |
| [`rules/`](rules) | Alerting and recording rules, one file per rule set, with tests |
| [`deploy/kubernetes/`](deploy/kubernetes) | Kustomize base, `local` (kind) and `production` overlays, dashboards |
| [`deploy/compose/`](deploy/compose) | Local demo; reuses the configuration in `deploy/kubernetes/base` |
| [`agents/alloy/`](agents/alloy) | Tenant agent: one config for Docker, systemd (Ansible) and Kubernetes (Helm) |
| [`docs/`](docs) | Architecture, onboarding, operations and design decisions |

## Documentation

- [Architecture](docs/architecture.md): components, security model, tenancy and failure modes
- [Onboarding a tenant](docs/onboarding.md): from `tenants.yaml` to the first alert
- [Operations](docs/operations.md): tokens, limits, rules, upgrades, backups, troubleshooting
- Decisions:
  [Mimir](docs/adr/0001-mimir-as-metrics-backend.md) ·
  [the gateway](docs/adr/0002-tenant-gateway.md) ·
  [onboarding patterns](docs/adr/0003-two-onboarding-patterns.md) ·
  [monolithic Mimir](docs/adr/0004-monolithic-mimir.md) ·
  [tenants as code](docs/adr/0005-tenants-as-code.md)

## Development

```bash
make help        # all targets
make test        # Go tests (race detector) and rule unit tests
make lint        # golangci-lint and shellcheck
make validate    # kubeconform on every overlay
make generate    # re-render config after editing tenants.yaml
```

CI runs all of the above plus the Compose demo with the smoke test on every
pull request. Tagged releases publish a multi-arch gateway image with SBOM
and provenance to `ghcr.io/jiza18/tenant-gateway`. Renovate keeps images,
Actions and the vendored Mimir mixin up to date.

## License

[MIT](LICENSE)
