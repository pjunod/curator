# NFS shutdown status — implementation, review and delivery

**Status:** #78 merged; cancellation, drain and NFS qualification in progress · **Updated:** 2026-11-01

Companion to the [implementation contract](NFS_SHUTDOWN_IMPLEMENTATION.md).
This page records actual implementation state, separately from the earlier
incident/proposal reviews. A code review is not runtime qualification of NFS.

| Item | State |
|---|---|
| Source baseline | Completion clone at `ecee8fe`, branch `codex/nfs-completion` |
| SSE cancellation and write deadlines | Implemented; responsive/backpressure/HTTP2 tests passed in the full Go gate |
| Listener failure cancels/drains workers | Implemented; occupied-port regression written; passed in the full Go gate |
| HTTP/background-search ownership and truthful goodbye | Implemented; handler/background join tests written; passed in the full Go gate |
| Job lease-heartbeat ownership | Implemented; Astra follow-up review addressed; blocked-store-call regression passed |
| Concrete cancellation/deployment-drain design | ADR 0025 in progress; Astra identified epoch, sidecar, pruning and uncertain-publication obligations |
| Durable cancellation migration/UI/API | Concrete transaction design being completed before migration |
| Maintenance drain-before-stop and actual-exit deployment command | Coordinator and worker/request admission wiring in progress; deployment command and tests pending |
| Automatic NFS stage/failover recovery | Not implemented; coordination qualification still required |
| Writeback prototype/selection and production grace | Not selected; lifecycle fixes do not make NFS interruptible |
| PR | [#78 — shutdown ownership](https://github.com/pjunod/curator/pull/78) merged; completion PR not opened |
| Adversarial code/design review | Astra source review completed; deadline lifetime, joins, bounded tests, publication ordering, reversible maintenance design and stale test callers addressed; local gate passed |
| Final validation | Go 1.25.7 / Node 22.23.4: make lint PASS (0 issues), make test PASS, make test-web PASS (124 tests), make test-e2e PASS (192 tests), repeated after the heartbeat follow-up review; all seven #78 CI checks passed. Completion source has not run its final gate |
| Merge | #78 merged at `ecee8fe36a5a6aaa91cacefbb1bd96172fb57e58`; no branch-protection bypass |
| Cleanup | #78 branch deleted; prior temporary clone already absent; investigation artifacts and original checkout/stash preserved. New completion clone registered and active |
| Apple toolchain | User requires Xcode 27.2; current Curator PR workflows are Linux jobs (including Expo JS bundles), with no older Xcode selection |

No production container, mount or setting has been changed by implementation.

## Adversarial review dispositions

Astra reviewed the independent implementation clone before any tests. The first
pass required idle HTTP/2 deadline handling, broader joins or narrower completion
claims, bounded regression startup, durable cancellation/publication ordering,
and reversible maintenance distinct from final shutdown. Repairs add HTTP/2 idle
coverage; join application loops, subscriber children and wanted watchers;
bound test setup; and specify per-placement authorization plus independent
maintenance admission. The spot-check found stale private test callers after
the supervisor signature change; these now own and join their worker groups.
All review findings are addressed in source/design; test and CI results remain
separate evidence. No automatic storage recovery is approved by this review.

A later audit found that the job worker canceled but did not join its lease
heartbeat. The worker now joins the heartbeat, including a store call still
unwinding after cancellation. Astra's focused follow-up approved the production
join and required repeat-safe notifications in the test fake; those are fixed.
The normal-completion lease lifetime is preserved. This does not promise lease
renewal during shutdown once the parent context is canceled.

## Local gate receipt

The final reviewed code passed all four repository gate commands. The first
lint attempt failed with cached positions pointing at a deleted unrelated
checkout; rerunning only lint with a fresh task analysis cache returned zero
issues. The shared Go compiler cache was retained. After the reviewed heartbeat
follow-up, all four commands passed again, including the blocked store-call
regression. No runtime source changed after that final gate. CI/branch-protection
results remain separate from local results.

## Completion work

The shared reversible coordinator now registers native/compat mutations,
background searches, scheduled work/manual triggers, durable job claims and
retention, import/cleanup execution, and acquisition retry/recovery/subscriber
iterations. This is work in progress, not a passing implementation claim.
Astra's pre-migration review requires epoch-specific prepared placement identity,
verified prior-receipt reuse, sidecar coverage, bulk-pruning exclusions and a
visible/safe resolution for uncertain authorization. These are being addressed
before the cancellation migration. No completion tests have run before final
source review. NFS qualification and numeric deployment grace remain pending.
