# Fable review handoff — Runner and Curator download lifecycle

**Status:** archived review brief; user waived Fable review on 2026-10-23
**Written:** 2026-10-22 · **Updated:** 2026-10-23

Read [the approved operating proposal](DOWNLOAD_LIFECYCLE_PROPOSAL.md) first,
then [implementation status](DOWNLOAD_LIFECYCLE_STATUS.md) and
[Cinema's HTTP integration contract](BAD_DOWNLOAD_API.md). Review both services
and their shared ownership boundary independently. On 2026-10-23 the user waived
Fable review because quota was unavailable and authorized merging after
adversarial agent review and passing unit tests. The evidence below records the
earlier handoff checkpoint; consult the PRs for subsequent validation and merge
status. Deployment and production media changes remain separate.

## 1. Review targets

| Repository | Baseline | Pull request | Reviewed head |
|---|---|---|---|
| Curator (`pjunod/curator`) | `7e8e2f20693e34233e09920e6015b28369024ce6` | [#70](https://github.com/pjunod/curator/pull/70) | Astra source approval is bound to `7e402065253453a42e4f7aee44f535cd6bc7f31e`, including duplicate completion, recovery cancellation and the subsequent custody regressions. Latest full local coverage is 85.1%, below 86%. Full local tests, focused race tests and lint pass. Linux tests, browser, mobile, lint, Docker and compatibility pass at `744ae51`, but its coverage gate fails at 85.0% |
| Runner (`pjunod/runner`) | `42995f0b45a7eb977c383c4b630fa75e58bbedfb` | [#254](https://github.com/pjunod/runner/pull/254) | Astra source approval, normal Linux PR checks and supply-chain audit pass at `83cd4cdfef3ec62a67237fc918114f622ef638a2`; separate manual coverage gate remains below 90% |

The primary agent reviews construction. Lower-cost agents implement changes;
Astra has performed adversarial source review and follow-up reviews of fixes.
Challenge the implementation independently, including code marked corrected in
the status page. Prior review records what was examined; it is not a substitute
for checking final behavior.

## 2. Preserve the approved behavior

1. **Ownership changes at durable publication.** Runner owns processing and
   failed sources; Curator owns the published completed payload. Runner may
   clean its source once usable payload and a restart-safe notification
   obligation are durable, even if Curator is offline. Runner must never delete
   the transferred publication.
2. **Recovery requires evidence.** Retain complete or repairable source bytes
   with a concrete next step. Unrepairable failures are purged unless the user
   opted into Keep. I/O failure, missing storage and unfinished diagnosis are
   not evidence that bytes are disposable. Removing Keep must re-evaluate
   disposition without discarding genuinely recoverable files.
3. **A manual grab creates another copy.** Existing library quality or coverage
   cannot veto it. One request is idempotent across retries; another explicit
   request is independent. Permanent manual failure stops for a user decision.
   One-time blacklist override does not change automatic search eligibility.
4. **Replacement remains scoped.** Remove explicitly bad disposable bytes
   before replacement. Retain usable quality targets until replacement imports
   and its current bytes are verified. Keep good episodes, shared files and
   unrelated copies. Episode replacement requires an explicit source-blacklist
   choice; declining excludes that source only from this replacement. Pack
   fallback imports selected episodes and cleans unused output.
5. **Reports are review-only.** Cinema or Curator may flag a library generation;
   a report cannot cause automatic deletion, blacklist or reacquisition. Cinema
   implementation is outside these PRs; its API contract is documented.
6. **Retries survive restart.** Recoverable work uses 30, 60, 120 and then
   300-second backoff indefinitely, respects a longer valid server delay and
   shows a warning after 30 minutes. Handoff and each owner's cleanup are
   independently observable and retryable. Cancellation must defeat replay.
7. **Old backlog remains separate.** A read-only reconciliation report exists.
   No deletion, import or retry batch has been approved. Migrations and normal
   reconciliation must not grant old files new automatic deletion authority.

## 3. Follow the shared contract across both implementations

Runner's `docs/CURATOR_DELIVERY.md` specifies the native protocol. Compare it
with Curator's `internal/ports/delivery.go` and NZBD adapter, and verify the shared
JSON fixtures match. Check actual HTTP handlers, not only internal types.

```text
 Curator durable request + token
              |
              v
 Runner admission receipt ---> download / verify / repair / extract
              |                                    |
              |                                    v
              |                       selected durable publication
              |                                    |
              |                                    v
              +---------------------- durable pending Delivery
                                                   |
                              +--------------------+-------------------+
                              |                                        |
                              v                                        v
                    Runner source cleanup                    Curator durable receipt
                    independent of ACK                                |
                                                                       v
                                                        per-file placement and import
                                                                       |
                                                                       v
                                                         completed-file cleanup
```

Inspect admission capability negotiation, exact token/body replay, immutable
delivery identity, mutable cleanup progress, acknowledgement conflict handling,
manifest ordering/digests, paginated discovery, and accepted-receipt refresh.
Clearing visible history must not erase custody evidence or create a new request.

Verify all-duplicate imports reach a terminal import outcome only after every
selected file has durable redundancy evidence. A ledger-write failure must keep
import retryable, and manual additional copies must bypass this normalization.
For recovery cancellation, test a failed heartbeat cancellation write followed
by a rejected receipt, then a restart after Runner acknowledges cancellation but
the local terminal transaction fails. The exact remote claim must match;
committed library files survive, the receipt stays undelivered audit history,
and an already-cancelled remote claim is not cancelled again.

## 4. Challenge crash, race and filesystem boundaries

Prior review found real defects in these areas. Check final paths and their
regressions; test names alone do not establish coverage:

- Selected publication on the same filesystem and across filesystems; private
  staging before and after its journal commits; interrupted copy, rename and
  hard-link fallback; owned ctime changes versus external file changes.
- EIO and ENOSPC before staging, during copy, after publication and before
  Delivery; typed errors must reach the right retry policy. Hashing must not
  hold global mutation locks or overwrite newer operation state on return.
- Partial health failure with independently usable episodes and archive sets;
  actual PAR damage/recovery-block accounting; corrupt PAR input; diagnostic
  budgets and cancellation; Keep changes after partial publication.
  A good published episode may still be an input to a retained shared PAR set:
  Curator can remove its publication, so Runner must preserve that specific
  dependency independently. Removing Keep must actually schedule reassessment,
  including for failed jobs with no publication. An unknown-integrity sibling
  must remain protected after another sibling publishes; diagnostic text is
  not deletion authority. Exercise Keep on/off after deletion has partly run or
  hit an I/O failure; cancelling and resuming must preserve the exact journal,
  attempts and conflict holds rather than strand the source indefinitely.
- Lost Add acknowledgement and restart; cancellation before dispatch, during
  an in-flight Add, and while a retry waits; manual and automatic paths. For
  qBittorrent, distinguish successful handleless acceptance from a lost response:
  exact request tags must bind later status without title matching or Add replay.
  Missing and ambiguous tags must remain unresolved, including after restart.
  Exercise season-plan dispatch too: it must persist the same correlation
  capability before Add, commit acknowledgement atomically, and bypass legacy
  title-based uncertain-submission recovery. Check that the initial planned
  Add and automatic retry share a download lock and preserve Retry-After.
  Planned imports must retain the stable source GUID and indexer ID needed
  for the episode replacement blacklist choice.
  Check both manual restart branches: acknowledged Add with no handle and an
  uncertain non-idempotent Add. Neither may bind the first of several exact
  matching tags, even if the normal queue matcher rejects ambiguity.
- Cluster grants versus private processing copies: preserve grant ownership
  restrictions, carry the persisted admission cohort only into owned copies,
  and preserve live Keep changes. Sealing and leader adoption must follow the
  selected publication after the original private working path is retired.
- Import commit before terminal receipt, unlink before cleanup receipt, selected
  Resolve after Activity dismissal, and Resolve waiting for another import.
  File-removal recording must atomically persist the directory cleanup
  obligation; directory deletion must have durable pending work before unlink.
  Failure to finish the attempt event or final success stamp must leave that
  work discoverable after restart, even when no payload files remain.
- Import/admission lock order: per-download, then import destination, then
  storage authority. Large staging copies and hashes must leave storage-only
  actions responsive. Final publication must revalidate the identities measured
  outside the lock; a retry must not bless a changed target by refreshing its
  expected identity. Check published-before-commit crash recovery and controlled
  hard-link metadata changes. Released storage-authority contexts must not
  bypass a later lock acquisition.
- Bad-file observation of changed or missing media; trusted storage identity,
  source and parent identity, quarantine replay, and protection after remount.
- Quality replacement whose new file disappears, changes or receives a review
  flag before old-media cleanup. Episode coverage must remain correct across
  mixed-quality packs and shared physical files.
- Pack fallback through the actual importer: it must record why unused episodes
  are disposable, wait for selected replacement coverage, and schedule cleanup
  after that coverage commits even if an earlier cleanup pass retained them.
  A test that inserts excess-file provenance by hand does not prove this flow.
  Include uniquely paired excess subtitles, retain ambiguous/unmatched sidecars,
  and verify a missing current-placement proof schedules a future recheck
  rather than repeatedly occupying the cleanup queue.
- Manual additional-copy import and selected Resolve must carry matching
  subtitles/artwork through the actual queued worker and restart. Unselected
  sibling media and its sidecars must remain untouched.
- Existing layouts on desktop and phone: action visibility, explicit blacklist
  choice, focus entry/dismissal/return, retry feedback and trace pagination.

The proposal's section 9.2 contains the full 34-case acceptance inventory. Report
missing behavior as a requirement gap rather than narrowing the approved scope.

Check Runner's qBittorrent compatibility adapter alongside Curator's native
qBittorrent client. Add tags must survive pending admission, snapshot restart
and `torrents/info`. A second Add with the same native torrent hash must not
relabel the original job with the second request's token. Native Delivery and
request-token guarantees must not be inferred from compatibility tags alone.

Check definite client rejection separately from transport uncertainty.
qBittorrent can return HTTP 200 with `Fails.`, and SAB can return a negative
application result. Definite rejection must commit failed state and rejected
submission phase together, so a crash cannot leave a permanently reserved
request. These must stop the rejected manual request; a lost response
must retain exact-token reconciliation. A new automatic fill request must never
bind to a torrent retained under a prior manual request's tag.

Check admission snapshots with missing or partially emitted source identities.
A durable source artifact/generation pair must survive queue omission, and the
pair must come from one complete namespace. A partial Delivery pair must neither
mix with nor fall back to older Artifact fields. Live source Keep remains
authoritative over a historical queue parameter.

Check hard-link fallback with nested directories and with a private staging
copy: target directories have their own identities, and a scratch hard link
must not be mistaken for a link to the original processing source. Repeated
failed cluster sealing must not accumulate private copies. Check that legacy
selected-file deletion validates the recorded review identity, the live file,
and direct library references before removing anything.

For Delivery-managed selected deletion, interrupt after the first unlink and
before its database completion, reopen the database, and scan the inventory
again. Replay must use the original complete reviewed batch. A changed
fingerprint cannot authorize additional paths. Recreated files, different
parents, replaced publications and changed storage identities must be retained.
An audit insertion failure must roll back the completion record; replay must
produce exactly one deletion event per selected file.

## 5. Acceptance review entry points

These are navigation aids, not a substitute for executing the cases. The final
validation table distinguishes tests that passed from tests merely authored.

| Proposal cases | Start here |
|---|---|
| 1–4, 13–16, 20–21, 26, 33 | Runner `crates/nzbd-post/src/manager.rs`, `crates/nzbd-state/src/artifacts/{publication,relocation,transforms}.rs`, state artifact tests and post-processing pipeline tests. Follow actual worker entry points as well as helper tests. |
| 3–4, 9–10, 17–20, 26, 32 | Curator [delivery tests](../internal/app/acquisition/delivery_test.go) and [cleanup tests](../internal/app/acquisition/delivery_cleanup_test.go). Inspect pending ACK, interrupted unlink, selected re-import, and accepted-receipt refresh. |
| 5, 14, 20 | Curator platform storage-identity implementations and [replacement tests](../internal/app/acquisition/replacement_test.go), especially trusted absence and wrong accessible mounts. Linux CI must exercise the Linux implementation. |
| 6–7, 23, 27–28 | [Manual additional-copy tests](../internal/app/acquisition/manual_additional_test.go), native request-admission handlers in Runner, and the real automatic/manual startup workers. Existing title coverage must not suppress an explicit additional copy. |
| 8, 22–25, 29–31 | [Replacement tests](../internal/app/acquisition/replacement_test.go) and [Runner cleanup/admission tests](../internal/app/acquisition/runner_cleanup_test.go). Check actual selected-file disposal and imported replacement evidence, not only operation states. |
| 11–12, 17, 19, 24, 32 | [Browser action tests](../test/e2e/tests/action-visibility.spec.ts), [queue layout tests](../test/e2e/tests/phase9-layout.spec.ts), per-indexer settings, and Runner's embedded UI harnesses. Inspect the captured desktop and phone screenshots. |
| 34 | [Legacy manual-import tests](../internal/app/acquisition/acq2_manualimport_test.go), [completed inventory/deletion tests](../internal/app/acquisition/completed_test.go), migrations and current database checks before destructive actions. Cached inventory cannot override the durable legacy cohort. |

## 6. Validation and review evidence

Astra approved Curator source checkpoint `7e40206` and Runner `83cd4cd`.
Curator `5243139` changes only the observation test state dispatch to satisfy
pinned lint; the final PR head includes the review handoff documentation.
Resolve the PR head before reviewing and record it in your report.
The Curator full ordinary Go suite, focused acquisition/SQLite/API race checks,
and pinned lint pass. Its latest full coverage result is **85.1%
(23,175/27,227 non-generated statements), below the unchanged 86% floor**.
Baseline `7e8e2f2` passed the same command at 86.3%; this is an introduced
coverage deficit, not a pre-existing gate failure. Independent source review can
proceed, but this result does not establish merge readiness. No gate is waived.

Curator Linux checkpoint `744ae51` passed every actual Go race test, browser
acceptance, mobile, lint, Docker and compatibility check; the test workflow
failed only at 85.0% coverage. The final pushed checkpoint still needs its Linux
results; earlier passing runs do not establish final-head CI success. Runner `83cd4cd` passes all normal Linux PR checks and its dependency
audit. Its separate manual coverage gate remains below 90%; the earlier same-
command macOS comparison failed that floor at both baseline and candidate.
Full details and log provenance are in [implementation status](DOWNLOAD_LIFECYCLE_STATUS.md).

The latest custody regressions verify current library bytes before source
cleanup, nested-parent identity during deletion replay, the durable directory
obligation before removal, additional-copy scope, placement transaction rollback,
and replacement admission surviving a failed operation-step write. Restart
cases reopen SQLite and construct a fresh service. Changed quarantine bytes
block a configured replacement candidate; valid reviewed bytes retire before
one candidate is admitted. HTTP admission rejects cross-copy and mixed-copy
selections without creating operations. Manual-import admission revalidates a
file changed while waiting for its target lock; the queued book worker preserves
unselected siblings. Completed-root discovery survives dismissed history and
uses persisted automatic publications while excluding manual-only roots.

Use disposable fixtures for additional testing. Report findings with exact
repository/head, file and line, concrete trigger, resulting behavior, severity,
and missing regression. Distinguish confirmed defects from unverified concerns.
Do not merge, deploy or execute backlog actions.
