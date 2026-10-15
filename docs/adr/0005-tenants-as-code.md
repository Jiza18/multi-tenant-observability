# 5. tenants.yaml as the single source of truth

- Status: accepted

## Context

Adding a tenant used to mean editing four places by hand: the token map,
Mimir's overrides, two Grafana datasources (copied in two deployment
targets), and the list of rule sets. They drifted: datasources existed for
removed tenants and limits were set for tenants that no longer existed.

## Decision

Declare tenants once, in `tenants/tenants.yaml`, and generate everything else
with `tenantctl render`:

- Mimir runtime overrides (limits, retention);
- Grafana datasources, including the federated "All tenants" one;
- the rule sync plan (which rule files each tenant gets).

Generated files are committed. CI regenerates them and fails on any diff.
Tokens are the exception: they are secrets and live in the secret store.

## Consequences

- Every tenant change is a reviewable diff, including its effect.
- Removing a tenant removes its datasources (Grafana `prune: true`) and stops
  syncing its rules; orphaned per-tenant rule files fail the render.
- One more tool to maintain (`cmd/tenantctl`), covered by unit tests.
