# ADR 0007 — Remote database (Postgres) support: evaluated, deferred

- **Status:** Deferred (revisit criteria below)
- **Date:** 2026-07-19
- **Relates to:** ADR 0004 (SQLite only), which stands

## Context

Real-world validation raised the cluster question: can the backend float
freely between Kubernetes nodes — one instance, restarted anywhere on node
failure — without a node-local dependency for the database file? SQLite on
NFS/Gluster is off the table (WAL + shared-file locking corrupts), which
makes the DB the one thing pinning the pod.

Two facts frame the evaluation:

1. **The instance-mobility problem is already solvable without changing the
   database.** Replicated block storage (Longhorn/Ceph RBD) makes the volume
   follow the pod; Litestream makes the durable copy live in object storage
   with node-local scratch and restore-on-reschedule — zero node-local
   dependency. Worked manifests for both live in `deploy/k8s/`.
2. **Postgres would not unlock multiple replicas.** The automation loops
   (RSS sync, backlog search, queue refresh, import) are a single-writer
   design; concurrent instances would double-grab and race the download
   state machine. Upstream Sonarr/Radarr support Postgres and remain
   strictly single-instance. Multi-active would additionally require leader
   election, which no *arr does and no homelab needs.

What Postgres WOULD buy: the DB becomes a network service with its own HA
story (CloudNativePG, Patroni), the storage constraint disappears entirely,
backups ride the Postgres ecosystem, and very large libraries get a more
forgiving query planner.

## Decision

**Defer.** SQLite-only (ADR 0004) stands while the project is in validation.
The floating-single-instance patterns in `deploy/k8s/` cover the stated need
(node failure → reschedule anywhere → no data loss) without the permanent
dual-dialect tax.

Revisit — and expect to accept — when **any** of these hold:

- Monarr is public and cluster operators are a real user population asking
  for it (the same demonstrated-need bar ADR 0004 set);
- a library outgrows SQLite in practice (sustained multi-writer pressure or
  query times that WAL + indexes can't fix);
- the project wants managed-DB deployments (e.g. a shared Postgres already
  operated by the user) as a first-class story.

## Scope sketch, so the lift is priced before it starts

Localized by design (ADR 0004 chose sqlc partly for this):

- **Queries:** sqlc dual-engine output (`sqlite` + `postgresql` blocks in
  `internal/infra/sqlite/sqlc.yaml`, package split into a store interface
  with two generated backends). Most queries are portable; the exceptions
  are enumerable (`ON CONFLICT` shapes are fine in both; `RETURNING` fine).
- **Migrations:** goose per-dialect directories. SQLite's table-rebuild
  migrations (0007's CHECK widening) become plain `ALTER` in Postgres; the
  STRICT keyword drops; the existing schema-enum drift test keeps both
  honest.
- **Driver seams to abstract:** the read/write connection split and
  `SetMaxOpenConns(1)` write discipline (unnecessary in PG), constraint
  errors detected by string sniffing (`constraint failed` → pgcode 23505),
  `VACUUM INTO` backups (→ delegate to `pg_dump` or document "your DB, your
  backups"), the WAL checkpoint scheduler task (SQLite-only, becomes a
  no-op), `PRAGMA`s in `Open`.
- **CI:** the test matrix doubles (every store/service test against both
  engines, Postgres service container in the Tests workflow) — permanently.
- **Estimate:** several focused days to land, plus the forever tax upstream
  pays: every future migration and query written and tested twice.

## Alternatives considered

- **rqlite / libsql-server (Turso):** SQLite semantics over the network,
  but a smaller ecosystem than Postgres, a different driver, and Raft
  write latency — more moving parts than Postgres for less community
  support. Rejected.
- **LiteFS:** replicated SQLite with lease-based failover; elegant on
  Fly.io, awkward on generic kube (FUSE + Consul lease), and Litestream
  covers the actual requirement with less machinery. Rejected for now.

## Consequences

- The kube story ships today as documentation + manifests, not code.
- ADR 0004's "reconsidered only on demonstrated need" now has concrete
  triggers and a priced scope, so the future decision is a go/no-go, not a
  research project.
