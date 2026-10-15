# 2. A purpose-built gateway for authentication and tenancy

- Status: accepted

## Context

Mimir trusts the `X-Scope-OrgID` header. Clients outside the cluster need to
authenticate and must not be able to pick another tenant. Options:

1. Basic auth or `auth_request` in a reverse proxy (nginx, Traefik
   ForwardAuth, ingress annotations) plus a small token-to-tenant service.
2. Grafana Enterprise Metrics' gateway (commercial).
3. A small reverse proxy that owns the whole decision.

The first version of this platform used option 1 (Traefik ForwardAuth). It
had three problems: it depended on one ingress controller's annotations
(ingress-nginx, the other common choice, was retired in March 2026); it
forwarded every path, Mimir's admin endpoints included; and tokens lived in
plain text in a ConfigMap.

## Decision

Write `tenant-gateway`, a reverse proxy in Go (standard library plus the
Prometheus client), that:

- only forwards an allow-list of routes, each requiring a scope;
- stores SHA-256 digests of tokens, never tokens;
- binds tokens to one tenant, several, or any (`*`), and refuses (rather
  than silently rewrites) requests for a tenant the token is not bound to;
- reloads its token file without restarting and keeps the last valid one if
  a new file is invalid;
- exposes metrics, health and readiness on a separate internal port.

## Consequences

- Works behind any Gateway API implementation or load balancer, or none.
- One more component to run and keep available (2 replicas, PDB). It is
  stateless and small (~26 MB image, distroless, non-root).
- No identity provider integration: tokens are static secrets. Good for
  machine clients (agents, Prometheus, CI); humans use Grafana with SSO.
- The route table must follow Mimir's API when it changes. Tests pin the
  behaviour of every route.
