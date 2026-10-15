# 1. Grafana Mimir as the metrics backend

- Status: accepted

## Context

The platform stores metrics and evaluates alerts for several clients
(tenants) that must never see each other's data. Operators need one view
across all of them. Candidates: one Prometheus per tenant with federation,
Thanos, VictoriaMetrics (cluster) and Grafana Mimir.

## Decision

Use Grafana Mimir.

Multi-tenancy is built in end to end, not only in storage:

- per-tenant limits and retention, changeable at runtime;
- a per-tenant ruler, so alert rules are evaluated centrally for tenants
  that only ship raw metrics;
- a per-tenant Alertmanager, so each tenant routes its own notifications;
- tenant federation, so one query can span tenants, which is what the
  cross-tenant alerts console needs.

VictoriaMetrics is lighter and would have been a fine storage, but its
open-source cluster version does not offer a per-tenant ruler and
Alertmanager with the same isolation, so they would have had to be built
around it. Per-tenant Prometheus with federation multiplies the moving parts
by the number of tenants. Thanos accepts tenant-aware writes but has no
per-tenant ruler or Alertmanager either.

## Consequences

- Mimir has no authentication: anything that reaches it can claim any
  tenant. A gateway in front is mandatory (ADR 0002) and network access to
  Mimir must be restricted.
- Mimir needs object storage, even for small installations.
- Mimir's operational dashboards and alerts come from the official mixin,
  vendored at the same version as the image.
