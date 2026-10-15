# 4. Mimir in monolithic mode with three replicas

- Status: accepted

## Context

Mimir can run as a single binary with every component (monolithic) or as
separate microservices. Mimir 3 also introduced an ingest-storage
architecture with Kafka between the write and read paths.

The expected load is tens of tenants and up to a few million active series.
The first version of this platform ran three replicas with local filesystem
storage, which does not work: each replica only saw the blocks it wrote.

## Decision

Run Mimir monolithic (`-target=all,alertmanager,overrides-exporter`), three
replicas in a StatefulSet, replication factor 3, with all long-term state in
object storage. Use the classic (ingester-based) architecture.

## Consequences

- One workload to operate and one configuration file, shared by the
  Kubernetes deployment and the Compose demo.
- Replicas are interchangeable; losing one has no impact, losing a disk
  loses no data that matters.
- Reads and writes cannot be scaled independently. When that is needed,
  the migration path is the official `mimir-distributed` chart; nothing
  outside Mimir (gateway, Grafana, rules, tenants) changes.
- No Kafka to run. Ingest storage becomes interesting at a scale this
  platform does not target.
