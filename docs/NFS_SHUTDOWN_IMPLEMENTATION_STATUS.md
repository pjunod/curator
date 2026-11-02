# NFS shutdown status — implementation, review and delivery

**Status:** #78 merged; #83 implementation, qualification and source review complete; delivery tracked in PR · **Updated:** 2026-11-02

The [implementation contract](NFS_SHUTDOWN_IMPLEMENTATION.md),
[deployment procedure](NFS_DEPLOYMENT_DRAIN.md) and
[qualification results](NFS_QUALIFICATION_RESULTS.md) describe the delivered
behavior and its measured limits. Production containers, mounts and settings
have not been changed by this work.

## 1. Delivery

| Item | State |
|---|---|
| Lifecycle PR | [#78](https://github.com/pjunod/curator/pull/78) merged at `ecee8fe36a5a6aaa91cacefbb1bd96172fb57e58`; all seven checks passed |
| Completion PR | [#83](https://github.com/pjunod/curator/pull/83), branch `codex/nfs-completion`; current main `f67e111` incorporated |
| Version/schema | Version0.38.9; cancellation migration0069, following main's migrations63–68 |
| Durable cancellation | Epoch-fenced controls/plans/grants, media and sidecar atomic results, retained custody/review, pruning and downgrade protection implemented |
| Deployment drain | Reversible admission barrier, participant ownership, loopback client and exact-container drain/TERM/actual-exit/replacement command implemented |
| Automatic NFS resumption | Disabled: namespace/lease/delegation/failover coordination remains unqualified |
| Prototype qualification | 120 trials; 40 eligible full completions; no optimized policy qualified; 41 low-memory OOM failures retained |
| Actual app qualification | Unlimited Docker4/4 scenarios passed; 512 MiB first scenario failed OOM and other3 were not run |
| Cancellation follow-up | Rebuilt `0993c17` app passed fresh unlimited 10 GiB cancellation/restart scenario after durable-before-signal correction |
| Selected fallback | In-process whole-file staging; drain before normal deployments, 30-second post-drain operator wait for inspected unlimited nuc3 Docker shape; actual exit required before replacement |
| Apple toolchain | Xcode27.2 required by user; these PR workflows use Linux, including Expo JS iOS/Android bundles, with no older Xcode selection |
| Merge/cleanup | Live [PR #83](https://github.com/pjunod/curator/pull/83) records final CI and merge; task-owned cleanup follows actual completion and preserves the original investigation checkout/stash and receipts |

## 2. Adversarial source review

Astra reviewed design and final source before the application gates. Findings
were repaired: deadline lifetimes and HTTP/2 idle streams; handler/background,
subscriber, wanted watcher and lease-heartbeat joins; epoch/selection atomicity;
publication ordering and stage descriptor identity; umask-independent modes;
resumable TERM; visible errors and focus restoration; retained-stage review
reachability; and automatic source custody, including bind aliases.

Main integration preserved the per-title and storage-decision fences, native
Runner torrent adapter and current lifecycle/receipt protections. Subsequent
reviews cleared integration fixes, the deployment client's actual-exit receipt,
the signed-status analyzer and the real-app measurement observer. No code
review alone was treated as NFS qualification.

Race CI then exposed a real cancellation ordering window: signaling before
intent persistence let the writer exit before finalization saw the intent.
The post-commit signal now follows durable admission, and registration's locked
intent check closes the concurrent-registration gap. A deterministic persistence
failure regression proves an uncommitted request cannot signal the writer.
Astra cleared the correction and its targeted real-app qualification.

## 3. Validation and CI

All four original local application gates passed after current-main integration:
lint0 issues, complete Go tests, web133 tests and browser212 tests. CI then
identified one test constant and a fractional-pixel phone assertion; both were
fixed. The browser assertion now checks ancestor intersection and viewport
geometry with at most one CSS pixel of clipping. The corrected E2E CI passed.

The initial combined race/coverage job exhausted package-wide 30-minute budgets
while still progressing through fresh SQLite migrations. The unchanged suite
now partitions top-level Test/Example/Fuzz seed names into eight deterministic
shards. JSON execution receipts prove exact-once execution; every shard must
succeed and supply matching source/toolchain/profile evidence. Atomic profiles
merge by source block, retaining zeros and checking statement counts before the
unchanged86.0% floor. Web, generation, vet and build remain in the required unit
check. Startup migration setup is outside shutdown latency assertions; the SSE
stop assertion remains two seconds.

Final local verification passed: full Go tests and the unchanged coverage floor
(86.1% on macOS), pinned lint with zero issues, entrypoint/maintenance race tests,
and Linux-only worker regressions on nuc3. Web133 and browser212 tests passed;
the final CI commit repeats required checks, including every Go race shard.
Astra cleared the added read/transactional failure and incompatible-record tests.
Failed checks are not represented as passing. Transactional fault-model
regressions use real SQLite with a single
injected boundary failure and independent durable snapshots; they establish
rollback for that model, not behavior after an ambiguous real COMMIT outcome.

## 4. Measured scope and evidence

Six initial post-drain actual exits measured0.113–0.194 seconds. Direct TERM during
a confirmed writable-descriptor copy measured24.340 seconds and left a matching
partial stage. The initial cancellation and its rebuilt follow-up both retained
the same cancelled epoch across restart. Normal replacement and two-writer
scenarios independently checked final library bytes and actual old process exit.
Astra reviewed the receipts and the bounded30-second post-drain operator wait.

The512 MiB app was a confirmed memory-cgroup OOM victim: exit137, OOMKilled=true,
PID zero. The remaining low-memory scenarios are untested. No optimization, NAS
outage deadline, or cross-host failover mechanism is qualified by these results.
Hard-mount outages can block actual I/O/close, shutdown and restart.

Raw artifacts and signed-source/build/hash receipts remain outside compiler
caches in the sibling `nfs-completion-evidence` directory and owned nuc3 campaign
roots. Original prototype classifications are preserved beside revised analysis.
Early missed-observer runs remain incomplete evidence; their exact containers
exited before subsequent campaigns began. Remote and local cleanup waits for
actual task completion and preserves all receipts and unrelated work.
