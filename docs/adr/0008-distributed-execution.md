# ADR 0008 — Multi-instance execution: a leased job queue, and Postgres only when hosts multiply

- **Status:** Proposed — needs a decision on the scope question in "The fork" below, and on step 1 vs. the role-flag stopgap in "Alternatives"
- **Date:** 2026-07-24
- **Relates to:** ADR [0004](0004-sqlite-only.md) (SQLite only), ADR
  [0007](0007-remote-database-postgres.md) (Postgres deferred). This is the
  trigger 0007 named, firing.

## Context

The requirement, stated plainly: several Monarr instances of the same
version, sharing one state source while keeping their own local config,
**on one host now and across hosts later**. Each host serves media to one or
more clients. Jobs are of two kinds — cluster-wide work like scanning
folders or fetching artwork, and work tied to a particular host, such as a
remux or transcode for the media that host is serving.

The "now" and the "later" have different blockers, and conflating them is
the main way this decision goes wrong:

- **Multi-instance on one host needs no new database.** SQLite's WAL mode
  coordinates several *processes* on one kernel correctly — that is ordinary
  supported use, not the network-filesystem case ADR 0004 rules out. Two
  Monarrs on one host is safe at the storage layer today.
- **Multi-instance across hosts needs a networked store.** That is the
  ADR 0007 question, and only that.

So the storage decision can wait. What cannot wait, and is identical in both
topologies, is that **nothing in Monarr claims a unit of work**. That is the
blocker for the second instance on the *same* host, on day one.

ADR 0007 deferred Postgres partly on the argument that it "would not unlock
multiple replicas," because the automation loops are single-writer and
concurrent instances would double-grab. That argument was answered against a
hypothetical user. It is now answered against a real requirement, and it
needs restating rather than repeating: **Postgres is neither necessary nor
sufficient for the thing people actually want.** Not sufficient, because
sharing a database does not make two schedulers safe. Not necessary, because
on one host SQLite already shares fine. What makes concurrent instances safe
is that each unit of work is claimed exactly once. The missing piece is not
the database; it is the queue.

Five things break the moment a second instance starts today. Only the first
is topology-dependent:

- **The database — multi-host only.** SQLite cannot be shared across hosts
  (0007, 0004). Not negotiable, not fixable with mount options. On a single
  host this row is simply not a problem.
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

Leases are renewed by heartbeat and reclaimed when they expire, so a node
that dies mid-job strands nothing.

The claim degrades cleanly across both topologies, which is what makes this
worth building before the storage decision is made. On Postgres,
`FOR UPDATE SKIP LOCKED` lets N nodes claim concurrently without blocking
each other. SQLite has no `SKIP LOCKED`, but it does not need one: a
`BEGIN IMMEDIATE` around the same `UPDATE … WHERE id = (SELECT … LIMIT 1)
RETURNING` is atomic because SQLite serialises writers outright. Claims
queue instead of skipping — at homelab job rates, an irrelevant difference.
Same table, same semantics, same call sites; only the claim statement is
dialect-specific.

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

### 3. Postgres is required for multi-HOST mode, and only for that

SQLite remains the default, and it covers single-host multi-instance
completely — the topology in front of us. ADR 0004's zero-dependency install
is the project's best property and multi-host is a minority need; making
everyone pay for it would be the wrong trade. `MONARR_DB=postgres://…` opts
in when and if hosts multiply.

Deferring this is not just thrift. The queue is the part with design risk
and the part every topology needs; the store swap is mechanical and already
priced in ADR 0007. Doing the risky, universally-useful piece first and the
mechanical, conditionally-useful piece second is the ordering that lets the
project stop after step 1 and still be better off.

### 4. The supporting pieces

A `nodes` registry (id, hostname, version, capabilities, heartbeat) for
lease reclamation, capability routing, and a view of the fleet in the UI.
Sessions move to a table so any instance can serve any user. Migrations gate
on an advisory lock and follow expand-then-contract, because a rolling
restart means two versions briefly share one schema.

The event bus needs fanout as soon as there is a second *process*, not a
second host — two instances on one machine have exactly the same stale-cache
and missed-SSE problem. Postgres `LISTEN`/`NOTIFY` is the clean answer later;
for step 1 the cheap one is to let the bus fan out through the same database
the queue already polls, which avoids adding Redis or NATS to a homelab
install and keeps the abstraction the later swap needs.

Per-node config already works: config is ops-layer only (bind, port, data
dir, logging), while indexers, clients, and profiles live in the DB and are
therefore shared by construction. Resist expressing node differences as
divergent config — they belong in node capabilities, or the cluster becomes
N snowflakes.

## Consequences

- Multi-host Monarr is no longer zero-dependency. Accepted, because it is
  opt-in and the default install is unchanged. Multi-instance on one host
  stays zero-dependency, which is the case actually being asked for.
- "Run exactly one instance" in [deployment.md](../deployment.md) becomes
  "run exactly one *of each job*." The floating-single-instance patterns in
  `deploy/k8s/` remain correct and remain the recommendation for anyone not
  running multiple instances.
- The queue is worth having on one instance: durable retries, visible
  failures, and backpressure are all things the timer scheduler cannot do.
  This matters, because it means step 1 is not speculative work done for a
  cluster that may never arrive.
- Transcode and remux remain net-new work. The queue is their prerequisite,
  not a substitute for them — an encoder port, ffmpeg adapter, profiles,
  and progress reporting are their own project.

## Staging, so this can stop at any point and still be worth it

1. **Queue on SQLite, one host, N instances.** Convert scheduled tasks into
   enqueued jobs with dedupe keys and leases. No new dependency, no storage
   change; delivers the actual near-term requirement, and de-risks
   everything after it. Valuable on its own even if 2–4 never happen.
2. **Postgres behind the store interface.** Per ADR 0007's scope sketch,
   scoped to multi-host. Only when hosts multiply.
3. **Multi-host.** Node registry, heartbeats, capability routing,
   `LISTEN`/`NOTIFY` bus, shared sessions, migration locking.
4. **Transcode as the first capability-routed job kind.**

Steps 1 and 2 are independently useful and independently revertible. Step 3
is the point of no return for the single-instance assumption, and should not
start until 1 and 2 are proven.

## Alternatives considered

- **Designate one writer, replicas read-only.** A role flag disabling the
  scheduler and acquisition on replicas: roughly a day's work against
  several for the queue, and it preserves ADR 0004 outright. It genuinely
  solves "N instances serving the UI and API on one host," and if that is
  all that is ever wanted, it is the right answer and this ADR is
  over-engineering.

  Rejected as the *plan* for two reasons. It cannot route work by
  capability, so it can never run a transcode on the node with the GPU —
  the specific thing asked for. And it is thrown away rather than built on
  when hosts multiply, whereas step 1 of the queue is the same code the
  cluster later runs. It remains a reasonable stopgap if instances are
  needed before the queue lands; it is not a foundation.
- **Leader election over the existing scheduler.** Solves double-grabbing
  without a queue, but yields one busy node and N idle ones, and still
  cannot route work by capability. Strictly worse than the queue for the
  same operational cost.
- **rqlite / LiteFS.** Already rejected in ADR 0007; nothing here changes
  that, and neither offers `SKIP LOCKED`.
- **Redis or NATS for the queue.** A better queue, and a second stateful
  dependency to operate for a homelab. Postgres is already the multi-host
  answer; one new dependency is enough.
- **Do nothing.** Worth stating, because it is not obviously wrong. Monarr
  does not need N instances to serve N clients — the software that streams
  media does that, and one Monarr can feed it. If the motivation is
  throughput, the honest measurement is that Monarr is idle almost all the
  time. The case for this ADR rests on capability routing and on the queue's
  own merits (retries, visibility), not on load.
