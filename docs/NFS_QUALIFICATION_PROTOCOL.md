# NFS shutdown qualification — frozen workload and evidence gates

**Status:** protocol frozen; [measured results](NFS_QUALIFICATION_RESULTS.md) recorded for prototype and application scopes · **Written:** 2026-11-01

Companion to [ADR 0025](adr/0025-import-cancellation-and-deployment-drain.md)
and [implementation status](NFS_SHUTDOWN_IMPLEMENTATION_STATUS.md). Data collection
is separate from application tests, policy selection and production rollout.

## 1. Scope and ownership

The initial campaign runs on nuc3 against QNAP's existing working and 20T NFSv4.1
hard mounts. It exclusively creates random, private task directories and a 10 GiB
random read-only source. Destination files use exclusive creation. The worker
records descriptor device/inode before bytes; cleanup requires the matching
current regular object, exact task-container identity/label and verified exit.
Unknown objects and pending processes remain in custody. Existing media,
production containers, mount options and network availability are unchanged.

A dedicated Docker image runs the reviewed static Linux worker. Memory shapes
are unlimited (the inspected Compose shape) and 512 MiB with swap disabled for
the container (the inspected Kubernetes limit). They are distinct measurements,
not a claim that Docker is equivalent to an entire Kubernetes deployment.
Raw data/owner receipts live outside disposable build caches.

## 2. Initial matched comparison

Five fixed configurations cover four policy categories:

| Policy | Interval |
|---|---|
| Whole-file buffered baseline | Final fsync |
| Periodic fdatasync | 256 MiB |
| Periodic fdatasync | 1 GiB |
| Concurrent fdatasync | 1 GiB trigger; requests coalesce, actual counts retained |
| Linux range-writeback pipeline | Start current 1 GiB window, wait previous, retain final fsync |

Run five rotated rounds per memory shape with the incident class of
1,400,000,000 bytes, then one full 10 GiB run per configuration/shape. All normal
successful policies retain final fsync and check sync/close errors. Concurrent
commit work joins before final sync/close. Errors remain primary even when they
also cancel copy work. A standalone prototype is not application performance.

Source-only `posix_fadvise(DONTNEED)` is advisory. Independently attributed READ
payload must be 90–110% of requested bytes for the declared client-cold class;
the NAS/server cache is uncontrolled. Retain all runs, including incomplete,
unsupported, contended and failed runs. Compare paired end-to-end ratios within
round/shape; do not silently pool unlike classes or discard unfavorable data.

## 3. Attribution and completion are different gates

Each trial saves raw mountstats before/after, sampled counters, host Dirty/
Writeback/thresholds, cgroup dirty/writeback/memory, task states and operational
production-import counts. Operational stage counts do not prove simultaneous
payload copies. The initial experiment starts exactly one scratch file writer.

One private tracefs instance records only NFS read/write metadata for the two
exports. It never records payloads or alters other tracing instances. Linux's
64-bit inode mode, descriptor device/inode, trace completeness and temporal
coverage are checked. Payload for other destination inodes is foreign host/export
traffic. The initial threshold is foreign initiated payload above 5% of own
payload. Raw mount/RPC evidence must be present and monotonic with unchanged
owning mount identity/options. The approximate per-RPC overhead calculation is
diagnostic and cannot independently establish cleanliness.

`completedPolicyWork` means a full expected input digest/byte count and successful
sync/close. `comparisonEligible` also requires trace/device/counter evidence,
declared source-read class, foreign threshold, log visibility and no logged NFS
outage or task WRITE error. Missing evidence is unknown, never passing. Host
attribution cannot prove absence of other NAS clients or background server load.
No logged outage establishes only that no outage was logged.

## 4. Stop and kill tails

The initial campaign attempts eight exploratory KILL trials across both sizes
and memory shapes, then collects at least twenty confirmed TERM observations
covering all twelve shape × size × operation-phase cells, capped at sixty total
stop attempts. Seed `20261101` selects copy offsets in 20–80% before measurement.
Retain missed/ambiguous attempts; the cap may leave coverage incomplete.

TERM records send timing, worker signal receipt and its declared operation phase,
plus canceled result and actual reaping. This establishes declared-phase
observation; kernel task-state evidence is separate. KILL cannot acknowledge its
phase, so its tail is exploratory and never substitutes for confirmed phase
coverage. A successful Docker signal call alone is insufficient evidence.

Close-writeback stress deliberately skips final fsync on a scratch file, is
labeled before any blocking open and never counts as a successful durable-policy
trial. It models a canceled/unpublished dirty file's close; it is not normal
post-fsync close performance. Attach EOF, failed stop or a zombie leader never
authorizes cleanup or another trial. Docker wait plus stopped-state verification
must establish actual exit first. No production outage/failover is induced.

## 5. Selection and application qualification still follow

The initial campaign ends as measurements, not qualification. A candidate needs
at least 90% paired baseline throughput and signal-to-actual-exit within sixty
seconds in every observed responsive-storage candidate trial, plus actual
two-writer comparisons/stop behavior. These are predeclared engineering targets,
not statistical tail bounds or outage deadlines. Report independent counts in
every class. Unsupported/missing evidence cannot pass a candidate.

If no candidate is qualified, retain the user's selected in-process measured-
grace fallback explicitly, without claiming it passed candidate budgets. The
actual application must then demonstrate durable cancellation, live status during
drain, admission races/resume, all mutation participants, and actual old-container
exit before replacement. Measure post-drain exit separately per deployment shape
and select an observed-margin stop wait; do not invent a ninety-second default.

Automatic cross-node stage/failover recovery remains outside the qualified
envelope until its namespace/OFD/lease/delegation/server-recovery tests pass in an
isolated environment. This campaign does not authorize faults on production
QNAP. The supported fallback preserves custody and blocks restart during outages.

Range writeback retains fsync because it is not a durability primitive; see the
[Linux manual](https://www.kernel.org/pub/linux/docs/man-pages/book/man-pages-6.13.pdf).
The deployment command explicitly disables the captured old container's restart
policy before TERM; [Docker documents restart-policy behavior](https://docs.docker.com/engine/containers/start-containers-automatically/).

## 6. Analyzer revision 2 preserves the original campaign

The frozen controller treated every nonzero `nfs_writeback_done.error` as an
error. This field is signed `task->tk_status`; nonnegative values are success,
including byte counts. The separately reviewed `analyze.py` accepts nonnegative
status only with complete own-object completion evidence and `res == count`.
A smaller final RPC with equal count/result is normal; short, zero-progress,
oversized, negative or malformed results remain outside the clean class.
See the [Linux v7.0 trace definition](https://github.com/torvalds/linux/blob/v7.0/fs/nfs/nfstrace.h#L1343)
and [completion handling](https://github.com/torvalds/linux/blob/v7.0/fs/nfs/write.c#L1385).

Revision 2 saves the exact captured manifest as an exclusive snapshot and hashes
all finalized per-trial inputs plus its own parser. It preserves original
classifications/reasons, removes only the erroneous signed-status exclusion,
re-applies foreign-traffic checks and requires actual sampled `memory.max` to
match the declared shape. Missing/inconsistent evidence remains ineligible.
`max` proves that cgroup's limit only, not absence of finite ancestor limits.
Live or partially classified trials are skipped. Original collection artifacts
are never rewritten by this analysis.
