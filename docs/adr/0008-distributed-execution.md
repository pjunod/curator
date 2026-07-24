# ADR 0008 — Multi-node execution: a leased job queue, and Postgres for clustered mode only

- **Status:** Proposed — needs a decision on the scope question in "The fork" below
- **Date:** 2026-07-24
- **Relates to:** ADR [0004](0004-sqlite-only.md) (SQLite only), ADR
  [0007](0007-remote-database-postgres.md) (Postgres deferred). This is the
  trigger 0007 named, firing.

## Context

The requirement, stated plainly: several Monarr instances across several
hosts, each serving media to one or more clients, sharing one state source
while keeping their own local config. Jobs are of two kinds — cluster-wide
work like scanning folders or fetching artwork, and work tied to a
particular host, such as a remux or transcode for the media another host is
serving.

ADR 0007 deferred Postgres partly on the argument that it "would not unlock
multiple replicas," because the automation loops are single-writer and
concurrent instances would double-grab. That argument was answered against a
hypothetical user. It is now answered against a real requirement, and it
needs restating rather than repeating: **Postgres is necessary but not
sufficient.** Sharing a database does not make two schedulers safe. What
makes them safe is that each unit of work is claimed exactly once. The
missing piece is not the database; it is the queue.

Five things break the moment a second instance starts today:

- **The database.** SQLite cannot be shared across hosts (0007, 0004). Not
  negotiable, not fixable with mount options.
- **The scheduler.** `internal/infra/scheduler` is in-process timers. Its
  `scheduled_tasks` table records what ran; it never claims anything. Two
  nodes run every task: two RSS syncs, two backlog searches, duplicate
  grabs, doubled TMDB and indexer traffic.
- **The event bus.** In-process. SSE updates and cache invalidation stop at
  the node boundary.
- **The wanted index.** A lazy in-process cache invalidated by local bus
  events — stale on every node but the one that changed something.
- **Sessions.** In memory, so a user is logged in to one node only.

## The fork

"Each host serves media to clients" and "remux/transcode jobs" describe a
media *server*. Monarr is a media *manager*: it acquires, organizes,
renames, and hands off. It has no streaming path and no encoder, and
building one is a different product competing with Jellyfin and Plex.

Two readings, and the answer changes the whole plan:

- **(A) Monarr grows a serving and transcoding role.** A rewrite of scope,
  not an extension. Not recommended, and not what this ADR designs.
- **(B) Monarr stays the manager and gains distributed job execution.**
  Each host runs Monarr alongside whatever actually serves media; Monarr
  nodes cooperate on *file* work — scans, artwork, and yes, remux and
  pre-transcode, whose output lands in the pool for the serving software to
  pick up. The transcode is file preparation, not a live stream.

**This ADR assumes (B).** Under (B) every requirement above is satisfiable
without leaving the project's lane.

## Decision

### 1. A leased job queue replaces timers as the execution primitive

A `jobs` table carrying kind, payload, state, priority, `run_after`,
`attempts`, `last_error`, and three routing columns described below. A node
claims work atomically:

```sql
UPDATE jobs SET state='leased', lease_owner=$node, lease_expires_at=now()+'60s'
WHERE id = (
  SELECT id FROM jobs
   WHERE state='queued' AND run_after <= now()
     AND (required_capability IS NULL OR required_capability = ANY($caps))
     AND (affinity_node IS NULL OR affinity_node = $node)
   ORDER BY priority, run_after
   FOR UPDATE SKIP LOCKED LIMIT 1)
RETURNING *;
```

`FOR UPDATE SKIP LOCKED` is the whole trick, and it is the concrete reason
this needs Postgres rather than a network filesystem. Leases are renewed by
heartbeat and reclaimed when they expire, so a node that dies mid-job
strands nothing.

The scheduler keeps its job: it stops *running* periodic work and starts
*enqueuing* it. The Tasks page survives unchanged.

### 2. Three routing columns, matching the three kinds of work

- **`dedupe_key`** — a unique partial index over non-terminal states. Gives
  "exactly one cluster-wide" for `rss.sync`, `backlog.search`,
  `importlists.sync`, `queue.refresh`, `library.scan`, `backup`. Note what
  this buys: singleton *jobs* without a singleton *node*. No leader
  election, no primary, no failover story to operate — any node may run it,
  exactly one does.
- **`required_capability`** — set to `vaapi`, `nvenc`, or similar. Only
  nodes advertising it can claim the row. This is how a transcode reaches a
  host that can actually do it.
- **`affinity_node`** — pins work to one node, for files only it can see or
  clients only it serves.

Everything else — artwork fetches, per-item metadata refresh — is left
unrouted and drains across all nodes in parallel, which is the one place
clustering actually buys throughput.

### 3. Postgres is required for clustered mode, and only for clustered mode

SQLite remains the default and the single-node story, untouched. ADR 0004's
zero-dependency install is the project's best property and clustering is a
minority need; making everyone pay for it would be the wrong trade. But
`MONARR_DB=postgres://…` opts into the clustered feature set.

This is deliberately *not* the full dual-dialect commitment ADR 0007
priced. Under that pricing every query and migration is written and tested
twice, forever. Scoping Postgres to clustered mode does not avoid the
dual-dialect tax on the store layer, but it does mean cluster-only tables
(`nodes`, `jobs`) need no SQLite counterpart, and the single-node path
never regresses.

### 4. The supporting pieces

A `nodes` registry (id, hostname, version, capabilities, heartbeat) for
lease reclamation, capability routing, and a cluster view in the UI.
Postgres `LISTEN`/`NOTIFY` to fan the event bus across nodes, avoiding a
Redis or NATS dependency. Sessions move to a table so any node can serve any
user. Migrations gate on an advisory lock and follow expand-then-contract,
because a rolling restart means two versions briefly share one schema.

Per-node config already works: config is ops-layer only (bind, port, data
dir, logging), while indexers, clients, and profiles live in the DB and are
therefore shared by construction. Resist expressing node differences as
divergent config — they belong in node capabilities, or the cluster becomes
N snowflakes.

## Consequences

- Clustered Monarr is no longer zero-dependency. Accepted, because it is
  opt-in and the single-node default is unchanged.
- "Run exactly one instance" in [deployment.md](../deployment.md) becomes
  "run exactly one *of each job*." The floating-single-instance patterns in
  `deploy/k8s/` remain correct and remain the recommendation for anyone not
  clustering.
- The queue is worth having even on one node: durable retries, visible
  failures, and backpressure are all things the timer scheduler cannot do.
- Transcode and remux remain net-new work. The queue is their prerequisite,
  not a substitute for them — an encoder port, ffmpeg adapter, profiles,
  and progress reporting are their own project.

## Staging, so this can stop at any point and still be worth it

1. **Queue on SQLite, single node.** Convert scheduled tasks into enqueued
   jobs with dedupe keys and leases. No behavior change, no new dependency;
   makes the execution model explicit and testable, and de-risks everything
   after it. Valuable on its own even if steps 2–4 never happen.
2. **Postgres backend behind the store interface.** Per ADR 0007's scope
   sketch, scoped to clustered mode.
3. **Multi-node.** Node registry, heartbeats, capability routing,
   `LISTEN`/`NOTIFY` bus, shared sessions, migration locking.
4. **Transcode as the first capability-routed job kind.**

Steps 1 and 2 are independently useful and independently revertible. Step 3
is the point of no return for the single-instance assumption, and should not
start until 1 and 2 are proven.

## Alternatives considered

- **Designate one writer, replicas read-only.** Much cheaper and preserves
  ADR 0004 outright. Rejected against the stated requirement: it cannot run
  a transcode on the node that has the GPU, which is the specific thing
  wanted.
- **Leader election over the existing scheduler.** Solves double-grabbing
  without a queue, but yields one busy node and N idle ones, and still
  cannot route work by capability. Strictly worse than the queue for the
  same operational cost.
- **rqlite / LiteFS.** Already rejected in ADR 0007; nothing here changes
  that, and neither offers `SKIP LOCKED`.
- **Redis or NATS for the queue.** A better queue, and a second stateful
  dependency to operate for a homelab. Postgres is already being adopted;
  one new dependency is enough.
