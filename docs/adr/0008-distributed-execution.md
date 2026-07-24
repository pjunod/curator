# ADR 0008 — Multi-host execution: a leased job queue, Postgres for clustered mode, and playback left to the media server

- **Status:** Proposed — the scope question below is answered, but the recommendation to integrate rather than build playback needs Paul's agreement
- **Date:** 2026-07-24
- **Relates to:** ADR [0004](0004-sqlite-only.md) (SQLite only), ADR
  [0007](0007-remote-database-postgres.md) (Postgres deferred). This is the
  trigger 0007 named, firing.

## Context

The requirement, stated plainly: several Monarr instances of the same
version, sharing one state source while keeping their own local config,
**across hosts — a cluster, k8s or otherwise**. Each instance plays media
for one or more clients. Work should be farmed out to whichever hosts are
idle or less busy: generic jobs like scanning folders and fetching artwork,
and heavier ones like a remux or transcode on behalf of a host that is busy
serving a client.

Multi-host is therefore the target. It is still worth separating the two
blockers, because they have different costs and only one of them is urgent:

- **Multi-instance on one host needs no new database.** SQLite's WAL mode
  coordinates several *processes* on one kernel correctly — that is ordinary
  supported use, not the network-filesystem case ADR 0004 rules out. Two
  Monarrs on one host is safe at the storage layer today.
- **Multi-instance across hosts needs a networked store.** That is the
  ADR 0007 question, and only that.

Postgres is therefore committed, not deferred — the target topology requires
it. But it can be sequenced *second*, because the harder and more universal
problem is that **nothing in Monarr claims a unit of work**. That blocks the
second instance even on one host, it is where the design risk lives, and it
is fully buildable and testable on SQLite before any storage change. The
store swap is mechanical by comparison and already priced in ADR 0007.

ADR 0007 deferred Postgres partly on the argument that it "would not unlock
multiple replicas," because the automation loops are single-writer and
concurrent instances would double-grab. That was correct, and it remains
correct — it is just not a reason to skip Postgres, it is a reason not to
mistake Postgres for the whole answer. **Necessary here, but never
sufficient:** sharing a database does not make two schedulers safe. What
makes concurrent instances safe is that each unit of work is claimed exactly
once, and nothing in Monarr claims anything today. Adopt Postgres for reach,
and the queue for correctness; adopting only the first produces a cluster
that races.

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

## The scope question, answered — and what it costs

Asked directly, the answer was: "each instance will be able to play media
for one or more clients," and work should be "farmed out to other idle or
less busy hosts," including "splitting up a remux or transcode job for one
of the hosts in the cluster that is serving a client."

That is three distinct products, and they should be priced separately
because only one of them is Monarr:

- **A media server.** Playback for clients: streaming endpoints, client
  capability negotiation, direct-play vs. transcode decisions, seeking,
  subtitle burn-in, session tracking. Monarr has none of this and it is not
  adjacent to anything Monarr has. Jellyfin and Plex are this product.
- **A distributed transcode farm.** A pool of worker nodes pulling
  encode/remux work over a shared library. **This already exists as Tdarr**,
  which is worth naming because it is precisely the described architecture,
  and because the interesting question becomes build-vs-integrate rather
  than how-to-build.
- **A media manager.** Acquire, organize, rename, reconcile. This is Monarr,
  and it is finished.

**The recommendation is to build the third and integrate the other two.**
Monarr gains a job queue that can farm work across hosts — which is real,
useful, and in its lane — and that queue can drive remux and pre-transcode
whose output lands in the pool. Playback stays with the software that
already does playback. A Monarr that grows a streaming stack is a
multi-month project whose end state is a worse Jellyfin, and the manager is
the part nobody else is currently rewriting.

The rest of this ADR designs the queue and the farming, which are needed
under either answer. **Segmented transcode is sketched but not committed**
(see "Splitting a transcode" below) — it is the piece with the worst
effort-to-payoff ratio, and the piece Tdarr already ships.

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

### 2a. "Farm work to idle hosts" needs no dispatcher

The stated goal is that work lands on whichever host is least busy. A
pull-based queue gives this away for free: a node claims its next job only
when it has capacity, so a busy node simply claims less and an idle node
claims more. Load balances itself, with no scheduler deciding placement and
nothing to get wrong when a node is slow rather than dead.

This is a genuine argument for the queue over any push-based or
role-based design, and it is why `nodes.load` should exist only for the UI
and for sizing decisions — never as an input to placement. The moment
placement consults load, the self-balancing property is replaced by a
heuristic that has to be tuned.

### 3. Postgres for clustered mode, SQLite for everyone else

The target topology is multiple hosts, so Postgres is accepted rather than
deferred — ADR 0007's revisit criteria are met. `MONARR_DB=postgres://…`
selects it.

It stays *opt-in*, though, and SQLite remains the default. ADR 0004's
zero-dependency install is the project's best property, single-node is still
the overwhelming majority of installs, and a clustered deployment is by
definition run by someone who can operate a database. The cost is the
dual-dialect tax ADR 0007 priced: every store query and migration written
and tested against both engines, permanently. That is the real price of this
ADR, and it is paid in every future change, not once.

### 3a. Splitting a transcode — sketched, not committed

Segmented encoding is the one requirement the queue does not answer by
itself. The shape is: split at keyframe boundaries, encode segments as
independent jobs, concatenate. The queue handles the fan-out; the hard parts
are elsewhere and are worth stating before anyone estimates this as "a job
kind":

- Segments must be cut on keyframes, or the concatenation stutters. That
  means a probe pass, and it means variable segment lengths.
- Rate control does not survive segmentation. Per-segment CRF drifts in
  quality across boundaries; matching a target bitrate requires a
  two-pass or a shared statistics file that segments cannot share.
- Audio, subtitles, and chapters are not segmented and must be passed
  through and remuxed at the end.
- Every node needs the same encoder build. Different ffmpeg or driver
  versions across nodes produce segments that differ subtly, and the seams
  show.
- The payoff is bounded: it only helps for one large file at a time. With a
  queue of many files, encoding whole files on separate nodes is simpler,
  faster in aggregate, and has none of the above problems.

**Recommendation: whole-file jobs first**, routed by capability. Revisit
segmentation only if single-file latency is measurably the problem, which
for a library-wide re-encode it is not.

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

- Clustered Monarr is no longer zero-dependency, and every future store
  query and migration is written and tested twice. That permanent tax is the
  real cost of this ADR — larger than the queue itself. Accepted because the
  target topology requires it, and contained because the default install is
  unchanged.
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
- Playback stays out of scope. If that is ever revisited, this ADR does not
  cover it and should not be cited as though it did.

## Staging, so this can stop at any point and still be worth it

1. **Queue on SQLite, one host, N instances.** Convert scheduled tasks into
   enqueued jobs with dedupe keys and leases. No new dependency, no storage
   change. This is where the design risk is, and it can be proven on one
   machine before anything irreversible. Valuable on its own even if 2–4
   never happen, because retries and failure visibility are improvements to
   a single instance.
2. **Postgres behind the store interface.** Per ADR 0007's scope sketch.
   Mechanical, but this is where the permanent dual-dialect tax starts.
3. **Multi-host.** Node registry, heartbeats, capability routing,
   `LISTEN`/`NOTIFY` bus, shared sessions, migration locking.
4. **Whole-file remux/transcode as the first capability-routed job kind** —
   not segmented (see 3a).

Steps 1 and 2 are independently useful and independently revertible. Step 3
is the point of no return for the single-instance assumption, and should not
start until 1 and 2 are proven. Playback is not in this list, and should not
be added to it — see the scope question above.

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
