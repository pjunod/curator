# Deployment: storage, floating nodes, and HA

Monarr is deliberately one process with one SQLite file (ADR
[0004](adr/0004-sqlite-only.md)): a single writer in WAL mode plus
in-process state (scheduler, sessions, wanted cache). That buys the
zero-dependency install, and it sets the two rules everything below
follows:

1. **Run exactly one instance.** Two Monarrs against one database means
   lock fights and double-grabbed releases. "HA" for Monarr is *fast
   failover of a single instance*, not active-active.
2. **The live database must sit on storage with local-disk semantics.**
   Which is the actual question here.

## Before anything else: who owns `/data`

The most common first-run failure has nothing to do with storage class. A
bind mount whose host directory does not exist is created by the Docker
daemon as `root:root`. The container runs as `${PUID}:${PGID}`, cannot
create `monarr.db` inside a root-owned directory, and Monarr exits before it
ever serves a request. Compose gives you no way to declare a bind mount's
ownership, so the host has to be correct first:

```sh
cd deploy && ./bootstrap.sh   # every path in your .env, owned by PUID:PGID
```

The same rule applies to any path you add later — a second root folder, a
pool on another disk. Create it owned by the runtime user, or Monarr and its
download client will both fail on it in ways that look like bugs.

If you hit it anyway the startup error names the directory, its owner, the
process's uid/gid, and the exact `install -d` to run. On Kubernetes prefer
`securityContext.fsGroup`, which makes the kubelet fix volume ownership for
you. A *named* Docker volume is also safe: on first use Docker seeds an
empty named volume from the image's `/data`, ownership included, and the
image ships that directory owned by `1000:1000`. Only bind mounts need the
host-side step, because a bind mount always takes the host directory's
ownership and no image can change that.

## Why the DB can't live on NFS — or Gluster

SQLite's WAL mode coordinates through a memory-mapped `-shm` file and
POSIX advisory locks. Network filesystems break both: NFS lock daemons are
famously unreliable (SQLite's own docs call network filesystems the
leading cause of corruption, and WAL is explicitly unsupported across
them), and GlusterFS — being FUSE with its own caching and lock
translation — has the same class of problem. It might appear to work for
days and then hand you a corrupt database after a failover or a cache
hiccup.

So: **no NFS, no GlusterFS, no SMB for `/data`.** This isn't a Monarr
limitation to configure around; it's SQLite physics.

Gluster is perfectly fine for the **media/downloads pool** — big
sequential files, no locking subtleties. Split your storage classes:

| Mount | Storage |
|---|---|
| `/pool` (media + downloads) | NFS / GlusterFS / CephFS — whatever your cluster shares |
| `/data` (SQLite, backups) | something that looks like a local disk to exactly one node |

## Patterns for a floating `/data`, best first

> Worked Kubernetes manifests for patterns 1 and 2 live in
> [`deploy/k8s/`](../deploy/k8s/) — edit the storage class / S3 endpoint
> and apply.

### 1. Replicated block storage (recommended on a cluster)

Give `/data` a **block** volume that follows the container — the
filesystem on top is plain ext4, so SQLite's semantics are correct, and
the replication happens below the filesystem:

- **Kubernetes**: Longhorn or Ceph **RBD** (not CephFS) as a
  ReadWriteOnce PVC; `replicas: 1` and `strategy: Recreate` so the old pod
  is dead before the new one mounts.
- **Plain hosts / Swarm**: DRBD between the nodes it can float to, or any
  SAN/iSCSI LUN the nodes can each attach (one at a time).
- **Cloud**: an EBS/PD-style volume that reattaches on reschedule.

Failover cost: seconds to reattach, zero data loss.

### 2. Local disk + streaming replication (Litestream / LiteFS)

Keep `/data` on plain local disk wherever the container lands, and let
[Litestream](https://litestream.io) replicate the SQLite file continuously
to S3/MinIO (which *can* be backed by your Gluster, since Litestream
writes objects, not live DB pages). On startup at a new node, restore the
latest generation, then run. RPO is seconds; wiring is a sidecar/wrapper
around the container. LiteFS goes further with primary election if you
want managed failover. This is the pattern when you have shared *object*
storage but no shared *block* storage.

### 3. Pin it, and lean on the built-in backups

Simplest and honest: constrain Monarr to one node with local disk
(placement constraint / nodeSelector). Monarr already snapshots the DB
daily with `VACUUM INTO` into `<data>/backups/` (last 7 kept) — point a
cron/rsync at that directory to copy snapshots onto the shared pool.
Node dies → start Monarr elsewhere, drop the newest snapshot in as
`monarr.db`, rescan. RPO is up to a day (or whatever you rsync), which for
a media manager is usually fine: the library rebuilds from disk truth by
design (ADR 0005).

A snapshot from `backups/` is always safe to copy while Monarr runs —
that's the point of `VACUUM INTO`. Copying the *live* `monarr.db` while
the process runs is not.

## Failover behaviors to expect

- Scheduler state persists in the DB — tasks resume with their cadence.
- UI sessions are in-memory — users re-login after a failover (the API
  key is unaffected).
- In-flight downloads are fine: the queue reconciles from the download
  client on the next `queue.refresh`.
- Never run two replicas "for safety" — surge/rolling updates included.
  `Recreate`, not `RollingUpdate`.

## The short answer to "would Gluster work?"

For the media pool, yes. For the live `/data` database, no — and NFS is
no either. Put `/data` on replicated block storage (Longhorn/Ceph
RBD/DRBD) if you have it, or local disk with Litestream replication if you
don't, or pin the container and ship the daily snapshots to the shared
pool.
