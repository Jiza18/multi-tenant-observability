# Onboarding a tenant

Onboarding takes three changes in Git and one secret. Examples use a tenant
`initech-prod`; the gateway is at `https://metrics.example.com`.

## 1. Declare the tenant

Add it to `tenants/tenants.yaml`:

```yaml
  - id: initech-prod            # Mimir tenant ID: [a-zA-Z0-9!-_.*'()], max 150 chars
    name: Initech · Production  # shown in Grafana
    retention: 30d              # optional, overrides defaults
    max_global_series: 300000   # optional
    rule_sets: [watchdog, agent-health, hosts]   # pattern A only, see below
```

Then render the derived configuration and commit everything:

```bash
make generate
git add tenants/ deploy/kubernetes/base/generated/ rules/sync-plan.tsv
```

On merge, applying the overlay updates Mimir's limits (no restart) and
Grafana gets two new datasources; the rule sync loads the tenant's rules.

Rules that only apply to this tenant go to `rules/tenants/initech-prod.yaml`
(loaded into the `custom` namespace).

## 2. Issue a token

```bash
make token NAME=initech-agents TENANT=initech-prod SCOPES=metrics:write
```

The token is printed once on stderr; stdout has the entry for the gateway
token file, with the SHA-256 only:

```yaml
  - name: initech-agents
    sha256: 5e0c1d...
    tenants: ["initech-prod"]
    scopes: ["metrics:write"]
```

- Add the entry to the token file, check it with
  `go run ./cmd/tenantctl check-tokens tokens.yaml` (a gateway that restarts
  with an invalid file cannot start) and store it in the secret store
  (`observability/tenant-gateway-tokens`). The gateways pick it up without a
  restart once External Secrets and the kubelet sync it (a few minutes).
- Hand the token to the tenant through a secret channel, never by chat or
  email. Store it on their side as a secret.

Scopes per use case:

| Client | Scopes |
|---|---|
| Alloy agent (pattern A) | `metrics:write` |
| Tenant Prometheus (pattern B) | `metrics:write, alerts:write` |
| Tenant's own Grafana or scripts | `metrics:read, alerts:read, rules:read` |
| CI that syncs rules (all tenants) | `metrics:read, rules:read, rules:write, alertmanager:config` with tenant `*` |

## 3a. Pattern A: install the agent

All variants use the same configuration, [`agents/alloy/config.alloy`](../agents/alloy/config.alloy),
which reads `MIMIR_URL` and `MIMIR_TOKEN` from the environment.

**Linux hosts (systemd) with Ansible**

```bash
cd agents/alloy/ansible
ansible-galaxy collection install -r requirements.yaml
cp inventory.example.yaml inventory.yaml   # git-ignored; token in Ansible Vault
ansible-playbook -i inventory.yaml playbook.yaml --ask-vault-pass
```

**Docker hosts**

```bash
cd agents/alloy/docker
MIMIR_URL=https://metrics.example.com MIMIR_TOKEN=... HOSTNAME=$(hostname) docker compose up -d
```

**Kubernetes clusters** with Grafana's
[k8s-monitoring](https://github.com/grafana/k8s-monitoring-helm) chart:

```bash
kubectl create namespace monitoring
kubectl -n monitoring create secret generic observability-token --from-literal=token=<token>
helm upgrade --install k8s-monitoring grafana/k8s-monitoring --version 4.5.3 \
  -n monitoring -f agents/alloy/kubernetes/k8s-monitoring-values.yaml \
  --set cluster.name=initech-prod
```

## 3b. Pattern B: forward alerts from an existing Prometheus

For clusters running kube-prometheus-stack, add to the `Prometheus` resource
(or the chart's `prometheus.prometheusSpec`):

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: observability-token
  namespace: monitoring
stringData:
  token: <token>   # scopes: metrics:write, alerts:write
---
# Merged into the Prometheus custom resource
spec:
  # Only the alert series leave the cluster.
  remoteWrite:
    - url: https://metrics.example.com/api/v1/push
      authorization:
        credentials:
          name: observability-token
          key: token
      writeRelabelConfigs:
        - sourceLabels: [__name__]
          regex: ALERTS|ALERTS_FOR_STATE
          action: keep
  # The local Alertmanager keeps working; this adds the platform's.
  additionalAlertManagerConfigs:
    name: observability-alertmanager
    key: config.yaml
---
apiVersion: v1
kind: Secret
metadata:
  name: observability-alertmanager
  namespace: monitoring
stringData:
  config.yaml: |
    - scheme: https
      path_prefix: /alertmanager
      api_version: v2
      static_configs:
        - targets: [metrics.example.com]
      authorization:
        credentials_file: /etc/prometheus/secrets/observability-token/token
```

Mount the token secret in Prometheus with `spec.secrets: [observability-token]`
so `credentials_file` exists.

The demo's Globex tenant uses this pattern with a plain Prometheus:
[`deploy/compose/demo/globex/prometheus.yml`](../deploy/compose/demo/globex/prometheus.yml).

## 4. Route notifications (optional)

Until a tenant uploads its own Alertmanager configuration, its alerts are
kept (and visible in Grafana) but not sent anywhere. To notify the tenant's
on-call channel:

```bash
mimirtool alertmanager load initech-alertmanager.yaml \
  --address=https://metrics.example.com --auth-token="$OPS_TOKEN" --id=initech-prod
```

Keep receiver secrets (webhook URLs, API keys) out of Git: render the file
from your secret store in CI.

## 5. Verify

```bash
# Data arrives (token with metrics:read for the tenant)
curl -s -H "Authorization: Bearer $READ_TOKEN" \
  'https://metrics.example.com/prometheus/api/v1/query?query=up' | jq '.data.result | length'
```

In Grafana:

- **Alerts console**: the tenant appears under *Watchdog per tenant* once its
  rules run: after the rule sync (pattern A) or once its Prometheus forwards
  alerts (pattern B).
- **Tenants overview**: samples per second and series usage against its limit.
- **Hosts / Node Exporter Full** with the tenant's datasource (pattern A).

## Offboarding

1. Remove the tenant's entries from the token file. Requests fail with 401
   as soon as the gateways reload it.
2. Remove it from `tenants.yaml`, `make generate`, commit. Its datasources
   disappear from Grafana.
3. Delete what it has in Mimir's ruler and Alertmanager. The rule sync only
   manages tenants in `tenants.yaml`, so it will not do this; without it the
   tenant's rules keep running and its Alertmanager keeps notifying:

   ```bash
   export MIMIR_ADDRESS=https://metrics.example.com MIMIR_AUTH_TOKEN=$OPS_TOKEN
   for ns in $(mimirtool rules list --id=initech-prod --format=json | jq -r '.[].namespace' | sort -u); do
     mimirtool rules delete-namespace --id=initech-prod "$ns"
   done
   mimirtool alertmanager delete --id=initech-prod
   ```

4. Its data expires with its retention, or delete it at once with Mimir's
   [tenant deletion API](https://grafana.com/docs/mimir/latest/references/http-api/).
