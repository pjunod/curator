# ADR 0008 — Multi-host execution: a leased job queue, mount-aware routing, and Postgres for clustered mode

- **Status:** Proposed — scope is settled; the open decision is the staging
  in "Staging" below (queue first, or the role-flag stopgap first)
- **Date:** 2026-07-24 (scope corrected same day — see "What the cluster is
  actually for")
- **Relates to:** ADR [0004](0004-sqlite-only.md) (SQLite only), ADR
  [0007](0007-remote-database-postgres.md) (Postgres deferred). This is the
  trigger 0007 named, firing.

## Context

The requirement, stated plainly: several Monarr instances of the same
version, sharing one state source while keeping their own local config,
**across hosts — a cluster, k8s or otherwise**. Work should be farmed out to
whichever hosts are idle or less busy: par repairs, unpacking, renames,
folder scans, artwork.

**Monarr does not play media and does not transcode.** Neither is in scope
here, and this ADR should not be cited as though it covered them.

Multi-host is the target. It is still worth separating the two blockers,
because they have different costs and only one of them is urgent:

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

## What the cluster is actually for — and how much of it is Monarr's today

The named workloads are par repairs, unpacking, renames, folder scans, and
artwork. Before designing for them, it is worth checking which of them
Monarr actually performs, because the answer changes what the queue is
farming:

| Work | Whose job today | Heavy? |
|---|---|---|
| Par repair | SABnzbd/NZBGet, before Monarr sees the payload | CPU-heavy |
| Unpacking | SABnzbd/NZBGet, likewise | Disk-heavy |
| Import + rename | **Monarr** — `internal/app/acquisition/import.go` | Usually instant; sometimes multi-GB |
| Library scan | **Monarr** — `internal/app/library/scan.go` | Disk-heavy on large trees |
| Artwork fetch | **Monarr** | Network-bound, embarrassingly parallel |

**Par repair and unpacking are not Monarr's work today, and there is no code
for either.** `internal/adapters/sabnzbd` submits and polls; the download
client repairs and extracts, and Monarr picks up the completed directory.
Farming those two across a Monarr cluster therefore means one of two things,
and they should not be conflated:

- **Monarr takes them over** — a par2 and archive stage of its own, running
  before import. That is net-new work that duplicates a responsibility the
  download client already owns, and it is only worth it if the goal is to
  drop the download client's post-processing entirely.
- **They stay where they are** — and the cluster's benefit for them is
  indirect at best. SABnzbd is its own single process on its own host; a
  Monarr cluster does not parallelise it.

This is not an objection to the ADR. It is a scoping correction: of the five
named workloads, **three are Monarr's and two are the download client's**.
The queue below is designed for the three, and it is the prerequisite for
the other two if they are ever brought in-house — but nothing here makes
par repair faster by itself, and no plan should assume otherwise.

The three that *are* Monarr's are genuinely worth farming. Scans walk entire
roots (`filepath.WalkDir`), artwork is hundreds of independent HTTP fetches,
and imports are occasionally very large — see the next section, which is the
one real constraint on placement.

### Shared storage is the precondition, and hardlinks are the trap

All three farmable workloads are filesystem work. Whether a second node can
do them at all depends on what that node has mounted — and one case degrades
silently rather than failing.

`place` in [`import.go`](../../internal/app/acquisition/import.go) hardlinks
`src` to `dest` and **falls back to a full `io.Copy` when the link fails**,
which is what happens across filesystems. That fallback is correct and
should stay. But it means an import farmed to a node where the download
directory and the library are not the *same* filesystem turns an instant
hardlink into a multi-gigabyte copy — no error, no warning, just a job that
takes a thousand times longer and burns network for the privilege.

So placement is not free, and the self-balancing property described in §2a
has one exception worth stating loudly:

```
 node has BOTH download dir and library on one filesystem? ─ yes ─▶ hardlink, instant
        │ no
 node has both mounted at all? ───────────────────────────── yes ─▶ COPY: slow, correct
        │ no
        ▼
   cannot import — must not be routed here
```

**Route imports by mount, not by idleness.** A node's mount set is a
routing input, and the busiest node with the right filesystem beats an idle
node without it every time. Scans and artwork have no such trap: scans need
the library readable, artwork needs only network.

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
- **`required_capability`** — a tag a node advertises, matched against the
  job's requirement. **For this workload the tags are mounts and
  toolchains, not hardware:** `mount:media-nas`, `mount:downloads`,
  `linked:downloads+media` for the hardlink case above, `tool:par2` if
  repair ever moves in-house. Any node carrying the tag may claim the row.
- **`affinity_node`** — pins work to exactly one node, for files only it can
  see. The degenerate case of a capability tag, kept separate because
  "this one host" is common enough to deserve a column rather than a
  synthesised per-node tag.

Everything else — artwork fetches, per-item metadata refresh — is left
unrouted and drains across all nodes in parallel, which is the one place
clustering plainly buys throughput.

Capability routing is doing less work here than it would in a cluster with
heterogeneous hardware; with no transcodes, its whole job is expressing
which node can see which files. That is still enough to need it, but it
argues for keeping the tag vocabulary small and mount-shaped rather than
building a general capability system.

### 2a. "Farm work to idle hosts" needs no dispatcher

The stated goal is that work lands on whichever host is least busy. A
pull-based queue gives this away for free: a node claims its next job only
when it has capacity, so a busy node simply claims less and an idle node
claims more. Load balances itself, with no scheduler deciding placement and
nothing to get wrong when a node is slow rather than dead.

This is a genuine argument for the queue over any push-based or role-based
design, and it is why `nodes.load` should exist only for the UI and for
sizing decisions — never as an input to placement. The moment placement
consults load, the self-balancing property is replaced by a heuristic that
has to be tuned.

The exception is the mount trap above: capability tags constrain *which*
nodes may claim a job, and within that set idleness decides. Constrain by
correctness, balance by pull — never the reverse.

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
N snowflakes. Mounts are the exception that proves it: they differ per node
by nature, which is exactly why they are advertised as capabilities rather
than configured as behaviour.

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
- Import gains a placement failure mode it does not have today — a job
  routed to a node without the right mounts either cannot run or copies
  gigabytes instead of linking. Capability tags exist to prevent this, and
  an import that falls back to copy should log loudly enough to be
  diagnosable, which today it does not.
- Par repair and unpacking are **not** made faster by this ADR. If moving
  them in-house is the actual goal, that is a separate decision with its own
  record; this one only ensures there would be somewhere to run them.
- Playback and transcoding stay out of scope entirely. Nothing here is
  designed for them.

## Staging, so this can stop at any point and still be worth it

1. **Queue on SQLite, one host, N instances.** Convert scheduled tasks into
   enqueued jobs with dedupe keys and leases. No new dependency, no storage
   change. This is where the design risk is, and it can be proven on one
   machine before anything irreversible. Valuable on its own even if 2–4
   never happen, because retries and failure visibility are improvements to
   a single instance.
2. **Postgres behind the store interface.** Per ADR 0007's scope sketch.
   Mechanical, but this is where the permanent dual-dialect tax starts.
3. **Multi-host.** Node registry, heartbeats, mount-tag routing,
   `LISTEN`/`NOTIFY` bus, shared sessions, migration locking.
4. **Parallel scan and artwork as the first genuinely farmed job kinds**,
   then imports once mount tags are trustworthy — imports last, because
   they are the ones that degrade silently when routed wrong.

Steps 1 and 2 are independently useful and independently revertible. Step 3
is the point of no return for the single-instance assumption, and should not
start until 1 and 2 are proven.

## Alternatives considered

- **Designate one writer, replicas read-only.** A role flag disabling the
  scheduler and acquisition on replicas: roughly a day's work against
  several for the queue, and it preserves ADR 0004 outright. It genuinely
  solves "N instances serving the UI and API on one host."

  Rejected as the *plan* because it farms nothing — all work stays on the
  writer, which is the opposite of the stated requirement. It also cannot
  express which node can see which files, so it cannot place an import
  correctly even by accident. And it is thrown away rather than built on
  when hosts multiply, whereas step 1 of the queue is the same code the
  cluster later runs. It remains a reasonable stopgap if instances are
  needed before the queue lands; it is not a foundation.

  Worth being honest that this option is more competitive than it was when
  this ADR assumed GPU transcodes: without heterogeneous hardware, "one
  writer, N read-only UI replicas" covers more of the ground than it used
  to. What it does not cover is farming, and farming is the requirement.
- **Leader election over the existing scheduler.** Solves double-grabbing
  without a queue, but yields one busy node and N idle ones — again, no
  farming. Strictly worse than the queue for the same operational cost.
- **rqlite / LiteFS.** Already rejected in ADR 0007; nothing here changes
  that, and neither offers `SKIP LOCKED`.
- **Redis or NATS for the queue.** A better queue, and a second stateful
  dependency to operate for a homelab. Postgres is already the multi-host
  answer; one new dependency is enough.
- **Do nothing.** Worth stating, because it is not obviously wrong. If the
  motivation is throughput, the honest measurement is that Monarr is idle
  almost all the time — scans and artwork are bursty, not sustained, and a
  single instance absorbs them on any reasonable hardware. The case for this
  ADR rests on the queue's own merits (retries, visibility, backpressure)
  and on correct placement, not on load.
