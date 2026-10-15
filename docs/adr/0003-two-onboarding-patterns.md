# 3. Two onboarding patterns: ship metrics, or forward alerts

- Status: accepted

## Context

Tenants arrive in two shapes: environments with no monitoring (VMs, small
clusters) and clusters that already run a mature Prometheus, usually
kube-prometheus-stack, with their own rules. Forcing the second kind to ship
every metric duplicates storage and cost and moves rule ownership away from
the team that knows the cluster.

## Decision

Support both:

- **Agent (A)**: Alloy ships all metrics; rules are versioned here and
  evaluated by Mimir's ruler, assigned through rule sets in `tenants.yaml`.
- **Own Prometheus (B)**: the tenant evaluates its rules and only
  remote-writes `ALERTS` and `ALERTS_FOR_STATE`, and pushes alerts to its
  Alertmanager on the platform.

The alerts console reads `ALERTS` only. Mimir's ruler also writes `ALERTS`,
so both patterns look the same there.

## Consequences

- Pattern B sends a few series per firing alert instead of thousands per
  node. The platform cannot draw dashboards for those tenants, only alerts.
- Alerts are only as good as the tenant's rules in pattern B. Watchdog, which
  kube-prometheus-stack ships, shows that the forwarding works, so these
  tenants get no central rule sets: a centrally evaluated Watchdog would keep
  firing even if forwarding broke.
- A tenant can move from B to A without changes on the platform side other
  than its rule sets.
