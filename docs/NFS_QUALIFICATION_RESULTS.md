# NFS shutdown qualification results

**Status:** unlimited-memory application scenarios passed; 512 MiB shape failed; no optimized policy qualified · **Updated:** 2026-11-02

The [protocol](NFS_QUALIFICATION_PROTOCOL.md) fixes the comparison and stop gates.
The [delivery status](NFS_SHUTDOWN_IMPLEMENTATION_STATUS.md) tracks PR #83.
All runs use owned scratch on nuc3. Production mounts, containers and settings
have not been changed. The user-selected fallback keeps storage in Monarr,
drains before normal deployment, and waits for actual process exit. A NAS outage
can block cancellation, shutdown and replacement.

## 1. Prototype evidence

The frozen campaign `completion-f2cec50c6a0b` completed 120 trials. Revision-2
analysis retains the original classifications and raw evidence, correcting only
the signed RPC-status interpretation documented in the protocol. Forty full
completions satisfy comparison eligibility. Forty-one trials were OOM killed,
all in the 512 MiB shape. Failed and missing observations remain failures or
unknowns; they do not become passes when surviving trials are fast.

For the unlimited shape, eligible small-file end-to-end medians include final
sync and close. These are prototype measurements, not Monarr throughput:

| Policy | Median MB/s | Eligible repetitions |
|---|---:|---:|
| Whole-file final fsync | 34.68 | 5 |
| Periodic 256 MiB fdatasync | 52.27 | 4 |
| Periodic 1 GiB fdatasync | 49.01 | 4 |
| Concurrent 1 GiB fdatasync | 30.70 | 4 |
| Range pipeline 1 GiB plus final fsync | 46.63 | 5 |

Three unlimited repetitions fail the foreign-write threshold. The low-memory
baseline completes only two of five small runs and no full 10 GiB run. Periodic
256 MiB survives its six full low-memory runs, but the only two available paired
baseline ratios are 0.878 and 0.868, below the declared 0.90 target. That survival
result does not qualify the policy.

Thirty TERM trials have confirmed signal receipt and actual exit. They cover
10 of 12 whole-file shape/size/phase cells; both low-memory close-stress cells
are missing. The longest controller signal-to-Docker-finish interval is 86.80 s.
Ordinary final-sync observations include 61.36 s for the small payload and
71.76 s for 10 GiB. Close-stress deliberately omits final fsync; its 64.15 s
maximum is not a normal post-fsync close measurement. Low-memory stop attempts
include 24 OOMs out of 30 attempts; their four confirmed survivors cannot qualify
that deployment shape.

Optimized-policy TERM and two-writer prototype comparisons are absent. No policy
passes the complete predeclared qualification contract. The fallback is the
user's explicit operational choice, not a claim that candidate budgets passed.

## 2. Real Monarr campaign

The application campaign freezes source/build receipts, Linux app/helper hashes,
immutable base-image ID, controller and deployment script. Each exclusive Compose
project uses an isolated local database, read-only source, private NAS target,
no network, no published ports and no restart policy. No production service is
stopped or replaced. The initial app is the reviewed PR implementation at `75460a3`; a targeted
cancellation/restart follow-up uses `0993c17`, after correcting durable-before-
signal ordering. Both runs freeze independent binary/source receipts.

The scenarios cover drain/abort/re-drain with live status and rejected mutation,
actual old-container exit before replacement, cancellation followed by restart,
two simultaneous imports, and direct TERM during observed copying. Final library
files require independent size/digest readback. A stopped leader alone is not
exit proof: Docker wait and stopped PID/exit-state agreement are mandatory.
Nonzero exits and OOMs fail the shape; a live blocked writer stops the campaign.

Early observer revisions missed the buffered copy window. Their raw evidence
is retained as incomplete qualification. Those scratch containers exited with
code zero and PID zero before another run started. The reviewed final observer
uses current journal epoch/control plus a writable open descriptor, stable
container/process/placement/inode/mount identity, and two increasing offsets.
It excludes read-only verification descriptors. The offsets prove userspace
copy progress, not NAS durability. It avoids live NFS getattr calls, which can
block behind writeback. Signal timing and post-exit partial-stage evidence are
recorded separately; an earlier copy observation alone does not prove TERM hit
copying.

The final observer run established these results:

| Unlimited-memory scenario | Result |
|---|---|
| Drain, reject new mutation, abort, re-drain, replace after actual exit | Passed; first exit verified in 0.188 s |
| Cancellation during 10 GiB copy, actual unwind, restart suppression | Passed; same epoch remained cancelled after restart |
| Two simultaneous advancing writer descriptors, drain, replace and byte readback | Passed; first exit verified in 0.194 s |
| TERM during advancing copy | Passed; 0.017 s observation-to-signal gap, 24.340 s to actual exit, retained partial stage 349,175,808/1,400,000,000 bytes |

Six post-drain process exits were observed, from 0.113 to 0.194 seconds. Every
successful application exit had code zero, PID zero and no OOM flag. Both normal
replacement scenarios independently verified final library bytes. Maximum native
API service time across the four unlimited scenarios was 0.0267 seconds; this
excludes Docker-exec overhead and is not an end-user latency percentile.

For the inspected unlimited-memory nuc3 Docker shape, use **30 seconds as the
post-drain operator wait**. It is more than 150 times the largest observed
post-drain interval, and intentionally leaves substantial scheduling/cleanup
margin. This is an operational selection from six observations, not a statistical
tail guarantee. Drain itself waits for actual work without a fixed copy deadline.
On timeout the deployment command preserves the exact container and receipt;
it never escalates to KILL or starts a replacement while the old process lives.
Direct TERM measurements are separate and do not justify applying 30 seconds
without first draining. The 600-second campaign wait was a measurement ceiling.

The first 512 MiB real-app scenario failed. The kernel identified Monarr as the
memory-cgroup OOM victim; Docker subsequently reported exit137, OOMKilled=true,
PID zero. This is a failed deployment shape, even though buffered copy progress
was observed and earlier API calls worked. The campaign stopped, preserved work,
and independently verified that failed process's actual exit. The remaining
three low-memory scenarios were not run and do not pass by inference. Do not
claim this whole-file policy is qualified for a 512 MiB container; the selected
fallback applies to the inspected unlimited Docker shape. No memory limit was
changed on a production container.

## 3. Unsupported coordination envelope

Earlier basic OFD exclusion passed across nuc3, nynuc and m6, but a nonblocking
attempt took 35.142 seconds. Namespace, lease/delegation, NAS restart/failover
and server-recovery faults have not been qualified. Automatic cross-process or
cross-host stage resumption remains disabled; uncertain stages retain custody.
No production outage was induced to fill these gaps.

## 4. Evidence ownership

The durable evidence directory beside the owned completion clone contains
`prototype-final-analysis.json`, `prototype-final-manifest.json`, build receipts,
application/controller logs, gate logs and observer stop receipts. nuc3 retains
raw prototype trials under `/home/pjunod/codex-nfs-qualification/completion-f2cec50c6a0b`
and application runs under `app-completion-75460a3`. Original and revised
classifications are distinct. Source edits and receipts are outside compiler
cache directories. Live or uncertain objects are never removed by a timeout.

## 5. Cancellation ordering follow-up

Race CI exposed an early-signal ordering window: a writer could unwind before
its cancellation intent committed. Signaling now follows the durable commit;
failed journal persistence leaves the actual writer running. A deterministic
regression injects that persistence failure. The rebuilt `0993c17` app repeated
the unlimited-memory 10 GiB cancellation/restart scenario in a fresh scratch
project: actual writable-descriptor progress was confirmed, cancellation was
durable, the writer actually exited, and the restarted process retained the same
cancelled epoch without re-importing. The captured manifest reports completion.
This follow-up updates cancellation qualification; it does not turn the failed
512 MiB shape or untested coordination faults into passes.
