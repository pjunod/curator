# Monarr — Project Status

## A salvage import never strands its download in downloaded — 2026-11-12 · v0.46.2

**Status:** built, targeted tests and lint run (see the commit report) · branch
`claude/overdue-downloaded` from `origin/main` d3b351a · no PR · not merged,
not deployed.

**Field case:** download 1196 (Strangers.With.Candy.S01E10, Runner job 2799 on
nuc4) sat in `downloaded` about 7 h past its deadline. Runner had failed it
`failed{publish}` with salvage pending, and its receipt still says so. The job
is no longer in Runner's queue. The salvage publication (737) was imported
and removed, and Cinema shows it. Runner job 2710 is a different transfer (the
season pack), and no Curator row points at it. Nothing waits on Runner here.

**Root cause:** (1) The salvage import hit a sealed plan from epoch 0 and
reopened the attempt with `BeginImportRetry`. Its `writeImportState` moved the
Runner-failed row to `downloaded` (a 6 h deadline). The salvage then imported
in epoch 1, but only a completed publication sets the row's state (contract
3.1), so nothing ever moved the row again. (2) The overdue exit's
`publicationOwnsExit` counted epoch 0's stage as open custody. That stage is
still `prepared` because placement reconciliation leaves it for the title's
next import or for disposal. So every poll skipped the row before it reached
the "Runner's terminal is recorded" branch.

**Fix:** `writeImportState` records only the handoff step for a row whose
publications are all salvage, and keeps the row's state and error.
`publicationOwnsExit` no longer lets a finished attempt (outcome completed)
own the exit. The leftover stage stays in the placement journal. Tests
`TestOverdueDownloadedSalvageRowTakesRunnersTerminal` (the row's exact shape)
and `TestOverdueSalvageImportRetryKeepsTheRowFailed` both fail without the fix.
After a deploy, 1196 exits at the next poll as `failed{publish}` (blameless).
Its re-grab is filtered because the episode is in the library.

## Overdue exit cancels a still-listed stalled job in Runner — 2026-11-12 · v0.46.1

**Status:** built, targeted tests and lint run (see the commit report) · branch
`claude/overdue-cancel` from `origin/main` 9a5821d · no PR · not merged.
`overdue.go` gains `cancelOverdueInRunner`; `ports.QueueCanceller` and
nzbd `CancelQueued`; tests `TestOverdueStalledJobGetsOneBestEffortRunnerCancel`,
`TestOverdueCancelOfAJobThatFinishedMeanwhileKeepsItsHistory`,
`TestCancelQueuedNeverTouchesHistory`; `docs/usage.md`
(pipeline:overdue). Decision recorded under the v0.45.2 section below.

**CI race-shard failure fixed (2026-11-12):** `TestEndedPackRecordsWhatItPlaced`
failed ~10% of runs (with or without `-race`) on main since 0.45.x. Root
cause, not a cleanup race: `importDownloadFiles` returned at the first file
that did not match its manifest, so the outcome depended on manifest order
(the fixture builds it from a Go map; forcing E02 first fails 5/5). The good
file after a mismatch was never placed, and the mismatched file never got an
attempt outcome, so the attempt's plan stayed pending ("import selection
changed; explicit retry required"). The cleanup runs only after the import
completes. Fix: a payload mismatch is that file's outcome (recorded in the
attempt), the rest of the payload is still tried, then the publication ends
keeping what was placed. Regression test
`TestPayloadMismatchListedFirstStillPlacesTheRest` (fails without the fix).
`go test -race -run '^TestEndedPackRecordsWhatItPlaced$' -count=50`: 0 failures.
Review of 8d42eec: a multipart audiobook is all or nothing —
`verifyBookParts` checks every manifest-constrained part (size and digest)
before any part is placed; one mismatch places nothing, the publication ends
import_failed and the book is grabbed again (blameless; still wanted). Test
`TestAudiobookWithACorruptPartPlacesNothing`. Residual: a part that changes
on disk between that check and its own copy still stops the import after the
parts before it (the old behaviour for books). `docs/usage.md` documents the
partial-keep rule for series/movie packs.

## Overdue downloads exit on their own; sealed plans do not wedge imports — 2026-11-12 · v0.45.2

**Status:** built, targeted tests and lint pass, pushed · owner: Claude Opus
5.5 session · branch `claude/overdue-exit` from `origin/main` 1b9fa58 (no PR;
the coordinator reviews and runs CI) · not merged, not deployed.

Live evidence (nuc3 0.45.1, schema 77, read-only; Runner on nuc4):

- 30 rows past their deadline (28 `downloading`, 1 `held`, 1 `downloaded`),
  45–91 h. Runner's `/api/v1/jobs` queue lists none of them except job 2359
  (#754) and 2938 (#1335); its request receipts still say `paused` (#249,
  #255–#344, jobs 1963–2058), `fetching` (#501, #502), `completed` with no
  publication path (#733, job 2338) or `failed PAR_FAILURE` (#711, whose
  Runner terminal was recorded but whose row stayed `downloaded`). Job 2359
  is blocked `storage_unavailable` since 2026-11-08 past Runner's own retry
  deadline.
- Root cause: `sweepDeadlines` acted only on `held` rows, and `detectGone`
  gave up on an absent job only when its receipt was missing too — a stale
  non-terminal receipt (or a completed one without a publication, which
  `admissionTerminal` cannot apply) read as "alive" forever; rows of a gone
  or disabled client are never polled at all. Overdue was only a warning.
- "cannot determine episodes from <hash>.mkv" (publications 3, 5, 7): each
  publication holds the real episode plus a ~90 MB obfuscated second video,
  so the grab-record mapping (one media file, ADR 0026 decision 22) rightly
  does not apply; the hash file is discarded{rejected} with that reason. It
  repeated because the imports never finished: their attempt epoch was
  sealed by an earlier, never-completed attempt with another file set, and
  every retry failed `import selection changed; explicit retry required` —
  returned uncounted, so it looped forever. Not a mapping gap; a wedge.

Changes:

- `overdue.go`: `exitOverdue` (every Runner poll, after the deadline sweep)
  decides each overdue non-terminal row of the client on Runner's word —
  receipt terminal applied; in queue and moving → deadline extended; Runner
  holding → waits on Runner's hold deadline; blocked past Runner's own retry
  deadline, still for 24 h, or not in Runner's queue (unknown or stale
  receipt) → `FailOverdueDownload`: failed, blameless (no blocklist), reason
  on the row and in History, replacement grab in the same write. A recorded
  terminal on a row that never left its state ends it too. Rows whose
  publication is waiting/importing are left to the publication's exit.
  `exitOrphanedOverdue` (hourly) ends overdue rows of a gone or disabled
  client. An unreachable Runner decides nothing.
- `publications.go`: an import refused with `ErrImportPlanChanged` opens a
  fresh attempt epoch (the uncommitted plan is scratch, like a stage) and is
  returned counted, so the attempt cap bounds it.
- VERSION 0.45.2; `docs/usage.md` (pipeline:overdue section).

Expected on deploy: #249, #255–#344, #501, #502, #711, #733 fail blameless
with their wants grabbed again on the first poll; #754 fails (blocked past
Runner's deadline); #1335 (0.6 h overdue, in Runner's queue) waits for
progress or the stall limit. Publications 3, 5, 7 import on their next
attempt in a fresh epoch.

Adversarial review of this branch — 6 findings, all fixed:

| # | Finding | Fix |
|---|---|---|
| 1 | "Not in queue" decided on one poll | Per-row absence clock (`overdueAbsent`), fail only after `goneGrace` of continuous absence across successful polls; an empty queue while rows are overdue skips the pass |
| 2 | Fresh epoch dropped a Resolve selection | Any run honours the newest `stepManualQueued` paths of this publication (not only approved runs) |
| 3 | Queue-derived failure could overwrite a landed terminal | `FailOverdueDownload(..., runnerSilent)` adds `runner_outcome='' OR provisional=1` |
| 4 | A hold with Runner's deadline ahead was failed after 24 h | Left to wait; the stall limit applies only to holds with no deadline |
| 5 | "Completed without publication" failed at once; custody ignored | Waits `overdueCompletedGrace` (15 min) for the deliveries feed; `publicationOwnsExit` also counts held stages and pending/retained authorizations |
| 6 | Progress compared with `>`; clocks never cleared | Any change is movement; clocks pruned when a row leaves the overdue set |

Expected-on-deploy timings shift accordingly: rows absent from Runner's
queue fail after 10 minutes of successful polls; #733 after 15 minutes.

Decision (Paul, v0.46.1): a job Runner still lists that has not moved for
24 h past its deadline, or that Runner holds blocked past its own retry
deadline, is failed and re-grabbed, and then Curator makes one best-effort
cancel of it in Runner (a queue-only `CancelQueued`, never the history-delete
fallback of Remove; 404 is ignored; data kept; 3 s timeout; fired in the
background after the loop, once the fail is committed). A failed cancel is logged at WARN and
ignored, never retried; the fail and regrab do not depend on it. No cancel
for a job absent from Runner's queue or one with a Runner terminal.

Validation (after the review fixes): gofmt, `go vet`, pinned golangci-lint
on `internal/app/acquisition/...` and `internal/infra/sqlite/...` 0 issues;
`make gen` no diff; `go test ./internal/infra/sqlite/
./internal/app/acquisition/` pass. Not run (by instruction): full suite,
web, e2e.

## Unwedge legacy imports + adopt unclaimed Runner publications — 2026-11-12 · v0.45.1

**Status:** built and tested on its packages, pushed · owner: Claude Opus 5.5
session · branch `claude/unwedge-legacy` (no PR; the orchestrator opens it) ·
not merged, not deployed.

**Merge order (decision, 2026-11-12, coordinator):** this branch ships
before M6b and owns migration **0077** (`0077_unwedge_legacy_imports.sql`),
so it applies on main by itself. `claude/pipeline-v2-m6b-curator`'s
`0077_retire_v1_record.sql` must be renumbered (0078) when M6b is rebased.
The M6b build was never deployed, so no live database is at goose 77 from
it (nuc3 is at 76); this branch's 0077 is the first 0077 any database sees.
Older notes in the v0.44.0 section that call M6b's migration 0077 are
superseded by this.

Live evidence (nuc3, read-only DB at v0.44.0 / goose 76):

- 40 `import_failed` publications (32 live, 8 historical with a download
  row) carry errors only older builds wrote — `N file(s) unresolved: <file>:
  superseded by download N which has not imported yet | the file is cut short
  | existing destination bytes have no measured replacement authorization |
  no acquisition record for …` and `the publication holds no importable media
  (samples and sidecars only)` — with unresolved/pending files. v0.44 never
  re-reads an import_failed row and the sweep never decides unresolved files,
  so they sat forever. 0076 matched only stage/stall reasons.
- Runner jobs 2707/2755: each failed at publish (`publication file changed
  before commit`, salvage pending) as downloads 1100/1148. Those rows were
  later deleted from Activity (Clear failed deletes every failed row without a
  publication; a pending salvage has none yet). Runner then delivered the
  committed directories (deliveries 549/550, readyAt 2026-11-11 22:39); the
  feed fact found no row for transfers `t-0-0afae…`/`t-0-faec…`, was
  dropped as "not Curator's", and the cursor moved on.
- Publication 344 is a historical record at `/working/monarr/completed` — the
  storage container under the configured root `/working/monarr`. Root cause:
  migration 0040 fenced every download's `import_path`; a pre-delivery
  download whose single-file payload sat directly in the completed directory
  reported that directory as its import path, and migration 0073 turned each
  fenced path into a historical publication without asking whether it was a
  payload directory. Because a publication claims everything inside it,
  Download storage showed every folder as "inside publication
  /working/monarr/completed" and refused their whole-folder deletes.
- The other 248 historical record-only rows (0040 cohort: no download row,
  no manifest, no files) point at directories that no longer exist
  (`/working/monarr/…` 147, `/processing/failed/…` 100, `/processing/…` 1;
  checked on the host through the container's mounts).

Changes (after the Fable adversarial review of this branch; every finding
fixed — see the review table below):

- **Migration 0077**: every import_failed publication with a download row
  whose error is a legacy wording or that holds unresolved files, and has an
  undecided file, is re-decided. Owned by an enabled Runner client →
  discovered with fresh limits (attempts 0, no deadline/hold/retry),
  unresolved → pending, held stages released and pending authorizations
  disposed (as 0076); the import decides each file's want (no inference
  from a download's state). Any other client → files discarded{rejected},
  a replacement grab for a completed publication, the sweep retires it.
  Skips a publication a person is deciding. Unresolved/pending files of
  *removed* publications → discarded and removed. Idempotent (tested).
- **Every waiting publication has an exit** (`offerWaitingPublications`,
  hourly): each discovered/importing publication is offered to the import
  workers when its client is an enabled Runner (deadline and attempt cap
  then bound it), ended by `endPublication` otherwise.
- **Pending media of an ended import** (ADR 0026 decision 27): discarded
  only when the want is filled or a replacement imported it; otherwise the
  publication is requeued (the requeue counts as an attempt; out of
  attempts → discarded). Writes are conditional on the publication as read.
- **Pending-salvage rows**: Clear failed, Remove and retention hide rather
  than delete a failed row whose salvage is pending; the hourly pass settles
  it from Runner's receipt (unknown, no longer pending, or seven days).
- **Adoption** (`publication_adopt.go`, hourly): each payload directory
  (the root's children, and its `completed` container's children) with a
  valid manifest naming a Curator `t-…` transfer and no publication is
  attributed exactly once, on Runner's word (row terminal → salvage; open
  row → Runner's receipt terminal, waiting while unreachable or running).
  No row → an orphan only after 24 h and with every enabled Runner client
  reporting the token unknown or ended: files discarded, history
  `publication_orphaned`, retired by the same pass.
- **No publication at a root or its container** (`publication_records.go`):
  refused at discovery, salvage, adoption and cutover marking — also through
  bind-mount aliases (`storagePath`), fail closed when roots are unreadable —
  and ignored by the inventory fence and `publicationClaiming`.
- **Record-only publications end** (hourly): deleted (freeing the path;
  history `publication_closed`; nothing on disk touched) when the path is a
  container (344) or the directory is gone with its storage verified before
  and after the check (root storage identity; outside every root, the
  mount of a verified completed root — otherwise never). A different payload
  (another generation) later published at a removed publication's path gets
  a new publication.
- **ADR 0026** addendum decisions 27–30; stale "unresolved files remain
  available in Resolve" texts fixed (inventory reasons, `docs/settings.md`).

**Decision for Paul to review:** an orphan's files are discarded (its
directory deleted) rather than held — only after 24 h and Runner confirming
the token unknown or ended; Curator minted the token, the attempt is gone
from its records and its want was re-grabbed when it failed. Live: job 2707
(Victor Frankenstein; library holds a copy via pub 813) and job 2755 (Good
Trouble S04E10; download 1276 imported it).

How each live group resolves after deploy:

| Group (live DB) | Resolution |
|---|---|
| 40 import_failed legacy (492, 501, 546, 559, 617, 639, 659, 668, 699–701, 704, 706, 710, 713, 715, 716, 719, 725, 728–731, 733–738, 799–801, 813; historical 6, 10, 12, 14, 29, 68, 236) — all owned by the enabled Runner client 3 | 0077 → discovered; the import places or discards every file (a want already filled → duplicate) |
| 5 live import_failed, all files already discarded (666, 667, 714, 717, 718) | already sweep candidates; next hourly sweep removes them |
| pending sidecars in ~40 live imported publications | existing sweep re-decision |
| pending media in approved historical imported pubs (17, 87, 89, 92–94, 108, 121, 167) | sweep: discarded where the want is filled, else requeued (bounded) |
| unresolved sidecars in removed pubs (538–541, 567) | 0077 → discarded, removed |
| unresolved files in discovered pubs (~80) | the pending import decides them |
| orphan dirs job-2707, job-2755 (manifests from 2026-11-10; Runner's receipts say failed) | adoption → orphan, discarded, removed |
| pub 344 (container record) | record deleted by the first hourly pass; never fenced anything after deploy |
| 248 historical record-only rows, directory gone (147 under `/working/monarr`, 101 under `/processing`) | record deleted once storage is verified (`/processing/…` through the `/processing` mount shared with the `/processing/completed` root) |

**Still needs the M7 approval (ADR 0026 addenda 8 and 10 require
`cleanup_approved_at` before anything in a historical publication is
deleted; the coordinator runs approve-cleanup after deploy):** historical
publications with a download row and no approval — **3, 5, 6, 7, 9, 10, 12,
13, 14, 29, 68, 236**. They import (or are already decided) automatically;
only their file deletion waits for the approval. After it, the per-file
sweep retires them with no further step.

Fable adversarial review of this branch — 8 findings, all fixed:

| # | Finding | Fix |
|---|---|---|
| 1 H | Requeued rows only offered by an enabled Runner client's poll | 0077 requeues only those; others end with a regrab; hourly offer/end of every waiting publication |
| 2 H | Record-only rows closed on weak evidence; closed path still reserved | Storage verified before and after `Lstat`; outside roots only on a verified root's mount; record deleted; new payload at a removed path gets a new publication |
| 3 M | Sweep discarded pending media against ADR 25 | Discard only when the want is filled/replaced; else requeue (bounded); CAS on the publication; ADR decision 27 |
| 4 M | Adoption forged a completed terminal | Runner's receipt decides; wait when unreachable or running |
| 5 M | Container refusal lexical only | `storagePath` aliases; fail closed when roots unreadable |
| 6 M | Weak orphan test; retention ignored pending salvage; `pending` never settled | 24 h + Runner confirms unknown/ended; retention guard; settle pass |
| 7 L | "Satisfied" inferred from download state | Removed: the import decides each want |
| 8 L | Docs, ADR, STATUS contradictions | ADR decisions 27–30; stale texts; superseded 0077 notes; M6b never deployed |

Known residue: SQLite may reuse a deleted publication's rowid; a
`notifier_deliveries.publication_id` (no FK) of a long-removed publication
could then name the new row. Display only; noted for Paul.

Validation (after the review fixes; Go 1.25.7 / pinned tools): gofmt
clean; `go vet` and pinned golangci-lint v2.12.2 on
`internal/app/acquisition/...` and `internal/infra/sqlite/...`; `make gen` +
`git diff --exit-code -- internal/infra/sqlite/gen internal/api/gen`
clean; lint 0 issues; `go test ./internal/infra/sqlite/
./internal/app/acquisition/` pass. Not run (by instruction): full `make test`, `make
test-web`, `make test-e2e`.

## No wedge: hash once, scratch stages, background reconcile, 4 workers, subtitles — 2026-11-11 · v0.44.0

**Status:** built, awaiting adversarial review · owner: Claude Opus 5.5 session
· branch `claude/no-wedge` (no PR yet; the orchestrator opens it after review) ·
plan: Fable 5.2 consult, Curator items A–E, plus Paul's subtitle requirement ·
not merged, not deployed.

Built:

- **A — hash once.** `placementSourceDigest` takes the manifest digest for a
  publication's source (publications.go passes `withExpectedSourceDigests`),
  `stageImportPlacement`/`placeTemp` hash the copy stream, and
  `preparePlacementPublication` compares that digest instead of re-reading the
  stage. Same filesystem: the stage is a hard link (`linkImportStage`), no copy.
  `verifyCrossVolumeCopy` is gone (a linked cross-volume file is hashed once).
  Resume (`reuseCommittedImport`), crash recovery and `cleanupPlacement` decide
  by recorded identity (inode, size, mtime; `sameContentGeneration`).
  Publication cleanup no longer hashes the source or the library copy
  (`libraryCopyHolds`: placement journal record by stat; hash only without a
  record). The verify budget paces only background proofs.
- **B — stages are scratch.** No "existing stage retained / custody review"
  state: a stage is reused only if the journal recorded it proven, else deleted
  and re-copied. Stall deadline from import start (`StartPublicationImport`).
  Migration 0076 requeues `import_failed` rows whose error is a stage/custody,
  queue-stall, verification-race or obfuscated-name failure; imported,
  removed and other rows are untouched.
- **C — reconcile in the background**, per-title import target lock, by stat;
  unsure rows stay prepared and the next import redoes the copy.
- **D — 4 import workers** (`MONARR_IMPORT_WORKERS`). Locks stay per title.
- **E — obfuscated single-episode grabs map by the grab record** (samples do
  not count as payload media; a contradicting release title maps nothing).
- **Subtitles** placed with their video in the same import, before the
  outbox write; other sidecars `discarded{rejected}`, never `unresolved`.

Decisions taken without Paul (for review):

- Locks stay per title, not per destination directory: an import decides a
  title's episodes once for all its files, and finer locks would race those
  decisions. With the whole-library lock off the import path, titles already
  import in parallel.
- Non-Runner imports (other clients, manual) still hash the source once: the
  placement journal is keyed by content and they have no manifest.
- A replacement import still hashes the existing library target once at
  preview (`PreviousSHA256`); a new destination costs nothing.
- Migration 0076 also requeues "cannot determine episodes" failures (E makes
  most of them importable; one that still is not fails again once, normally).
- Migration number 0076: `claude/pipeline-v2-m6b-curator` had claimed 0075;
  it is now 0077 on that branch (`d61ea59`), so the two merge in either order. *(Superseded 2026-11-12: `claude/unwedge-legacy` owns 0077 and ships first; M6b renumbers to 0078 when rebased.)*
- Manual and approval-held imports keep the files the person did not select
  as `unresolved` (the person asked to decide); every automatic path decides.

Adversarial review r1 (Fable) — 12 findings, all fixed:
1-2 a publication has no deadline until its first import starts, the
deadline is never reset, payload failures are counted
(`MaxPublicationImportAttempts` = 5), and either limit ends it
`import_failed` with a replacement grab; 3 an unreadable or symlinked
subtitle is discarded with its reason; 4 a subtitle never overwrites a file
Curator did not place (next free suffix); 5 the link path never chmods and
falls back to a copy on any problem; 6 manifest sizes are checked by stat
before linking or copying, a mismatch ends the publication for a regrab;
7 replaced subtitles' rows become `discarded{superseded}`, and no import or
cleanup path produces `unresolved` any more (only a person's own manual or
approval-held selection does); 8 leftover sidecars only follow a video that
is still the publication's placement, and download-less publications
resolve too; 9 language: `hi` is Hindi alone and SDH beside a language,
`fr-CA` is one tag, codes only after the video stem or as the only token,
the last token only; 10 placed-subtitle lookup is a byte range on an
indexed column; 11 0076 retires (discarded{superseded}) wedged rows whose
want a later import satisfied and releases the custody rows of requeued
ones; 12 `claude/pipeline-v2-m6b-curator` renumbered its migration to 0077
(`d61ea59`). *(Superseded 2026-11-12: `claude/unwedge-legacy` owns 0077 and ships first; M6b renumbers to 0078 when rebased.)*

Adversarial re-review r2 (Fable, of `032720d`) — 8 findings, all fixed:
1 environmental stops (blocked/full library, storage identity, a manifest
unreadable for now) hold the publication (`last_stop`) and are never ended by
the deadline; the sweep never discards pending media; 2 supersede only
against a live attempt or one whose file is in the library; 3 only a
malformed manifest ends a publication, read errors hold it; 4 only typed
payload failures count, spaced on the retry ladder (`retry_at`); 5 ending
records committed placements (history, Cinema outbox), discards the rest
and regrabs only what is still wanted; 6 a person's retry resets attempts
and deadline; 7 a file row the manifest does not list is discarded; 8 M6b
rebased onto this branch (below).

Adversarial re-review r3 (Fable, of `14d12b2`) — 6 findings, all fixed:
1 the deadline sweep re-reads the publication under the row's lock and
completion is guarded (`AND state IN (discovered, importing)`,
`ErrPublicationMoved`); 2 leaving a hold restarts the deadline; 3 holds back
off on the retry ladder and are recorded once when they begin and once when
they clear; 4 `endPublication` holds on an unreadable manifest, title or
placement check instead of ending; 5 holds are exposed (`held`,
`heldReason`, `heldSince` on the queue publication, shown on the Activity
row; `heldPublications` on `GET /system/pipeline`, shown on System), and a
missing manifest becomes a hold after 30 minutes; 6 a publication outside
the configured completed roots is a hold. Web: `tsc --noEmit` and
`vitest run src/api.test.ts` (27 pass) for the touched web code.

**Merge order: merge `claude/no-wedge` first, then
`claude/pipeline-v2-m6b-curator`** (rebased onto this branch; its migration
is 0077 after this branch's 0076). *(Superseded 2026-11-12: `claude/unwedge-legacy` owns 0077 and ships first; M6b renumbers to 0078 when rebased.)*

**Deploy order:** a same-filesystem placement hard-links Runner's file,
which moves its ctime: deploy this curator only after the Runner build whose
publication identity comparisons drop ctime (Fable plan W1). Across the
NFS exports on nuc3 (different filesystems) the placement copies, so the
current deployment is not affected either way.

Verified (fast lane only, by instruction; Go 1.25.7, 2 CPUs): `gofmt -l`
clean; `go vet` and pinned `golangci-lint v2.12.2 run` on
`internal/app/acquisition`, `internal/infra/sqlite`, `internal/docs` (0
issues); `go test` of those three packages, whole packages, all pass. New
acceptance tests: `TestPublicationImportReadsEachByteOnce` (copy to another
filesystem reads 1.0x and writes once; same filesystem links and reads ~0),
`TestRetryDiscardsAnInterruptedStage`,
`TestPlacementReconciliationRunsInTheBackgroundPerTitle`,
`TestMigration0076RequeuesOnlyWedgedImports`,
`TestObfuscatedSingleEpisodeGrabImportsByTheGrabRecord`, the subtitle tests
in `publication_subtitles_test.go`.
Not run: `make test` (other packages), `make test-web`, `make test-e2e` (web
untouched; the single gate runs once in CI after review).

Closed after the first report: sidecars earlier builds left `unresolved` in
imported publications are re-decided by the cleanup pass (migration 0076
makes them pending; the placed video is not touched), and an upgrade that
replaces a video removes the subtitles Curator placed for it (by placement
record and placed digest) and places the new release's. Historical
publications not yet approved get this when their review batch is approved
(the sweep does not visit them before).

## Pipeline contract v2, M6a (curator part): one retry ladder, dead fields out, docs rewritten — 2026-11-08 · v0.42.0

**Status:** PR #101 (draft, base `main`; #97 has merged) · owner: Claude
(Sonnet builder; Opus orchestrator merges) · review: Sonnet review **approve**
with fixes `b53448e`; Fable adversarial review r1 **request changes**
(`108cdef`) -> the fixes below; the single gate (`make lint`, `make test`,
`make test-web`, `make test-e2e`) is **pending** and runs once after the fixes ·
not merged, not deployed. Nothing here depends on a Runner release. M6b
(`claude/pipeline-v2-m6b-curator`, stacked on this) is the deploy-gated half
and is **not** safe to merge with it.

Review r1 fixes: issue #100 folded in (dead `EnsureDeliveryDirectoryIdentity` and
`UpdateDeliveryFileDisposition` deleted, the `t.Skip` in `publication_paths_test.go`
is now a `t.Fatalf`, `publications.cleanup_paused_at` in `requiredColumns`); the
plurx Retry-After cap reads `backoff.Outbox.Max()`; `internal/docs` evaluates the
code's constants and checks them per section and in the literals table;
`STATUS.md` links are checked.

Built (plan M6 curator items 2 and 3, IMPL_CURATOR section 8):

- **One retry ladder.** `internal/domain/backoff` (`Ladder.After(tries)`,
  `Retry` and `Outbox`) replaces four private ladders: runner submission and
  manual admission (`infra/sqlite`), the blocked-import retry
  (`acquisition/importretry.go`) and the notification outbox
  (`notify/deliver.go`). A table test keeps each old function verbatim and
  proves the same sequence over 40 attempts (`TestLaddersMatchTheSchedulesTheyReplaced`).
  The import-retry test seam (`importRetryBackoff = 0`) is untouched.
- **`measured` and `note` deleted** from `GET /system/pipeline`: OpenAPI schema,
  generated code (`make gen-api`), handler, the web `measured: false` tile
  branch and its test, and the "until pipeline M5" note. The `parkedReason`
  description no longer says "Removed in M4" (it was not: season custody and
  non-Runner clients still use it). This is a correction outside the stated M6a scope (the field stays; no wire
  change), listed here so it is not found by diff.
- **`docs/integration.md` rewritten** from the contract: the milestone-by-
  milestone sections (M2, M4, M5) and the superseded "Durable job control
  contract (v1)" are folded into numbered seams (facts and terminals, held jobs,
  publications and per-file cleanup, Cinema sub-deliveries), the transfer token
  is described as opaque (`t-<id>-<32 hex>`), and a literals table is checked by
  `internal/docs` (event names, both ladders, deadlines, lane width, scope,
  timing kinds).
- **`docs/README.md`** indexes every file under `docs/`; `internal/docs` fails
  when a file is not listed, an ADR is missing from `docs/adr/README.md`, or any
  relative link (and heading anchor) in `README.md` or `docs/**/*.md` does not
  resolve. It found and fixed one anchor that was already broken
  (`smart-media-discovery-m0c-probe.md`).
- **Archive**: `plan-integration.md`, `DOWNLOAD_LIFECYCLE_PROPOSAL.md` and the
  five `RUNNER_FAILURE_*` files moved to `docs/archive/` (`git mv`), each with a
  one-line pointer; every link the move broke is rewritten (README, five docs,
  PIPELINE_CONTRACT, status.html). ADR 0026 gains cross-links and an addendum;
  the ADR index lists 0024 and 0025, which it had missed.

Deleted (non-test, non-generated): the four private ladders, the
`measured`/`note` code in Go, TypeScript and OpenAPI (generated file -6 lines),
and from `docs/integration.md` the milestone narratives and the v1 control
contract (308 lines removed, 381 added: 946 to 1019 lines, because the new
sections state facts the old page left to the contract).

Moved to M6b (deploy-gated, not done here): the dead v1 receipt writers and
readers in `infra/sqlite/delivery_receipts.go`, the `translate()` legacy
branches, and the `v1_record` source. They are read by the v1 adapter and the
receipt screens until the tables go, so deleting only the writers would leave
test fixtures with no production code to exercise.

Not removed on purpose: `RemoveFileRequest.Blocklist`/`Search` (marked
Deprecated in Go): they are the old remove endpoint's wire shape and are
rejected explicitly, not dead code from M2/M4/M5.

`TestImportWritesTheOutboxInItsTransaction` predated M5 (which gives a chat
notifier its own one-shot outbox row); it was fixed on #97 itself (`e9f7aca`)
and arrives here by the merge of that branch.

Verified (fast lane): `go build ./...`; `go vet` and `go test` of
`internal/domain/backoff`, `internal/docs`, `internal/app/notify`,
`internal/app/acquisition`, `internal/app/pipeline`, `internal/infra/sqlite`,
`internal/api`; `gofmt -l`; web `tsc --noEmit` and `vitest run src/api.test.ts`.
All pass except `TestQueueImportHTTPRecoversFailedImportAfterPayloadPathIsRepaired`
(the known root-only failure: the sandbox user is root).
Not run (by instruction): `make test`, `make test-web`, `make test-e2e`, the
coverage floor.

Decisions for Paul: the ladder helper is `internal/domain/backoff`, not
`internal/app/pipeline/backoff.go` as the plan says: `internal/app/pipeline`
imports `internal/infra/sqlite`, and the submission and admission deadlines are
computed inside sqlite, so a pipeline-package helper would be an import cycle.
Version bumped to 0.42.0 (a removed API field and a new test package are more
than a patch; M2/M4/M5 each took a minor).

## Pipeline contract v2, M5 (curator part): outbox sub-deliveries and read-back — 2026-11-07 · v0.41.0

**Status:** [PR #97](https://github.com/pjunod/curator/pull/97) on
`claude/pipeline-v2-m5-curator` (main merged in after M2 + M4 landed as #95;
migration **0074**, M4 holds 0073) · owner: Claude (Opus orchestrator) ·
approved by Fable, gate running, not merged, not deployed. Built to contract
section 4 / section 6 and checked against cinema's real handlers (Forgejo
#1404-1409).

M6 follow-up: the `measured` field of `GET /system/pipeline` is now always
true (every seam has an end), so the web `measured: false` tile branch and the
OpenAPI "until pipeline M5" note are dead; deleted in M6a (v0.42.0).

Built (plan M5 curator items 1-4, IMPL_CURATOR section 7):

- **Outbox, not bus.** Every notification of an import is a
  `notifier_deliveries` row written in the import's own transaction
  (`CompletePublicationImport`, or `RecordImported` for a legacy client /
  manual import). The dispatcher's `Subscribe[ImportCompleted]` (32 slots,
  drop on full) and its `enqueue` are deleted; `ImportCompleted` stays on the
  bus as the SSE live signal only. Chat notifiers and Plex/Jellyfin get one
  row each, attempted once.
- **Migration 0074**: `publication_id`, `directory`, `idempotency_key`,
  `attempt`, `expected_files_json`, `remote_request_id`, `remote_state`,
  `waiting_on`, `visible_at`, `item_ids_json`; unique
  `(notifier_id, idempotency_key)`. One sub-delivery per (notifier,
  publication, directory), key `<transfer>/<generation>/<sha256(dir)[:16]>`,
  expected files `(path, bytes, mtime_ns)` stat'ed after placement. A
  re-import of the same directory refreshes the row and advances its attempt.
- **Dispatcher**: per-sub-delivery 20 s deadline; schedule `5s, 30s, 2m, 5m,
  15m, 1h`, then hourly, no end for transient; permanent only on 401/403 (POST),
  400, 422 `path_outside_roots` (or an old 422 with `roots`). Ambiguous
  outcomes (timeout, connection error, 429, 5xx) reuse the attempt; confirmed
  retryable ones (`503 path_unreadable`, a `failed` or vanished request, an
  expected file not placed) advance it. Visible only when every expected file
  is `placed` with equal bytes and mtime_ns → `visible → plurx item <ids>`;
  else `warning: expected <path> not placed (<reason>)`. A 202 is polled with
  `GET /scan/requests/{id}`; `waiting_on` stored and shown. Old cinema: 403 on
  the poll, or a 200 without `files` → `ok`, `queued (cinema does not report
  results)`, timing discarded.
- **plurx.go**: `SendScan` (request_key/<attempt>, expected_files, wait_ms
  10000), `ReadResult`, typed `NotifyError` classification; the permanent-429
  default is deleted. `TestPaths`: `probe_paths` = Settings → Library roots,
  fails with Curator's path beside Cinema's roots; a 422 without `roots` (the
  old extractor) falls back to the legacy probe with the note "checking the
  library paths needs a newer cinema".
- **`import_to_cinema_visible`** starts in the publication's `imported`
  transaction (only when a Cinema sub-delivery is written) and ends when the
  import's last pending sub-delivery becomes visible (same transaction as the
  settle). `pipeline.MeasuredBy` and the "not measured — needs M5" tile note
  are deleted.
- **API/web**: `Delivery` gains `directory, attempt, requestKey,
  remoteRequestId, remoteState, waitingOn, visibleAt, itemIds, expectedFiles,
  warning` (pending 30 min after it was written); Test answers
  `{message}`. Delivery log: Directory column with the attempt, `visible` and
  `warning` pills, Cinema's exact `waiting_on`/skip text. Retry keeps the key
  and advances the attempt.
- **Docs**: integration.md (new M5 section, seams 4 and 5), usage.md (Pipeline
  tile, notifications), settings.md (Test, failure table, queue), architecture.md
  (event table).

Deleted (non-test): the bus import hop in `notify.go` (subscription, `Outboxed`
plumbing, `dispatchImport`, `enqueue`), the `5s/30s/2m`-then-fail schedule and
its exhaustion branch, the permanent default (incl. 429) in `plurx.go post`,
`EnqueueDelivery` (sqlc query + Go), `pipeline.MeasuredBy`.

Tests (fast lane): `internal/app/notify/outbox_test.go` against a fake Cinema
(`internal/adapters/notify/cinematest`, new and old handler behaviour) with the
real adapter and a hand-moved clock: outage 10 min → three directories
visible with item ids, no retry; lost response → same key, one request; three
slow directories each with their own deadline, never re-posted after visible;
skip → warning → attempt 2 → visible; same-path replacement with equal size
and whole-second mtime → visible only when mtime_ns matches; 503 → visible;
outside root → failed once; queued → polled with waiting_on → visible; old
cinema 403 / 200-without-files → ok. `plurx_v2_test.go` (wire, classification,
ReadResult, TestPaths incl. `TestOldCinema422WithoutRootsFallsBack`,
`Test429IsTransient`), `outbox_import_test.go` (completing tx writes the
sub-delivery, key, expected files, timing start), `sub_deliveries_test.go`.

Not verified: `make test`/`test-web`/`test-e2e` (not run, by rule); the compat
test runs against a faithful fake of the old handler (path required by the
extractor, 403 on status read), not the Rust handler itself; no e2e spec
(`notifier-deliveries-v2.spec.ts`) was written — it could not be run here, so
the delivery-log layout is unchecked in a browser. Pre-existing failure on the
M2 base, not touched here: `internal/api`
`TestQueueImportHTTPRecoversFailedImportAfterPayloadPathIsRepaired` (204, want
400; fails identically at 6e5fe39).

Decisions for Paul: chat-notifier imports also moved to the outbox (one
attempt each) rather than losing them when the bus subscription went;
`ImportCompleted` kept on the bus for the SSE stream; `warning` derived as
"pending 30 min after the row was written"; a permanently failed
sub-delivery leaves its timing open (it is genuinely not visible) while an
old cinema's unreportable one is discarded; a vanished request (404 on the
poll) and other 422s are retried (literal reading of "permanent only on");
VERSION 0.41.0 assuming M4 takes 0.40.0.

Review r1 (Fable, `reviews/curator-97-fable-r1.md`, changes requested) —
all eleven findings and the cinematest divergences fixed; table in the
report on PR #97. Two cinema facts taken from the review, not read from
source (Forgejo is unreachable from the build workspace): the byte cap on
expected files is assumed to be the 16 KiB of the stored JSON
(`scan_request_v2`'s CHECK), and a failed request's typed body is assumed to
carry `code`.

**Before deploy (finding 9, a contract question if it fails):** on nuc3,
for one file Curator placed, compare the nanosecond
mtimes host-side (`stat -c '%y' <host path>`) and inside the plurx container
(`docker exec plurx stat -c '%y' <container path>`). If the fractional
seconds differ (e.g. CIFS 100 ns vs ext4 ns), every file would be reported
`placed` with a mismatched `mtime_ns` and re-scanned hourly forever: stop and
take it to Paul — the comparison stays exact equality until the contract
says otherwise.

Post-deploy (after merge, with cinema M5): `import_to_cinema_visible` p95 < 2 min
over a day — `curl -s -H "X-Api-Key: $KEY" $MONARR/api/v1/system/pipeline` —
with the waiting count beside it; the delivery log shows `visible → plurx item
<id>` on new imports; `SELECT status, count(*) FROM notifier_deliveries WHERE
idempotency_key != '' GROUP BY 1` has no growing `pending` tail.
## Pipeline contract v2, M2 + M4 land together: coverage floor — 2026-11-08 · v0.40.0

**Status:** [PR #95](https://github.com/pjunod/curator/pull/95) on
`claude/pipeline-v2-m2-curator` · owner: Claude (Opus orchestrator) ·
M2 and M4 both approved by Fable (#95, #96); not merged, not deployed.

**Why M4 lands with M2:** #95 failed CI only on the coverage floor
(82.9% against `COVERAGE_MIN` 86.0%; all 1796 tests passed). Most of the gap
was the v1 receipt cleanup M2 left live (`delivery_cleanup.go`, 974 uncovered
statements), which M4 deletes. Testing code that the next milestone deletes
would be wasted work, so M4 (#96) is merged into this branch and the two ship
as one PR at M4's version, 0.40.0. Decision taken for Paul by the
orchestrator.

Merged in order (merge commits, no rebase):

1. `claude/pipeline-v2-m2-cov-a`: acquisition tests (reducer fact x state
   matrix, publication import paths, grab admission and removal, replacement
   grab worker and operation steps, v1 receipt lifecycle after the cutover);
   `replacementAttemptCleanupBlock` removed (a stub that never blocked).
2. `claude/pipeline-v2-m2-cov-b`: the 29 receipt-store writes M2 left without
   callers deleted from `delivery_receipts.go` (-676); storage-fault matrices
   for the reducer, publication, replacement and acquisition stores and the
   recovery worker (template-copied databases, run in parallel).
3. `claude/pipeline-v2-m4-curator` (#96): see the M4 entry below.

Defects fixed by the coverage work (7):

| # | Where | Defect | Fix |
|---|---|---|---|
| 1 | `completePublication` | A salvage import moved its failed row to `imported` (contract 3.1) | Settles its attempt with `SettleImportAttempt`; the row stays failed |
| 2 | Resolve placement | Placing an unresolved file re-ran the import in its old attempt epoch and failed on "existing stage retained" | A Resolve selection opens a fresh attempt epoch, like ImportNow |
| 3 | `downloadsWhere` | A read failing mid-scan returned a truncated list as complete (continuity-loss reconcile, held sweep) | Checks `rows.Err` |
| 4 | `SetPublicationFileOutcome` | Discarded the RowsAffected error and reported not-found for a write that landed | Returns the error |
| 5 | `buildWanted` / `Wanted` | One read failure hid a title from RSS, backlog and Wanted for the rest of the day (cached as fresh) | Fails the walk; nothing cached. `WantedList` fails on an unreadable profile list |
| 6 | `CreateReplacement` | A storage error re-checking the review was reported as `ErrReplacementStale` | Only a missing observation or a missing/conflicting pin is stale |
| 7 | `DeleteProfile` | A failed read of the kind defaults counted as "not a default", so a kind's default could be deleted | Any read error other than unset fails the delete |

Integration (merge of M4): conflicts in `acquisition_handlers.go` (M2's
batched `publicationFiles` loop with M4's cleanup state, pause and `removed`),
`Activity.tsx` (M2's focus move with M4's "discarded and removed"),
`delivery_receipts.go` (only what both sides kept survives) and eight test
files (tests of code M4 deleted stay deleted). Adapted to M4: the cov-a receipt
lifecycle tests seed legacy receipts with SQL (cov-b deleted the writers) and
drop the abandon-route test; six v2 import tests expected `imported` where
M4's cleanup now ends a fully evidenced live publication `removed`. sqlc
output regenerated (`publications.cleanup_paused_at`).

Coverage: projected **87.65%** against the 86.0% floor (CI's method: `-coverpkg=./...`,
blocks merged across profiles, `/gen/` excluded). Measured on this branch for
the three packages the work changed, with every other package at main's
numbers (87.61% with M2 head's numbers instead):

| Package | main | #95 at ae07935 | now |
|---|---|---|---|
| `internal/app/acquisition` | 82.5% (10519/12747) | 75.5% (9303/12320) | 85.6% (10041/11728) |
| `internal/infra/sqlite` | 85.7% (4368/5096) | 83.3% (4547/5457) | 88.1% (4333/4916) |
| `internal/api` | 89.2% (2519/2824) | 88.8% (2562/2886) | 88.2% (2576/2920) |

Measured without `-race` (coverage blocks are the same; the race build of the
sqlite package did not finish in 40 minutes on the shared 2-CPU workspace).
Full runs of the three packages on the final head: acquisition and sqlite pass;
api passes except `TestQueueImportHTTPRecoversFailedImportAfterPayloadPathIsRepaired`,
which relies on a read-only directory and fails only as root (it passes run as
an unprivileged user; unchanged from main). CI's floor is the check of record.

## Pipeline contract v2, M4: per-file cleanup, dead v1 mechanisms removed — 2026-11-07 · v0.40.0

**Status:** branch `claude/pipeline-v2-m4-curator` (stacked on
`claude/pipeline-v2-m2-curator` @ 6e5fe39) · owner: Claude (Opus builder) ·
pushed, **no PR** (by instruction), not reviewed, not merged, not deployed.
Decision record: [ADR 0026](docs/adr/0026-pipeline-contract-v2.md) addendum
(items 9-12); ADR 0021 marked partly superseded.

Review r1 (Fable, [PR #96](https://github.com/pjunod/curator/pull/96), changes
requested) — every finding fixed on this branch:

| # | Finding | Resolution |
|---|---|---|
| 1 | Storage-change fence not on the per-file path; storage-review delete could unlink inside a publication | `cleanupGate` (live identity, then `StorageChangeReview`/`StorageReviewPath`) at pass start, before every unlink and before the rmdir; automatic passes stop, an operator's own discard is the review. Inventory classifies every publication-claimed entry regardless of the review flag; `DeleteSelectedLegacyFiles` refuses a publication path. Tests: path fence, generation fence, inventory + refused delete |
| 2 | Stray after manifest unlink strands the publication | With every manifest file removed the stored manifest suffices; Storage deletes such a publication whole (`wholeDeletable`, identity + ownership checks). Test with `.DS_Store`, manifest gone too |
| 3 | Folded 0040 cohort undeletable, shown as awaiting import | 0073 (and the M2 cutover marking) records manifest-less, row-less directories as `import_failed` with the reason; approved, Storage deletes them whole. Test |
| 4 | Stale `publications.error` | Cleared when a pass ends with nothing stopped (only cleanup's own messages). Test |
| 5 | Sweep hashing inline, unattributable | The sweep only queues passes on the cleanup lane; each pass is in the in-flight registry (`publication_cleanup`, with why) and on the publication in Activity with Pause/Resume (`POST /downloads/publications/{id}/cleanup`, `publications.cleanup_paused_at`). Test |
| 6 | Resolve Discard hid a kept file | `ErrDiscardedButKept` (409 `discarded_kept`) with the reason; Activity shows it. Test |
| 7 | Exact path lookups | `publicationClaiming`: same, containing or contained directory (storage paths); used by the legacy cleanup fence, Storage deletes and the inventory. Test |
| 8 | Other platforms re-hash forever | `perFileCleanupSupported` is false there: the sweep logs once and refuses, the pass stops |
| 9 | Preflight refusal had no operator path | integration.md: roll back to v0.39.x, finish or cancel the named requests, upgrade again |
| 10 | Docs/UI literals | integration.md storage-check wording fixed; Storage offers only discardable (unresolved) publication files, with corrected text |
| tests | crash between unlink and record; pre-unlink generation re-check; hard-linked placement; digest-less manifest; fences | added (fence and pre-unlink tests mutation-checked) |

Built (plan M4.1-3, IMPL_CURATOR section 6):

- **Per-file, evidence-bound cleanup** (`publication_cleanup.go`): a
  publication file leaves only when `placed` (placement recorded the manifest
  SHA-256 and the library file still has it: same inode, else re-hashed) or
  `discarded`; only on `live` publications or `historical` ones with
  `cleanup_approved_at`; `unresolved`/`pending` never. Before each unlink:
  generation (the directory's manifest, or the stored v1 manifest), file
  identity (size + digest vs manifest, then an unchanged inode generation
  since hashing, checked under the import-target and storage-decision locks),
  storage identity of the completed root (`checkStorageIdentity`, twice).
  Directory `rmdir`ed when only the manifest remains. Never size-only: a
  manifest entry without a digest is kept. Evidence that no longer holds
  demotes the file to `unresolved` (Resolve shows why; the sweep stops
  re-reading it).
- **One implementation** for: the post-import cleanup (cleanup lane, inline
  when no workers), the hourly sweep (`CleanupPayloads` →
  `sweepPublications`), Resolve → Discard, Storage → Delete selected files on
  a publication directory (`DiscardPublicationFiles`: unresolved files only,
  same checks — the operator "delete this leftover"), receipt "Retry Curator
  cleanup", and the M7 approval (`POST /downloads/publications/approve-cleanup`).
- `awaiting_import` shows its age in Activity (`stateEnteredAt` on the queue
  item; `web/src/queueTime.ts`); publication files show *removed*; approved
  historical publications are labelled.
- **Migration 0073** (count check in Go before it runs: refuses, naming the
  batch ids, while an operator's v1 explicit deletion request is unfinished):
  storage-change path fences → `storage_change_review_paths`; the 0040 review
  cohort folded into historical publications, then `legacy_cleanup_review`
  and `legacy_inflight_import_evidence` dropped; deliveries cursor →
  `runner_history_cursors.deliveries_after` (+ `history_read`, so a row made
  only for that cursor still starts with continuity reconciliation);
  `runner_delivery_cursors`, `runner_admission_poll_schedule`,
  `runner_delivery_explicit_delete_{batches,files}` and the receipt delete
  trigger dropped. Down migration recreates empty tables and keeps the folded
  cohort fenced.

Deleted, each verified first (callers grepped):

- `delivery_cleanup.go` (-1,850): receipt-bound cleanup ledger, explicit
  deletion batches, redundancy proofs; five helpers still used elsewhere moved
  to `delivery_fs.go` (+137). The abandon route, its handler and its web
  button; `queueDeliveryCleanup`; 18 receipt-cleanup DB methods.
- `completed_delete.go` receipt coupling: whole-entry delete and the partial
  sweep refuse publication directories; the storage-review per-file delete
  stays (now the only "review" fence: storage change).
- `retention.go` overlap: Clear / retention / dismissed-row deletion test
  `publications.state != 'removed'` instead of v1 receipts (a publication
  cascades with its row, so such a row is hidden, never deleted); removing a
  publication marks its row's payload removed.
- `season_cleanup.go`: verified **not** a duplicate — it serves only
  non-Runner torrent partials (`delivery_expected=0`, set by
  `season_stall.go`); kept, with the 0040 fence replaced by the
  storage-review and publication fences.
- 0040 review cohort: `LegacyCleanupReview`, `LegacyAutomaticImportReview`,
  in-flight evidence; callers now use `StorageChangeReview` (kept).
- Code the removals left unreferenced: `verifiedDeliveryFile`,
  `replacementHasCurrentCommittedPlacement`, `tryLockStorageDecision`, the
  `explicitDeliveryUnlink` test hook. Left for M6: the dead v1 receipt
  *writers* in `delivery_receipts.go` (M2 left them; tests use them as
  fixtures; the tables stay read-only one more release).

Non-test line delta of `internal/app/acquisition/` since M2's head (6e5fe39):
**+874 / -2,104**. Whole branch, non-test non-generated Go+SQL+TS: +1,389 /
-2,822. Tests: +466 / -1,369 (tests of the removed ledger, batches and 0040
cohort deleted; storage-change and review tests converted to the new fences).

Tests added: `TestCompletedPublicationIsRemovedPerFileWithoutRunner` (c),
`TestPartiallySelectedSeasonPack` (d, incl. the operator discard),
`TestStorageIdentityChangeDeletesNothing` (e), `TestSameSizeDifferentContentIsNotDeleted`
(f), `TestHistoricalCohortWaitsForApproval` (g),
`TestReplacedManifestGenerationDeletesNothing`,
`TestMigration0073RefusesWithLiveDependents`, `TestApprovePublicationCleanupHTTP`,
retention tests rewritten on publications, `queueTime.test.ts`.

Verified (fast lane): `go build ./...`; `go vet ./...`; `go test` of
`internal/app/acquisition`, `internal/infra/sqlite`, `internal/api` (all pass
except the known root-only `TestQueueImportHTTPRecoversFailedImportAfterPayloadPathIsRepaired`);
web `tsc --noEmit`, `vitest run src/queueTime.test.ts src/api.test.ts`;
`make lint`.

Could not verify: `make test` / coverage floor, `make test-web`,
`make test-e2e` (fast lane only, by instruction); no browser check of the
Activity/Storage changes; no run against a real Runner M3 manifest or nuc3
data; migration 0073 not run on a copy of production.

Decisions for Paul:

- **Columns not dropped** (plan M4.3 lists them): `parked_reason`,
  `parked_at`, `superseded`, `delivery_expected`, `request_token_idempotent`,
  `exact_transfer_correlation`. The count check is not only rows: M2 kept live
  code on every one (season planner fences and operator cancellation for all
  clients, the Runner `monarr-delivery` request parameter and native-torrent
  precondition, the submission retry, qBittorrent tag correlation). Dropping
  them is a redesign of those paths, so it needs scheduling, not a migration.
- A manifest entry with no SHA-256 is **kept** (IMPL allows skipping the hash
  with a log line; the M4 brief says never size-only).
- Non-Runner clients keep their whole-payload cleanup (no manifest exists);
  it never touches a directory a publication claims. IMPL section 10 says
  cleanupPublication is the only unlink under a completed root; the other
  remaining ones are this legacy-client path and the operator's Storage
  actions (refused on publication directories).
- Operator discards of a historical publication wait for its review batch
  (consistent with M2's 409 on receipt cleanup).
- The 0040 cohort is folded into the historical cohort rather than freed,
  so dropping its fence frees nothing (contract section 7: no backlog
  mutation).
- An approval endpoint exists for M7 (`approve-cleanup`); no UI for it.
- Docs: plan section 7 names `integration.md` sections 4-5; this repo's
  numbered sections 4-5 are the plurx scan and delivery queue, so the cleanup
  text went into *Pipeline contract v2 in Curator* as a new subsection.

## Pipeline contract v2, M2: Curator reducer, publications, replacement grabs — 2026-11-07 · v0.39.0 (mobile 0.28.0)

**Status:** draft [PR #95](https://github.com/pjunod/curator/pull/95) on
`claude/pipeline-v2-m2-curator` (main merged at c606856) · owner: Claude
(Opus builder) · Fable review r1 (3 blocking, 5 should-fix, 6 minor): all
addressed, re-review accepted them; not merged (after Runner #281), not
deployed. Decision record: [ADR 0026](docs/adr/0026-pipeline-contract-v2.md).

Built (plan M2.1-9, IMPL_CURATOR section 5):

- **Migration 0072**: rebuilds `downloads` with `submitting`, `held`,
  `notified` and the reducer columns (`runner_outcome` + reason/seq/cause/
  blames/salvage/fetched, `provisional`, `deadline_at`, `state_entered_at`,
  `runner_stage`),
  an immutability trigger on a final `runner_outcome`, and deadline triggers
  on insert/state change/progress; new `publications`, `publication_files`,
  `replacement_grabs`, `wantable_attempts` (+ triggers recording the current
  attempt on every insert), `runner_history_cursors`. Data: parked and
  superseded live rows become `failed` provisional (`parked: <reason>`); held
  control rows become `held`; every v1 receipt becomes a `historical`
  publication with the per-file outcomes it reached (the newest receipt wins
  a shared path; each file takes its newest outcome); terminal rows keep a
  `runner_outcome`. Child tables survive the rebuild; a down migration exists.
- **Reducer** (`reducer.go`): facts from SSE, queue, ordered history
  (`since_seq`, cursor per incarnation, owned by the poll), `/deliveries`
  (`include_accepted=true`) and request receipts; transfer-token match
  (mismatch counted once per row/token); a fact with only a handle reaches a
  tokened row only when the last queue poll names that handle as the row's
  token; terminal recorded once with its publication / replacement grab /
  blocklist / history / seam timings in one transaction; a downloading
  deadline moves on progress, a state change or a NEW post-processing stage
  only; provisional rows are in the poll's rows;
  provisional `submit`/`lost` re-opened by any Runner fact; `held (stale)`
  instead of a Curator `hold_expired`; continuity loss reconciles every
  outstanding transfer by token.
- **v1 adapter / publications** (`publications.go`): manifest from
  `.runner-manifest.json`, else the v1 record; per-file outcomes
  (`placed`, `discarded{superseded|duplicate|rejected}`, `unresolved`);
  precedence by `wantable_attempts`; outbox rows written in the completing
  transaction; manual approval honoured; Resolve (Place/Discard/Keep) via
  `POST /queue/{id}/publications/{publicationId}/files/resolve`; historical
  cohort frozen once at first start.
- **Failure is the next grab**: `replacement_grabs` worker (5 s + kick):
  movie/book search, episode single-release search (never a pack), season
  planner only for episodes with no single release; only `blames_release`
  blocklists and spends bytes; `failure_to_next_grab` starts with the
  failure and ends when the worker submits. `fetch{indexer_unavailable}`
  writes no grab and defers the source indexer (its `retry_at`, at least 15
  min) with the same time as the row's `provider_retry_after`. Legacy
  clients' failures use the same worker.
- nzbd adapter: failed maps to `failed`; facts on every event; history by
  `(incarnation, since_seq)`.
- `pipeline:overdue` judges `deadline_at` (health.go comment, warning text,
  usage.md wording updated per the M0 review hand-off).
- Activity (web): Held by Runner section, stale/provisional lines,
  publication file list with Resolve (styled; the result is shown inline,
  on screen, and takes focus); e2e `pipeline-v2-activity.spec.ts`. Mobile 0.28.0: badge labels for held,
  held (stale), failed (provisional); a hold's message is not shown as an
  error.

Deleted (non-test, non-generated lines, vs the merged M0 head 8c8af17):
3,078 removed, 5,016 added (604 of the added are migration 0072).
Largest: `delivery.go` -1,369 (receipt discovery, lease/verify/ack/import
scheduling, manifest re-verification), `delivery_cleanup.go` -614 (receipt
outcome recording, sidecar staging, automatic cleanup scheduling and retry
worker), `acquisition.go` -476 (`authoritativeRunnerObservation`, custody
waits, admission-poll permits, ack/source locks), `delivery_lifecycle.go`
-124, `runner_admission.go` -100 (admission-poll pacing), `nzbd.go` -70,
`health.go` -63, `replacement_worker.go` -48 (cleanup gate),
`download_control.go` -46 (control refusal matrix). Tests: -9,676 / +1,333
(v1 delivery/receipt/park/admission-pacing tests deleted with their
behaviour).

Verified: `go build ./...`; `go vet` on touched packages; `go test` on
acquisition, api, sqlite, nzbd, notify, pipeline, ports, cmd: all pass except
the pre-existing root-only failure below; `make lint` (0 issues);
web `tsc --noEmit` and `vitest run src/api.test.ts`; mobile `tsc --noEmit`
and `vitest run src/queueState.test.ts src/device-build-script.test.ts`;
the one new e2e spec, `npx playwright test tests/pipeline-v2-activity.spec.ts`
(4 passed: desktop and phone geometry, result in the viewport and focused).

Could not verify: `make test` (and its 86.0 coverage floor, at risk after
deleting ~9.7k test lines; the review's #13 tests were added to protect it),
`make test-web`, the rest of `make test-e2e` were not run (fast lane only,
by instruction); no run against
a real Runner (contract v2 fields are produced by Runner M1+; an older Runner
exercises only the legacy mappings); migration 0072 not run on a copy of
production data; not deployed to nuc3 or devices.
`TestQueueImportHTTPRecoversFailedImportAfterPayloadPathIsRepaired` fails
here because the sandbox runs as root (chmod 0500 does not block writes); it
fails identically on the M0 branch.

Decisions for Paul:

- Legacy `ParkDownload` stays for non-Runner clients only (season planner's
  uncertain-submission fence and the torrent stall watchdog,
  `season_stall.go`); IMPL section 10 lists parks as deleted. **Curator never
  parks or stall-retires a Runner row**: both paths return for Runner clients
  explicitly, and an operator's remove of a Runner row records a final
  `cancelled` terminal (IMPL section 5.5). Columns retire in M4.
- Live publications are never deleted in M2 (per-file cleanup is M4);
  historical ones not until their M7 review. Curator-cleanup on a historical
  receipt is refused (409); Runner source cleanup is refused (409,
  contract section 7).
- Sidecars are left `unresolved` (no automatic sidecar import yet).
- `submitting` is used only on the Runner grab path; `ListActiveDownloads`
  still excludes imported rows.
- Publication imports need the completed root's storage identity (an
  inventory scan) as before.
- Extra columns beyond IMPL: `publication_files.wantable_id`,
  `publications.remote_path`.
- Both seam timings start at terminal receipt (review #5, reversing the
  earlier start-at-first-search): `runner_done_to_import` at a completed
  terminal (at approval for a manual-approval client), `failure_to_next_grab`
  when a replacement grab is written; nothing to grab discards it.
- An indexer outage (`fetch{indexer_unavailable}`) defers the indexer for at
  least 15 minutes when it named no `retry_at` of its own (Runner has
  already exhausted its retries against it).
- **Same-day deploy window with a pre-M1a Runner:** pushed failures carry no
  `fetched` measure (the old `withFetched` guess is gone), so each counts its
  full size against the re-grab bytes budget until Runner M1a is deployed.
- Placements made by a publication import record its Runner generation, so a
  resumed import only reuses its own publication's committed placements.
- Receipt "retry handoff" and Resolve now import the receipt's historical
  publication; a receipt without one (impossible after cutover) is refused.

## Pipeline contract v2, M0: contract, timings, overdue check — 2026-11-07 · v0.38.15

**Status:** [PR #94](https://github.com/pjunod/curator/pull/94) on
`claude/pipeline-v2-m0-curator` · owner: Claude (Opus orchestrator) · two
adversarial reviews addressed (Sonnet agent: 7 findings; Fable: 2 blocking +
4) · not merged, not deployed.

First curator milestone of the pipeline rewrite
([PIPELINE_CONTRACT.md](docs/PIPELINE_CONTRACT.md) is now in the repo).
Measurement only; no behaviour on the download path changes.

- Docs: contract added; "Being replaced by" banner on `integration.md`;
  `plan-integration.md` section 3 and `DOWNLOAD_LIFECYCLE_PROPOSAL.md`
  sections 4.1-4.2 marked superseded; the stale "not fixed or deployed" status
  lines in `RUNNER_FAILURE_DIAGNOSIS.md`, `RUNNER_FAILURE_FIX_PLAN.md` and
  `RUNNER_FAILURE_BUILD_STATUS.md` now say Runner PR #247 fixed it; `usage.md`
  has a "Pipeline" section.
- Migration 0071 `pipeline_timings`; new package `internal/app/pipeline`
  (timings, histograms, `pipeline:overdue`). Recording points on the old
  paths: `failure_to_next_grab` (handleFailure), `runner_done_to_import`
  (started when the import is queued, or at approval for a held row, ended at imported); `import_to_cinema_visible` is not started at all until M5.
- `/metrics`: three histogram families, `monarr_pipeline_waiting`,
  `monarr_pipeline_overdue_rows`, `monarr_runner_fact_mismatch_total`.
- `GET /api/v1/system/pipeline` (`pipelineSummary`) and three tiles on System
  -> Connections.
- Deleted: nothing.

Could not verify: the System tiles in a browser (no component test harness in
this repo; the tile text is a unit-tested pure function); full suites, e2e and
coverage floor were not run (fast lane only, by instruction).

Decisions for Paul: overdue judges `updated_at` against fixed per-state
allowances until M2 adds `deadline_at`. Because `updated_at` is rewritten on
every poll the client still lists the row, the M0 check catches downloads
Curator has stopped hearing about, not jobs held or stalled inside the
client; the code, the warning and `usage.md` say exactly that (Fable review
finding 1, option b — keeps the download path untouched in M0). A failed
metrics query omits that series rather than writing zeros
(`monarr_pipeline_scrape_errors_total` counts it). `failure_to_next_grab`
times the immediate re-search only until M2's reducer ends it from
`replacement_grabs`. Timings only
start when they can end, so none is left open forever: `import_to_cinema_visible`
is NOT started in M0 (deviation from IMPL section 4: M5 starts and ends it; the
tile reads "not measured - needs M5" with no waiting count), a series failure
ends only when a season job was queued (otherwise the timing is dropped), and
`failure_to_next_grab` starts at the first search actually attempted. "Waiting"
counts open timings started in the last 24 h. No pruning: closed rows are about
three per download and pruning would make histogram counts go down. The
`notify.Dispatcher` is not handed the recorder (M5 wires it). Parked and
superseded downloads are never overdue. `/metrics` reads one aggregate per
kind (index on kind, ended_at), and the overdue gauge is a COUNT.

## The re-grab budget counts bytes, not attempts — 2026-11-03 · v0.38.13

**Status:** merged as #90 (`e62d19f`), deployed to nuc3 2026-11-03 00:48Z.

After v0.38.12 went live, Dexter: Resurrection kept re-planning correctly:
each round picked new posts, failures settled in seconds and the next round
excluded them. S01E01–E06 then stopped: every 2160p post tried had failed
health (taken down), and the cap of three failed grabs per episode per 12 h
held them until the window rolled. Paul chose "count bytes, not attempts".

- Runner's history record carries `success_articles` / `total_articles`;
  the nzbd adapter now reports that fraction as a failed row's progress
  (it reported 1 for every finished row, failed or not).
- `regrabCapped` sums the share of a release each failed grab fetched and
  caps at three releases' worth; a size-less row counts in full. A health
  failure with zero articles costs nothing.
- The season failure path now records `regrab_capped` in history when an
  episode is capped; it used to stop silently.
- Review (Sonnet): fixed — a failure settled from the Runner receipt alone
  (history aged out) carried progress 0 and would have counted a 100 GB unpack
  failure as free; unmeasured failures now count in full via
  `DownloadStatus.FetchedKnown`. A pushed `job_pp_finished` failure carries no
  article counts, so the adapter completes it from the history row. A ceiling
  of 20 failures per window bounds zero-cost retries (each still spends an
  indexer grab). `regrab_capped` is recorded once, on the failure that crosses
  the line.

Validation: `go test ./...` locally green except
`TestQueueImportHTTPRecoversFailedImportAfterPayloadPathIsRepaired`, which
fails only as root (chmod 0500 doesn't bind root). CI on main `e62d19f`:
Lint, Docker, Compat and Tests (End-to-end, Mobile, Go race shards 0–7)
green. The
branch's own PR runs were cancelled after merge; main ran the identical tree
(`4033dd0`).

Live outcome:

- The 32 failed Dexter rows stored before the deploy carried progress 1 under
  the old adapter. Runner's history showed `success_articles=0` for every one,
  so only those rows were set to progress 0 (DB backed up first to
  `~/work/claude-runner-failure-20261102/db/pre-progress-fix.db` on nuc3).
- Rounds 3–8 (plans 127–133) ran unattended: about forty-five more 2160p S01
  posts failed health with zero articles, each blocklisted within seconds at
  zero budget cost. By 02:10Z S01E03, E05, E06, E07 and E08 had imported, E09
  was downloading, and the loop was still working E01, E02, E04 and E10.
  S02E01 imported earlier.

Cleanup: remote branches for Runner #266, Curator #89/#90 and ansible #27
deleted; cloud clones removed.

Open follow-up: `settleSeasonPlan`'s `INSERT OR IGNORE` successor is dropped
while the season job is running; the job's own retry covers it today.

## Failed downloads no longer freeze behind Runner — 2026-11-02 · v0.38.12

**Status:** merged as #89 (`a9b0bdb`) with the paired Runner change
(pjunod/runner #266); both deployed to nuc3 2026-11-02.

Field report (Paul, 2026-11-02): "dexter resurrection already failed all of
the initially tried downloads, but curator is just sitting there like it
doesn't know. I would expect it to instantly know the status of the
downloads is failed, and to continue queueing up more attempts from the
searches" — and "interconnection is a first class citizen between the two
apps. time to act like it".

Root cause, verified on nuc3: all 11 jobs ended FAILURE/HEALTH with empty
`/processing` directories. Runner classified "no files to assess" as
`unknown`, retained the empty directory "for recovery" as `runner_owned`, and
never pinned that source into the request receipt. Curator correctly holds a
replacement while Runner owns failed bytes, so the hold could never settle,
not even if the operator deleted the files in Runner.

- Runner: an attempt with no source files is unusable and disposed; the
  file-lifecycle tick disposes any retained failed source whose manifest is
  empty; retained failures pin `Artifact:Id` + `Artifact:Generation`; old
  receipts adopt their job's allocation on the next lookup, so the 11 live
  rows settle on deploy with no click.
- Curator: a held terminal outcome now retires the in-flight stage, so held
  failures leave **Downloading** (they showed an empty bar under it).
- Deployed Runner (`0a4f172`) at 22:20Z: all eleven rows settled to failed
  and were blocklisted within ten minutes, but nothing re-queued. Two more
  Curator causes, found live:
  - The season planner filtered candidates by the title/indexer blocklist
    only. Failures are blocklisted by release identity (indexer + GUID), so
    the planner re-chose the identical NZB (download 586 = 585) ten seconds
    after it failed; dispatch refused it and the plan settled `needs_replan`
    with its successor dropped (the running job held the dedupe key).
    `skipCandidateFor` now applies both blocklists wherever the title is known.
  - A failed grab's season re-search merged into the S01 comparison parked for
    its next discovery epoch (24 h) and stayed parked; the week-long cooldown
    could also swallow it. A failure now bumps `replanGeneration`: a parked
    job is made due, a running one reruns when it finishes, the checkpoint
    write preserves the bump, and the cooldown does not apply.
  - Left as a follow-up: settlement's `INSERT OR IGNORE` successor is still
    dropped while the job that settles is running. Promoting it there could
    loop on a persistent dispatch refusal (e.g. a disabled client); with the
    planner and dispatch now agreeing on the blocklist it no longer fires for
    this case.
- Review (Fable) of the replan change: no loop or indexer hammering
  (N failures ⇒ at most N reruns; deferrals refund attempts; budgets and
  RetryAt are rechecked). Fixed: a rerun now starts from a fresh checkpoint
  (carrying `Started` would park it behind its own plan and hold the single
  comparison-admission slot for hours); `DeferClaimedJob` no longer parks a
  replan requested after the handler's last check; a request that lands after
  the comparison finished queues itself; RSS refuses an identity-blocklisted
  post once the title is matched.

## Lifecycle follow-ups after #83 — 2026-11-02 · v0.38.11

**Status:** [PR #88](https://github.com/pjunod/curator/pull/88), ready, on
`main` after #87 (owner: Claude for Paul). Review addressed; full local gate
green (`make lint` 0 · `make test` 53 packages ok · `make test-web` 133 ·
`make test-e2e` 212). CI, merge and deploy to nuc3 next.

#83 repaired main's CI differently from the closed #86. Four of #86's fixes
were still needed; each was confirmed by a test that fails on main first.

- Legacy cleanup review fences per file: the most specific record holding a
  file decides, so an ordinary sibling download is not deleted through a
  legacy fence.
- Delivery cleanup distinguishes a library it cannot read (retry, with "library
  placement could not be read; cleanup will retry") from a placement that
  changed (protected, as before). Size and generation checks run before hashing.
- nzbd delivery adapter keeps the HTTP status and Retry-After when an error
  body cannot be read, closed, or is oversized; a transport failure while
  reading a page no longer triggers the single-row retry.
- The phone tab bar no longer covers lifecycle action buttons or their results
  (`web/src/viewportChrome.ts`; e2e checks `clearOfShell` on phone).

**Review (independent agent):**
- Fixed (confirmed by probe): a deleted title or season folder was judged
  "unreadable" because its own parent was missing, so cleanup retried forever
  under a message blaming storage. Unreadable is now judged against the
  registered library root: a readable root with the folder gone is a
  definitive loss (`protected`); a missing, unreadable or empty root (an
  unmounted mount point) retries. The exact-copy fallback now runs before the
  retry decision.
- Fixed: sidecars get the same transient/definitive split (database, digest
  and storage read failures retry).
- Fixed: a page too slow to read within the client timeout still falls back
  to the single-row retry, so discovery keeps advancing.
- Fixed: an unreadable error body is joined to the status error rather than
  flattened, so a cancellation stays visible to `errors.Is`.
- Accepted: the per-file fence recheck reloads records before each unlink,
  like the existing `rejectReferencedPayload`; freshness is the point.
  Selections are small. A one-off double scroll reservation on first mount
  of a notice is cosmetic.

Decision for Paul to look over: a changed library placement stays `protected`
(#82's choice); only unreadable library storage retries.

## Recovery takes only its own title — 2026-11-02 · v0.38.10

**Status:** merged ([PR #87](https://github.com/pjunod/curator/pull/87),
fb24a61) and deployed to nuc3 2026-11-02: `/api/v1/system/status` reports
0.38.10, commit fb24a61, schema 70. No recovery rows existed, so none returned
to review. Rebased onto main after #83, which took migration 0069 and 0.38.9.

Asked by Paul: "fix the recovery scope". A Runner recovery import was leased
against `lifecycle_library_revision`, which every title's edit advances, so a
recovery held every title's import authority and any other title's commit
voided its preview: it could only run while the whole library was quiet.

- Migration 0070 keeps `lifecycle_item_revision` (per media item: the item,
  its copies, seasons, episodes, files and file-episode links; moves count for
  both titles) and `lifecycle_shared_revision` (quality profiles, root
  folders, custom formats, download clients).
- A recovery accepts both for its target title; the placement transaction
  checks and advances them. It takes only its own title's import authority,
  so other titles import and publish while it runs.
- Waiting for its own title polls without queuing and fails at once if that
  title changes. A cancellation on a busy title stays pending for the next
  sweep instead of holding the sweep.
- A recovery placement refuses a destination file row owned by another title.
- Upgrade: queued/importing recoveries accepted under the old revision go back
  to review; the shared revision starts at 1 so a never-accepted lease is never
  current. `lifecycle_library_revision` is unchanged for acquisition plans.

**Review (independent agent) — fixed:** legacy rows would have run unleased
after upgrade (probe-confirmed); cancellation ack blocked the whole sweep on a
busy title; file-episode links now also advance the file's title; download
clients added to the shared revision; foreign destination rows refused.
**Noted:** a recovery waiting on its title can lose the race to a steady
stream of same-title imports (each commit voids it, so it fails rather than
waits forever).

**Validation:** before the rebase onto #83: `make lint` 0 · `make test` 50/50
(non-root) · `make test-web` 131 · `make test-e2e` 208. After the rebase: see
below.

## Samples, settled retries, identical bytes, storage stalls, per-title lock — 2026-11-01 · v0.38.7

**Status:** done. [PR #80](https://github.com/pjunod/curator/pull/80)
merged as `2c367c5`, tagged `v0.38.7`, and deployed to nuc3. Five
adversarial reviews were addressed (below).

**Validation.** The full gate was green on the tree before the final-review
fixes (lint 0 · `make test` all packages, as non-root · web 124 · e2e 192).
The final-review fixes invalidated lint, the acquisition and api packages and
e2e; all four were rerun green on the final tree. All seven CI checks were
green on `19c4418`.

**Main went red under #79.** iOS build 33 for TestFlight left the Android
`versionCode` at 31, and the mobile test keeps the two equal. #80 merged main
and set `versionCode` to 33. The branch was rebased onto main after #77 and
#78 took 0.38.6 (no code overlap), which is why this is 0.38.7.

**Deploy.** Ansible's `media/deploy.yml -e sync=false -e only=monarr --limit
nuc3`, run on nuc3 itself with `-c local` under tmux. Its result line:
`nuc3/monarr: redeployed at v0.38.7 (/opt/noirr/curator/deploy)`.
`/api/v1/system/status` reports 0.38.7 @ `2c367c5`, schema 62. The
inventory still pointed at `/opt/noirr/monarr`, a path nuc3 no longer has, so
a monarr deploy would have stopped at "missing checkout".
[ansible #24](https://github.com/pjunod/ansible/pull/24) (merged) points it at
`/opt/noirr/curator`. The repo's `media deploy` workflow cannot be used yet:
`MEDIA_DEPLOY_SSH_KEY` is not set on pjunod/ansible.

**Live check after the deploy (02:00Z):**

| Check | Result |
|---|---|
| Storage link | 333–1,085 Mbit/s receive during imports, against 2.4 Gbit/s saturated on 0.38.6. No NFS "not responding" since the deploy; the last stall was 23:17–23:51Z. |
| Maschile Plurale, Quem Ama Cuida | Imported: receipt 158 and the S01E124–E140 receipts. |
| Sample deliveries | Good.Trouble and wtFOCK episodes importing one after another: 144 → 152 receipts imported in the first 16 minutes, and the rest are draining on the 5-minute lease. |
| Good.Trouble S02E06 AVON copies | All six payloads `removed`; five of them removed in 02:03–02:05Z. |
| Settled failures | Just Friends (2018) WEBRip (does not improve on the WEB-DL) and Them S01E02 (cut short) are on the long backoff, with one warning each. |
| The Craft (1996) 2160p REMUX | 68 GB placement running on one worker; the other worker keeps importing, so other titles no longer wait behind it. |

**For Paul:**
- `gha-m6-monarr-01` is offline, so `gha-nynuc-monarr-01` runs every Curator
  job one after another. #80's seven checks waited about an hour behind #79's.
- The macOS runner inventory in pjunod/ansible still pins Xcode 26.6 (label
  `xcode-26`). Runners for that version no longer exist; Apple jobs need 27.2.
- Commit identity is still his name plus Co-Authored-By and Claude-Session
  trailers, as he asked. A session hook asks for Claude <noreply@anthropic.com>
  instead. His call.

Measuring v0.38.5 on nuc3 turned up a second permanent loop. Every Runner
delivery that shipped a scene sample (`sample-<release>.mkv`) failed on every
attempt with "selected file is not importable: …/sample-…mkv": the automatic
delivery selection offered every video in the manifest, samples included, to
the validator for explicit selections, which refuses samples and fails the
whole import. The completed-folder scan has always skipped samples, so only
Runner deliveries hit it. Live damage: Good.Trouble S02E09 and S02E10 grabbed
six times each (up to 239 attempts per copy), wtFOCK S02E03, S02E10, S03E01,
S03E02, S03E05 and S03E06 three or four times each (up to 1,206 attempts since
Oct 27). No new grabs of either show since v0.38.4's re-grab guard.

- One `isReleaseSample` predicate now serves the completed scan, the manual
  browser, the delivery selection (automatic and Resolve), the selection
  validator and the redundancy proof's media count. It also recognizes files
  inside a release's `Sample/` folder.
- A sample is recorded as `known_incidental` ("release sample excluded from
  media selection"); rows an earlier attempt left as "not attempted by the
  importer" are corrected on the next attempt. Still protected either way.
- Resolve's select-all (sample included) imports the episode instead of
  rejecting the batch.
- A manifest with no importable media (only a sample, or only sidecars) is a
  failed import. Before, it skipped the importer and the delivery was marked
  imported with zero files. Runner's acknowledgement is unchanged (it is sent
  before the importer runs; the manifest itself was valid).

**The amplifier.** Every failed delivery import retried every 5 minutes
forever, re-reading the whole payload and writing an `import_failed` history
row and a notification each time — 742 rows in 11 hours for these two shows.
That turned all three of today's defects into 1,000-attempt loops.

- A settled failure (the importer's verdict: `ErrNoFilesImported`, with no
  storage block, ledger or proof error joined in) now waits about as long as
  the delivery has been blocked, from 5 minutes up to 1 hour. Elapsed time,
  not the attempt count, sets the pace (acknowledgement retries also count
  attempts). Environmental failures keep 5 minutes, and so does any receipt
  whose Runner acknowledgement is still pending.
- An automatic retry that ends in exactly the failure last recorded in the
  download's trace returns it to failed without a new history row or
  notification.
- A Resolve re-import of an imported delivery resets attempts and blocked
  time; **Retry** retries at once and keeps them (the pace is elapsed-time
  based, so the count no longer matters).
- Not changed: the scheduler's lease write still writes its snapshot of the
  retry state, because that write is also how a freshly resolved association
  (cleared error, counted attempt) is persisted.

**Review** (adversarial agent): no deletion-safety findings. Addressed: the
zero-media guard created its own forever loop and skipped the ledger (now it
falls through the shared failure path, and the loop is fixed generally, above);
comments and docs claimed it withheld Runner's acknowledgement (corrected;
tests assert the ack); Resolve select-all with the sample failed; stale sample
rows were not reclassified; `Sample/` folders were missed.
**Second review** (Sonnet, on the retry change): no blockers. Addressed: the
repeat check used the receipt's error, which acknowledgement and verification
failures overwrite (duplicates returned during a Runner outage; now it reads
the download's trace); ack failures doubled the backoff (now elapsed-time
based); joined ledger/proof errors counted as settled; a pending ack waited on
the import's backoff; attempts never reset on re-import. The lease write's
stale snapshot was left (see above). The 6-hour ceiling and the restart-time reset were
dropped for a 1-hour ceiling: "settled" includes verdicts that clear on their
own (an episode not yet in metadata), and an hour bounds that without a reset
that re-hashes every stuck payload on each restart.
**Decision for Paul:** the name test stays a substring match, so a real episode
whose title contains "sample" is skipped. Narrowing it to a delimited token
risks placing an undelimited sample clip as the episode, which would then
block the real file as "does not improve on" it. Kept the conservative rule.

**Storage stalls on nuc3 (diagnosed 2026-11-01, Paul asked).** The NAS (QNAP,
NFSv4, `hard` mounts) stopped answering nuc3 six times today: 13:28 (27 s),
15:37 (3 min), 17:58 (8 min), 19:09 (9 min), 21:44 (6 min), plus once on
Oct 30. Each time everything on the mount froze — Curator's imports and
cleanups, and Runner, whose held-jobs recovery (#261) parked the affected jobs
as `storage_unavailable` and resumed them correctly.

- Cause, from Prometheus: nuc3's storage link is a 2.5 GbE USB adapter
  (RTL8156B, `enx6c1ff71662f6`). It ran receive-saturated at 2.2–2.44 Gbit/s
  for hours, and every outage began as a write phase started on top of that
  (transmit rising to 0.2–0.5 Gbit/s). No NIC errors or drops, no NFS
  retransmits, QNAP CPU never above 46%: the NAS's storage, not the network,
  gives out under saturating reads plus writes.
- Who was reading: Curator, at 267 MB/s — the whole link. A goroutine dump
  (taken with Paul's OK; it restarted Curator) and its open files showed three
  redundancy proofs hashing the same 2.9 GB library file at once, and a manual
  placement hashing its 18.6 GB target while holding `importTargetMu`, which
  every other import and cleanup was waiting on. Underneath it all, the
  forever-retry loops re-read whole payloads every few minutes.
- No deadlock: the dump showed every lock waiter behind that one placement,
  and the scheduler loops idle between ticks.

Fixed here:

- One verification read budget for the process (`MONARR_VERIFY_READ_MBPS`,
  default 100 MB/s, `0` = unlimited) paces every content digest — delivery
  verification and recheck, redundancy proofs, placement checks, cleanup.
  Placement copies are not paced.
- Concurrent digests of one unchanged file share a single read (keyed by
  file generation; not a cache — nothing outlives the read).
- The settled-failure backoff (above) removes the bulk of the re-reads.
- Attempts left "running" by a previous process are marked `interrupted` at
  start (no finish time invented, no outcome inferred).

**One title no longer holds every title** (Paul: "fix the rest"). At 23:25Z
Maschile and Quem Ama Cuida had finished in Runner and sat queued behind The
Craft 1996 (68 GB remux, placing at 10.7 MB/s, about two hours), because
`importTargetMu` was one process-wide mutex held for an import's whole run.
It is now `importTargets`: a global RWMutex plus one mutex per media item.

- Per title (global shared + the item's lock): imports, grab and
  manual-import admission, season admission, replacement, sidecar
  publication, receipt cleanup and explicit delivery deletes, partial
  payload settlement, reservation resolution. Different titles proceed
  concurrently; one title is serialized as before.
- Whole library (exclusive): recovery (its preview is leased against the
  library-wide revision, so another title's commit would invalidate it),
  path-addressed legacy deletes whose owners are not known, and the
  placement journal's reconciliation at start.
- Exclusive acquisition never queues: a waiting `sync.RWMutex` writer blocks
  new readers, so one queued request would stop every title behind the
  slowest placement. It polls `TryLock` with backoff; operator requests wait
  at most 15 s and report "imports are publishing; try again shortly"; a
  whole-library request while holding a title is refused, not deadlocked.
- The storage-decision lock still serializes the short publish-and-commit
  window. The storage link (a USB 2.5 GbE adapter to the NAS) remains the
  hardware limit.

**Fourth review** (full model, on the per-title lock and review-three
fixes). Three blockers, all probe-confirmed, all fixed:

- Recovery was invalidated by any other title's commit (library-wide
  revision lease): recovery takes the whole library again, as it always did.
- One queued exclusive request (the hourly partial-payload sweep, an
  explicit-delete retry, reservation resolution, an operator delete) brought
  back the original stall: sweep and retry paths now lock the owning title,
  and exclusive acquisition never queues (above). Tests: a waiting
  whole-library request does not hold up another title (fails with a queued
  `Lock`).
- The payload guard missed bind-mount aliases (pre-existing hole): it now
  compares filesystem identity with the publication (`os.SameFile`), not
  path text. That also fixes the medium regression where a library inside a
  completed root stopped all cleanup — only the publication counts now.

**Final review** (full model, on the fourth-review fixes): no deadlocks.
Fixed: a library folder that is a symlink to (or bind mount of) a subfolder
of the publication passed the guard — any symlinked folder on an evidence
path is now refused, as placement refuses it, and a library file that is
the payload file's own directory entry is refused. Recovery's wait for the
whole library starved under steady imports and then failed its
library-wide revision check anyway; it now waits without queuing, shows
"Waiting for imports in progress to finish" on the recovery, and fails at
once ("library changed while recovery waited…; preview again") the moment
another title commits. The hourly partial-payload sweep tries the whole
library for 250 ms, not 15 s, and logs a deferral at Info. A queued job
picks up its download's current title under the download lock; resolved
reservations and partial payloads re-check the title after locking; a
released lock's context can no longer pass as held.
**Decision for Paul:** recovery still needs a quiet library because its
preview is leased against one library-wide revision. Leasing it against the
target title's own revision would let recovery take only its title — a
migration and a change to the recovery lease, so not done here.

Also fixed: a foreground digest no longer joins a background read; an
import's own pre-lock verification is foreground; a re-read download that
names another title is refused under the old title's lock; releases are
idempotent and deferred, so a panic cannot leak a title lock; replace-bad
marks count only while the operation is live. Noted, not changed: two
different-title placements can now overlap on one volume, and the capacity
check is not a reservation — a simultaneous ENOSPC converges through the
storage retry; season admission's library-wide revision check fails (and
retries) somewhat more often under concurrency.

v0.38.5 measured live on nuc3 (started 21:30:10Z): two imports in flight at
once, and 2.9 GB duplicate cleanups running 225–275 s each on the cleanup
workers alongside them. The HIG S05E04/E06 duplicates are draining as
redundant.

**Identical bytes are redundant** (approved by Paul). Good.Trouble S02E06 AVON
is a 66 MB, one-minute fake that was imported six times, each copy placed over
the last. The five superseded payloads sat `placed`/`retry` ("verified library
placement is no longer present"), retried hourly, because the surviving
library copy is flagged implausible and the proof required an unflagged file.

- A library file in scope (same item, copy and episodes, same format) holding
  exactly the payload's bytes — digest bound to one stable file generation —
  now proves the payload redundant whatever that file's standing: unverified
  quality, an open review flag, an implausible runtime, a profile that would
  reject it. Deleting the payload loses nothing the library does not hold.
- A `placed` row whose placement was superseded falls back to that proof.
- Kept out: a file selected by an in-flight replace-bad operation (that
  operation owns it and the payloads answering it), and a different extension
  (identical bytes delivered as .epub over a library .pdf are a format change).
- Tests that asserted the old rule (review flag, copy profile) now expect
  redundancy; a new test covers the superseded placement (fails before with
  the production error) and refuses one whose placement now holds different
  bytes.

**Third review** (full model, on identical bytes and read pacing). No blocker
in the rule as designed. Addressed:

- High, probe-confirmed: a library row aliasing the payload's own path made
  the payload its own proof, and cleanup deleted the only copy. Evidence
  inside the receipt's publication or any completed root is never library
  evidence, at proof time and again under the lock (test fails without it).
- High: the read budget starved the import-lock holder — background proofs
  drew from the same budget while every other import waited on the lock.
  Reads made while holding a title, or for an operator's request, now have
  their own budget, and background reads slow to a tenth while one is
  active. With the per-title lock the blast radius is one title anyway.
- The limiter's deadline refusal now reads as `context.DeadlineExceeded`, so
  a waiter re-reads rather than inheriting the leader's error; a panicking
  leader releases its waiters; an invalid budget value is logged.
- The lock-time recheck refuses evidence a replace-bad selection made since
  the proof now owns; `redundant` rows whose recorded library file was
  replaced fall back to identical bytes in scope, as `placed` rows do; the
  redundancy reason no longer claims a quality verdict for identical bytes.
- Operator **Delete files…** requests verify on the foreground budget.

Not changed, deliberately: reusing a placement's target digest across its
checks would reintroduce the generation-keyed memo rejected in #76 (mmap
writes change bytes without changing mtime/ctime); the per-title lock bounds
the cost to the title being upgraded. Two processes sharing one database
would mark each other's live attempts `interrupted` — Curator is single
instance. The lease write's snapshot is unchanged (above).

## Imports no longer wait behind cleanup — 2026-11-01 · v0.38.5

**Status:** [PR #76](https://github.com/pjunod/curator/pull/76) — two reviews
addressed (memo rejected; comment and cleanup worker count fixed). Full gate
once: lint 0 · go test green (the chmod-based
`TestQueueImportHTTPRecoversFailedImportAfterPayloadPathIsRepaired` fails only
as root; rerun alone as non-root on the final commit: pass) · web 124 ·
e2e 192. CI green, merged (990e768), deployed to nuc3 by Paul.

Paul asked why importing is slow and whether it must be serial. Measured on
nuc3 after v0.38.4: a real 2.5 GB import took 31 s; proving a 2.9 GB
duplicate took 250–280 s; and attempts ran strictly one after another.

- **Serial:** imports have two workers by design, but payload cleanup shared
  them. A 31 GB cleanup re-reading both copies held one worker for 20+ min,
  leaving one for every import. Cleanup now has its own two workers.
  Placement itself stays serialized by `importTargetMu`, one process-wide
  destination lock that recovery, replacement, deletion and season cleanup
  also take; narrowing it per item is a separate decision for Paul (large
  crash-safety surface, limited gain while copies share one mount).
- **Re-reads:** a retry of an accepted delivery re-read its payload twice
  (verify, then a recheck moments later with nothing in between). The recheck
  now runs only when an acknowledgement round trip separates it from the
  verification.
- **Rejected after review:** remembering digests by file generation (inode,
  size, mtime, ctime). mmap writes change bytes without changing mtime/ctime
  (reproduced on ext4), and the racy-margin rule mixed the file server's
  clock with Curator's. A wrong digest feeds a cleanup that deletes, so every
  proof that can lead to deletion keeps reading the bytes.

## Obfuscated duplicates are redundant — 2026-11-01 · v0.38.4

**Status:** [PR #75](https://github.com/pjunod/curator/pull/75) — adversarial
review addressed (one blocker: a borrowed episode reached the different-bytes
branch, which could mark a different file redundant and delete it — the borrow
now accepts exact bytes only; the sole-file count now requires both manifest
and disk to agree, as the importer counts disk). Full gate once on the final
tree: lint 0 · go test green (the chmod-based
`TestQueueImportHTTPRecoversFailedImportAfterPayloadPathIsRepaired` fails only
as root; rerun alone as non-root: pass) · web 124 · e2e 192. CI green,
merged; deployed to nuc3 by Paul.

After v0.38.3 reached nuc3 both stuck episodes imported within minutes
(`episode_from_release`), but the other twelve byte-identical copies of each
stayed `unknown_recoverable`, retried every few minutes with a fresh
`import_failed` row ("episode N is protected by current policy"), and were
never cleaned up. The redundancy proof read the duplicate's episode from its
file name — the same defect v0.38.3 fixed in the importer — so a hash-named
copy could never be proven redundant. The proof now borrows the grabbed
release's episodes under the importer's exact conditions (sole media file in
the manifest, release covers the grabbed episode); everything after that —
exact bytes against a verified, unflagged library file — is unchanged.

## Obfuscated imports and the re-grab loop — 2026-11-01 · v0.38.3

**Status:** [PR #74](https://github.com/pjunod/curator/pull/74) — review addressed,
local gate green, CI green, merged; deployed to nuc3 by Paul (the earlier
"no SSH key" note was wrong — nuc3 uses the same key as every node).

Field report: two episodes sat in Runner's "Fetching NZB" for most of a day
behind a 22-hour indexer `Retry-After`. The quota had been spent by Curator,
which grabbed the same two obfuscated NZBs thirteen times each since
2026-10-27. Runner delivered and Curator accepted all 26; every import failed
with `cannot determine episodes from "321d96….mkv"`.

- **Root cause:** episode identity came only from the payload file's basename.
  A sole anonymous file now borrows the grabbed release's episodes when the
  whole payload has one media file and the release covers the grabbed episode
  (`episode_from_release` history event).
- **Loop:** a failed import released the want and the identical release was
  grabbed again. Candidate selection now passes over a release whose bytes are
  retained after a failed import (not cut short, still on disk, not removed
  from Activity); `Grab` refuses it as a backstop.
- **Guard bypass:** RSS now applies the regrab cap.

**Review:** adversarial agent — no blockers; five should-fix findings
(manual-subset mis-assignment, an unclearable block, docs, no Runner-path
test, a per-candidate scan) fixed in the second commit.
**Validation:** `make lint` 0 issues; `make test` green (one test,
`TestQueueImportHTTPRecoversFailedImportAfterPayloadPathIsRepaired`, depends on
`chmod 0500` and fails only when run as root — rerun alone as a non-root user:
pass); `make test-web` green; `make test-e2e` 192/192 after installing the pinned
Playwright browser (the first run never launched a browser).
**Decision for later review:** the block is keyed by release title, not by
item copy; a file name with episodes but no season is not completed from the
release. Both conservative.

## Zero request cap means unlimited — 2026-10-22 · v0.37.2

**Status:** implementation, adversarial review and local validation complete.
[PR #69](https://github.com/pjunod/curator/pull/69) is open against `main`;
GitHub checks and merge pending.

A zero indexer cap now disables Curator's local request allowance for manual,
automatic and RSS requests. Positive caps retain their existing allocation.
Season forecasts preserve request duration without adding quota waits for an
unlimited indexer. Provider retry delays, 15-minute RSS pacing and usage
accounting remain active, including across cap changes.

Regression coverage includes requests beyond the former defaults, switching
between zero and positive caps, provider delays, RSS pacing, season forecasts,
and saving zero through the API and the desktop/phone settings dialog.
The settings reference and original acquisition plan record the new meaning.

**Validation:** `make lint` and `make test` passed with Go 1.25.7;
`make test-web` passed 123 tests and `make test-e2e` passed 162 tests with
Node 22.13.0. The first GitHub run passed Go race/coverage, web, browser,
lint and Docker checks, but Expo doctor rejected four pre-existing outdated
SDK patch versions. Updated those four Expo ranges and their lockfile; the
additional diff passed independent review. Mobile typechecks, all 107 tests,
all 20 doctor checks and both iOS/Android exports passed locally. GitHub checks
on the final dependency correction are pending.
**Review:** one cap-transition finding resolved. Prior unlimited manual usage
now reduces the combined search pool and its forecast when a positive cap is
restored, preserving RSS capacity. Independent re-review found no remaining
actionable issues. **Cleanup:** isolated clone retained until merge.


## Manual searches are charged as manual everywhere — 2026-10-04 · v0.36.5

**Cause, measured on nuc3:** each twelve-hourly backlog pass plus the season
comparisons spend the whole automatic share (50 requests per indexer per
twelve hours). That rationing is working as designed. The defect was
attribution: searches a person asked for were booked against the same share,
so every button was a no-op for hours after each pass.

**Fix:** who asked is a property of the request and travels with it.

| Path | Before | Now |
|---|---|---|
| Auto search, movie or book (0.36.4) | automatic share | manual allowance |
| Auto search, series | automatic share; silently dropped inside the week-long cooldown or when already queued; queued behind any parked comparison | manual flag in the job payload; promotes a parked or running job; admitted past unattended comparisons; forecast against the manual pool; only a repeat within ten minutes is declined, and says so |
| Wanted run, one title or target | automatic share | manual allowance |
| Wanted run, everything or a whole reason | automatic share | unchanged on purpose: it is the backlog on demand |
| Wanted target no indexer could be searched for | "searched, 0 seen" | skipped as `indexers_unavailable`, reason attached |
| Curator mobile Auto search | "Search started." always | the same sentence the web shows |

Auto search results name each indexer that could not be searched and why
(`incomplete`, 0.36.4).

**Decisions taken without Paul, for review:** bulk Wanted runs stay on the
automatic share so one press cannot spend the reserve every other deliberate
search depends on; a manual comparison's self-scheduled reruns go back to
unattended; the manual repeat cooldown is ten minutes.

**Not done:** the mobile build has to be installed on the physical devices.
Mobile "Search all now" still runs the backlog task and now says it is
unattended work; it does not show per-target results as the web does.

## Compact TV quality details — 2026-09-29

**Implementation and adversarial review complete.** TV file measurements
collapse into a counted, scrollable disclosure. The quality badge and
upgrade status stay visible. [Delivery status](docs/TV_QUALITY_STATUS.md)
tracks review, final validation, and merge.

## Series monitoring — 2026-09-29

**Building:** persistent future monitoring and automatic episode search.
[Delivery status](docs/SERIES_MONITORING_STATUS.md) tracks the isolated clone,
pre-merge review, final checks, and PR.

## Completed-folder accounting — 2026-09-29

**Building:** durable accounting for the reported 804 GB of forgotten completed
downloads. [Live delivery status](docs/COMPLETED_FOLDER_STATUS.md) tracks the
implementation, adversarial review, final gates, and merge.

> **Snapshot 2026-09-28 · v0.31.0 · Audio language is a profile criterion
> (ADR 0022): a profile can require English (or any set of languages), a
> file measured to carry none of them keeps the item wanted at any quality,
> a release offering the language replaces it even at a lower quality, and
> releases in other languages are refused. The *Just Friends* case.**
>
> Previously: v0.30.0 · Failed plurx deliveries can be retried from the
> delivery log (the import-notify wedge).
>
> Previously: v0.28.1 · One folder, one item (ADR 0020) · v0.28.0 · The
> native app gained the web's Interactive search.
>
> Previously: v0.24.1 · Curator hands Cinema exact book work and edition
> metadata when an ebook or audiobook import lands (§Phase 2.5 — Books).
>
> Previously: v0.23.0 added audiobook curation · v0.20.0 rebuilt the calendar ·
> v0.19.1 made Access a first-class web tab · v0.19.0 added configurable
> download-priority policies. Full gate green: lint · Go · 76 web unit tests ·
> 52 native unit tests · web build · 101/101 browser tests.
>
> This is the explicit work ledger: every deliverable we've committed to, and whether it is
> done. Checked = shipped and verified, not "mostly there". Update at the end of every
> working session. Design rationale: [docs/architecture.md](docs/architecture.md) ·
> decisions: [docs/adr/](docs/adr/) · this file: state only.


## Recovery import page rework — 2026-09-28

**Status:** shipped (0.32.0). Field report: Settings → Recovery was a stack
of unlabeled selects; a handoff whose staging had *failed* in Runner was
offered in the dropdown with its error printed as an anonymous alert, and
Preview died on `lstat /recovery: no such file or directory` because the
mount was never checked before the click. The page is now a three-step flow
(handoff table → searchable title, copy only when there is a choice, series
episode mapping → preview and import) under a readiness strip (Runner ·
recovery mount · credential); the Preview button names the missing
precondition; import activity is a table with progress bars. Handoffs carry
`source`, `consumer`, `created_at`, `updated_at` from Runner (runner PR #243).
Gate: lint 0 issues · `go test ./...` · vitest 119 (+15, `recovery.test.ts`)
· Playwright 117 (recovery spec extended). Verified with mocked handoffs in
Chromium.

## Delivery — Audio language requirement

**Status:** merged `85f66df` · tagged v0.31.0 · deployed to nuc3 · **Updated:** 2026-09-28 ·
**Version:** 0.31.0 · PR [#46](https://github.com/pjunod/curator/pull/46) · ADR [0022](docs/adr/0022-audio-language.md)

| Item | State | Evidence |
|---|---|---|
| Independent clone | complete | Cloud clone of GitHub `main` @ `b71e8c6` (v0.30.0); user checkout (at 0.24.4, dirty) untouched. |
| Vocabulary | complete | `internal/domain/language`: one canonical code for container tags (`eng`/`ger`/`deu`/`en-US`), release tags (`GERMAN`, `ITA`, `VFF`, `MULTi`, `DL`) and profile requirements; a smaller release-tag list so `per`/`may`/`fin` in episode titles are not languages. |
| Profile model | complete | `Profile.Languages` + `Audio{Languages, Known}`; `LanguageMet` / `LanguageAcceptable` / `Satisfied`; `Upgrade` takes the release's languages and the disk's audio — a file failing the requirement is beaten by any acceptable release that offers it, rank ignored, floor and resolution cap kept. |
| Don't-churn on the new axis | complete | Absence is a fact only when every audio track declares a language; an untagged track, an unprobed file, or `und` never starts a hunt. Weakest-link across a season / an item's files. |
| Parser | complete | `Parsed.Languages` read from the tag section after the title; silence = English; `MULTi`/`DUAL`/`DL` = `mul`; `WEB-DL` stripped before tokenizing; subtitle tags ignored. |
| Decision engine | complete | `Decide(Release{Quality, Languages}, …)`; new code `language_not_wanted`; target-met requires both axes. |
| Wanted / grab / import | complete | `wants()` uses `Satisfied`; RSS, backlog and auto-search route through `releaseOf(parsed)`; import judges the language by the release name, the post-placement probe records the truth. Wanted row: `upgrade from WEB-DL 1080p (German)`. |
| Storage | complete | Profile definition JSON gains `languages` (omitted = none). Migration **0034** `media_files.audio_languages` (declared tag per track, `+`-joined, `und` untagged), backfilled in SQL from `media_info`, written at every probe; the list-view stats tuple reads it (`~langs`) instead of `json_each` (~6× cheaper); `DiskStateForItem.Audio`. |
| API | complete | `audioLanguages` on `QualityProfile`/`ProfileInput` (validated against `GET /languages`), `upgradeReason` + `audioLanguages` on items, `languages` on `ReleaseCandidate`; facts pill names the language. |
| Web | complete | Profile editor: "Required audio languages" pill picker (Film & TV only) with live sentence; item page: `upgrading · no English audio on disk (German only)`; interactive search shows non-English advertised languages. Mobile: types only. |
| Tests | complete | `TestJustFriends` (7 scenarios: wanted, grabs the English 1080p over the German remux, lower quality in the right language wins, wrong language only → nothing, untagged → not hunted, English among others → done, disk state), grading on list + get paths, profile round trip, API contract, parser/language/mediainfo/decision unit tests, e2e profile editor. |
| Docs | complete | ADR 0022, `docs/settings.md` §Quality profiles, `docs/usage.md` (interactive search, item page, loops), VERSION 0.31.0. |
| Draft PR → adversarial review | addressed | PR #46 opened as draft. Review: 2 must-fix (the grab→import→still-wanted **loop** for untagged wrong-language releases — now bounded: a measured mismatch blocklists the release + `language_mismatch` history; lint misspell), 7 should-fix (subtitle-adjacent tags read as audio · manual import could delete an English file for a German one · "undetermined" claim not rendered · book profiles could require a language · web sentence order · `json_each` cost · wanted "(English)" noise), 8 nits — all fixed with regression tests (`TestLanguageMismatchIsBlocklisted` on a real `ger`-tagged MKV fixture, `TestManualImportNeverDeletes…`, Erai-raws/NL.Subs parser cases, migration 0034 backfill, `weakestRecorded` mixed files, book-profile 400, e2e "audio language undetermined"). |
| Full gate (once, after fixes) | green | `make lint` 0 issues (golangci v2.12.2) · `make test` all ok · `make test-web` 104 · `make test-e2e` **117/117** · coverage **86.1 %** (floor 86.0) · touched packages ×3 `-shuffle=on` green. Not verified here: nothing outstanding — no native change, no hardware. |
| Merge | complete | GitHub PR #46, all four checks green (Lint · Tests · Compat · Docker) on `e755d51`, rebased onto `10d5a96` (#45 landed mid-flight; STATUS.md only). Merged as `85f66df`, tagged `v0.31.0`, branch deleted. Pushed through a scratch clone in the Mac VM's `/var/tmp` (the cloud git proxy refuses pushes to this repo). |
| Deploy — server | complete | `media/deploy.yml -e sync=false -e only=monarr --limit nuc3` (ansible-core 2.21 on nuc3 itself, `-c local`, under tmux — the Mac VM kills background processes at the end of each call, so the controller cannot host a 5-minute play; Paul's `~/code/monarr` untouched). nuc3 `/api/v1/system/status` = **0.31.0 @ `85f66df`, schema 34**. |
| Deploy — clients | n/a | Mobile types only; no native release. |
| Live check — Just Friends (2019) | complete | Item 522: file `WEB-DL 1080p` measured **Dutch/Portuguese** audio (facts pill now says so). Profile "Best" (332 items) left as is; a new profile **7 "Best · English"** (same target, English required) was created and assigned to this one item. Item page: *upgrading · no English audio on disk (Dutch or Portuguese only)*; wanted: *upgrade from WEB-DL 1080p (Dutch/Portuguese)*. The scheduled search grabbed `Just.Friends.2018.1080p.AMZN.WEB-DL.DDP2.0.H.264.DUAL-100REAL` within a minute of the deploy (download 1221) — the same release whose manual import at 23:17 had been refused as "does not improve on the WEB-DL 1080p already here". **Paul's call:** tick English on "Best" / "1080p" to apply the rule library-wide (every file measured without English audio becomes wanted; untagged releases of non-English films will each be downloaded once and then blocklisted). |
## Previous delivery — Import notify wedge (plurx queue) + retryable deliveries

**Status:** done — Curator merged `b71e8c6`, tagged v0.30.0, deployed to nuc3; plurx #608 merged and on all four nodes; backlog re-sent · **Updated:** 2026-09-28 · **Version:** 0.30.0 ·
PR [#44](https://github.com/pjunod/curator/pull/44) · plurx PR [#608](http://192.168.4.7:3000/noirr/plurx/pulls/608) — which duplicated the earlier open [#605](http://192.168.4.7:3000/noirr/plurx/pulls/605); Paul ruled: leave what's done, the other session landed the remaining pieces. Lesson recorded: list open PRs before building a fix.

| Item | State | Evidence |
|---|---|---|
| Diagnosis | complete | Monarr's `notify.plurx` posted `/api/v1/scan`; plurx (nynuc, m6, nuc4 at `g86d516219`) answered 500 `storage task failed: library work was not accepted: QueueFull` from three seconds after startup. Replicated `background_jobs` held exactly 10,000 rows — 6,219 succeeded embeddings, 2,700 succeeded subtitle extractions, 971 queued, 5 running — all since the 2026-09-27 02:32 reset. The table bound counted settled history; history is pruned only after seven days. The imported files themselves had landed (`/20t/movies/Lonesome (2023)/…mkv` present); only the index notify failed. |
| plurx fix | complete (#608; duplicated #605) | SQLite migration 85 / replicated schema 63 replace the enqueue and upkeep triggers: admission evicts the oldest evictable settled rows at the bound, upkeep drains history from 9,000, and `queue_full` for size is answered only when nothing is evictable. `POST /api/v1/scan` maps a refused admission to 503 naming the refusal. Regressions: SQLite admission-at-bound + upkeep convergence; replicated upgrade from the exact v62 predecessor with 10,000 settled rows; per-library bound still refuses as `TargetError::Refused`; HTTP 503 contract. |
| Curator: retry failed deliveries | complete | `POST /notifiers/{id}/deliveries/{deliveryId}/retry` and `…/deliveries/retry`; row goes back to pending, due now, attempts reset, last error kept until the next attempt. **Retry** per failed row and **Retry all failed (N)** in the delivery log. Tests: API (`TestAcqFailedDeliveriesCanBeRetried`, unknown ids), worker redelivery (`TestAFailedDeliveryCanBeRequeuedAndThenDelivered`), web (`deliveries.test.ts`). |
| Adversarial review | addressed | Curator: 6 findings (docs/integration.md §5 stale, sweep scope vs 100-row window, disabled notifier, error span, stale `result`, `:execrows`) all fixed in `61563d6`. plurx: 6 findings (evict-the-named-row, MAINTENANCE_NEEDED predicate, 503 scope, install order, receipts nuance, verification gaps) fixed in `835316801`. |
| Full gate | green | Curator local: lint 0 · `go test ./...` · notify/api/sqlite `-race -count=2..3 -shuffle=on` · web 92 + build · e2e 117/117. GitHub on `61563d6`: all 7 checks green. plurx on nuc3 (1.97.1): clippy `-D warnings` · core lib 1367 · replicated v62→v63 contract · plurxd http/scan+bounded+library selections; Forgejo fast lane + promotion gate green on `6e0041509`. |
| Deploy — plurx | complete | ansible `deploy.yml -e sync=false -e only=plurx` (run from nuc3 with `~/.local/bin/ansible-playbook` — a run from the Mac VM dies when the bridge call returns): nynuc, m6, nuc4, nuc3 all on `v0.3.0-5192-g29358ce51`, healthy, replicated schema v63, **0 `QueueFull`** on every node afterwards; the 1,540 queued jobs drained. |
| Deploy — server | complete | `deploy.yml -e sync=false -e only=monarr --limit nuc3` from the Mac VM (ansible-core venv in `/var/tmp`): `/api/v1/system/status` = 0.30.0 @ `b71e8c6`, schema 33. |
| Deploy — clients | n/a | No mobile change. |
| Backlog re-sent | complete | `POST /api/v1/notifiers/1/deliveries/retry` → `{"requeued":88}` (Sept 2–13 503s, Sept 14–16 unreachable, the Aug 22–25 permanent ones, the Sept 27–28 500s). Three minutes later: **593 ok, 0 failed, 0 pending** — every one answered `queued as sr-…` and plurx is indexing them. Side note for later: `Absent (2011)` accounted for 34 of the failures — the same item imported repeatedly. |

## Previous delivery — One folder, one item

**Status:** merged `e97a46f` · tagged v0.28.1 · deployed to nuc3 · **Updated:** 2026-09-25 ·
**Version:** 0.28.1 · PR [#39](https://github.com/pjunod/curator/pull/39)

| Item | State | Evidence |
|---|---|---|
| Independent clone | complete | Cloud clone of `main` @ `8e6cced`; pushed through a scratch clone in the Mac VM's `/var/tmp` (cloud git proxy refuses the repo). User checkout untouched. |
| Folder choice | complete | Add, root-folder edit, placement suggestion and copies use `freeFolder`: plain `Title (Year)`, else `… {tmdb-N}` (tvdb / imdb / olid / monarr-id, then counter). |
| Refusals | complete | Typed path, manual entry, copy folder held by another item → `409 folder_conflict` naming the holder. |
| Migration 0032 | complete | Repairs shared folders (item with files keeps it), cleans stored spellings, adds unique index on `media_items.path`. Tested against the real v31 schema. |
| Adoption | complete | Reads `{tmdb-N}` / `[tmdbid-N]` hints; holder check uses cleaned paths; a failed adoption removes the item it created. |
| Adversarial review | addressed | 3 should-fix (copy in a held root refused; uncleaned holder → stray item; concurrent same-name adds → 409) + 4 nits, all fixed with regression tests. |
| Full gate | green | Local: lint 0 · Go race+coverage 87.0 % (floor 86.0) · web 88 + build · e2e 111/111. GitHub on `657bd2b`: all 7 checks green. Mobile unchanged. |
| Deploy — server | complete | ansible `deploy.yml -e only=monarr -e sync=false --limit nuc3` (monarr runs only there): `/api/v1/system/status` = 0.28.1 @ `e97a46f`, schema 32. Migration moved item 731 (Leviticus, tmdb 1044056) to `/20t/movies/Leviticus (2022) {tmdb-1044056}`; item 566 kept the plain folder (neither had files). `library-folders` health check now OK. |
| Deploy — clients | n/a | No mobile change. |

## Previous delivery — Native interactive search

**Status:** merged `7d1b5e7` · server deployed · clients handed off · **Updated:**
2026-09-23 · **Version:** 0.28.0 (mobile 0.27.0, build 25) · PR [#36](https://github.com/pjunod/curator/pull/36)

| Item | State | Evidence |
|---|---|---|
| Independent clone | complete | Branch `feat/mobile-interactive-search` on upstream `4a90fb0`; user checkout untouched. Pushed through a scratch clone on the Mac VM because the cloud git proxy refuses this repository; the repo has since been renamed `pjunod/curator` on GitHub. |
| API client | complete | `searchReleases` (120 s timeout, partial-result headers) and `grabRelease` in `mobile/src/api.ts`. |
| Search screen | complete | `mobile/src/screens/ReleaseSearchScreen.tsx`: every candidate, rejections visible, Would-be-grabbed view, Grab / Grab anyway (confirm), partial banner, season + episode chips, late answers dropped. |
| Detail entry points | complete | Interactive search on the item (series: first real season), Search pack per season, Search per quality copy / book edition (series copies open on the first season). Hardware Back returns to the detail page. |
| Feature gate | none | Nothing to enable: the screen calls the server's existing endpoints. No Developer-tab section because there is no toggle. |
| Adversarial review | addressed | 8 findings, all fixed (series copy without a season → 404; series with no seasons; copy name lost from subtitle; late search answer over a newer scope / setState after close; filter re-rendering every row; inert chips; two doc inaccuracies). Plus a pre-existing server bug it found: whole-item grabs (movies/books, web and mobile) failed candidate-token verification (search issues season 0, grab defaults to −1) and lost retained match evidence — fixed in `consumeCandidateToken`, regression-tested at both the service and API level. |
| Main was red — fixed here | complete | (1) e2e fake indexer advertised no search capabilities, so season searches were generic probes answered with the movie catalogue — 6 browser specs failed from that. (2) A grab/poll race erased the `grabbed` handoff step when a client finished instantly (phase7 flake) — the step is now written before the client is asked. (3) Queue rows were 156 px because the identity line wrapped four times — one elided line now. (4) Wanted's direct row hid which copy was wanted — pill restored. (5) Coverage was 82.4 % against the 85 % floor — six new test files cover the API/library/adapter/acquisition gaps; a blank alias now answers 400 not 500. |
| Full gate | green | Locally: lint 0 issues · Go race+coverage 87.1 % (floor now 86.0) · web 88 tests + build · mobile strict TS + 89 tests + Expo Doctor + iOS/Android bundles · e2e 111/111 twice · compat 17/17. On GitHub for `0346afe`: Lint, Tests (unit + e2e + mobile), Compat and Docker all green — the first fully green run since `4a90fb0`. |
| Deploy — server | complete | `deploy.yml -e only=monarr --limit nuc3 -e sync=false` from the Mac VM: nuc3 rebuilt and healthy, `/api/v1/system/status` reports 0.28.0 @ `7d1b5e7`. His `~/code/monarr` checkout was not synced (sync=false) and is untouched. |
| Deploy — clients | handoff | Physical iOS/Android installs need this Mac's Xcode/adb: `curator-mobile-deploy-handoff.md` (session outputs) is the prompt for a session with that access. |

## Previous delivery — Folder repair

**Status:** adversarial findings addressed; fast lane green · **Updated:** 2026-09-19 ·
**Version:** 0.25.2 · PR [#30](https://github.com/pjunod/monarr/pull/30)

| Item | State | Evidence |
|---|---|---|
| Independent clone | complete | Integrated onto upstream `3aaec3f`; user checkout left untouched after the workflow instruction. |
| Undownloaded titles | implemented | Missing-folder reporting requires previously recorded media; old reports are filtered on read. |
| Real folder issues | implemented | Missing, inaccessible, and non-directory paths have distinct explanations. |
| Bulk repair | implemented | Browse existing folders, apply entered paths across pages, retain per-row errors, and recheck restored drives. |
| File preservation | implemented | Matching files retain identity, copy assignment, quality, provenance, and episode links through a transactional location update. |
| Adversarial review | addressed | Preserve moved-file identities; remove silent retention of unavailable separate-copy records. Both findings have regression coverage. |
| Fast lane | green | Go 1.25.7 focused folder service/API regressions, one bulk-repair browser test, web typecheck/production build, backend build, lint (0 issues), generated API consistency, and focused identity/search regressions. |
| Full suites | delegated | Separate batch process handles full unit failures under the 2026-09-19 workflow. Existing GitHub Actions are unchanged. |

The review fixes are in `38f840e`; `e00579e` resolves three existing
staticcheck findings without changing behavior. Checks were run after the
review fixes. Full local unit and browser suites were not rerun. GitHub's
existing workflows still run automatically; the first PR run's mobile job
failed Expo Doctor because seven pinned Expo packages are behind the SDK's
expected patch versions. No mobile files change in this PR; that finding is
recorded for the separate batch process. The linked PR is the authoritative
merge state.

See [folder repair](docs/settings.md#disk-scan--repairing-folders-with-recorded-media)
for operator behavior. No feature toggle is introduced.

## Previous delivery — Open Library refresh resilience

**Status:** ready to merge; adversarial review approved; fast lane green ·
**Updated:** 2026-09-19 · **Version:** 0.25.1

| Item | State | Evidence |
|---|---|---|
| Separate clone | complete | Work based on upstream `8ce3590`; original checkout excluded from delivery. |
| Connection reset recovery | implemented | Three attempts with backoff for transient network and HTTP failures. |
| Open Library pacing | implemented | One request per second; retries share the same budget. |
| Retry-After and response cache | implemented | Respect provider delays; stop a lookup for waits over 30 seconds; cache only valid JSON. |
| Regression coverage | green | Reset recovery/exhaustion, interrupted body, timeouts, HTTP errors, cancellation, pacing, and cache behavior. |
| Adversarial review | approved | Independent review of `d316497` against `8ce3590`; no actionable introduced defects. |
| Fast lane | green | Go 1.25.7: adapter/library and architecture tests (`-count=1 -shuffle=on`), scoped vet, backend build, formatting and diff checks passed. |
| Delivery | ready for PR | One batch includes code, regression tests, operator documentation, and patch version. |

The operator behavior is documented under
[Refresh metadata](docs/usage.md#the-item-page). Per the delivery request,
this batch uses one fast lane after review instead of the full local suite.
Existing GitHub Actions remain unchanged. No feature toggle is added.

## Previous delivery — Wanted list controls

**Status:** ready to merge · adversarial review addressed · fast lane green ·
PR [#28](https://github.com/pjunod/monarr/pull/28) · **Updated:** 2026-09-19

| Item | State | Evidence |
|---|---|---|
| Missing/upgrade reason filter with counts | complete | `web/src/pages/Wanted.tsx` |
| Search across every visible field and target ID | complete | `web/src/wanted.ts` |
| Title/reason/type sorting and 25–500/all pagination | complete | shared `Pager` controls |
| Adversarial review | addressed | Persisted page clamp after live shrink; exposed current sort direction to assistive technology. |
| Fast lane | green | Production build/typecheck and 82 web tests passed on `fd34b44`. |
| Delivery | PR open | Forgejo has no Monarr repository, so the configured GitHub upstream is authoritative. |

## Progress at a glance

| Phase | Done | Definition of done |
|---|---|---|
| 0 — Walking skeleton | **17/17 ✅** | `docker run` → UI loads, status reports, tests pass |
| 0.5 — Tests, hooks, CI hardening | **7/7 ✅** | full pyramid green locally and in CI |
| 1 — Library | **13/13 ✅** | real media folders imported and browsable |
| 2 — Acquisition core | **16/16 ✅** | search → grab → import → correctly named file (movie + season pack) |
| 2.5 — Books (ADR 0006/0018) | **8/8 ✅** | keep and curate ebook + audiobook editions of one work |
| 3 — Automation | **9/9 ✅** | runs unattended for a month |
| 4 — Ecosystem compat | **5/6 ✅** (stretch deferred) | Jellyseerr/Prowlarr/Bazarr work against the shim |
| 5 — Depth & parity | **7/7 ✅** | custom formats, client zoo, lists, anime |
| 6 — Quality truth (ADR 0013/0014) | **9/9 ✅** | on-disk quality measured; target profiles; churn regression pinned |
| 9 — Discover (ADR 0015) | **7/7 ✅** | browse what's good and add it without leaving the app |
| Native mobile apps | **7/7 ✅** | daily workflows compile for native iOS + Android runtimes |
| Launch logistics | 4/9 | public repo, releases, name housekeeping |

## Native mobile apps ✅ (built 2026-08-01, v0.19.1)

- [x] One Expo/React Native project produces iOS and Android bundles
- [x] First-run setup automatically discovers and displays IPv4/IPv6 servers
      through DNS-SD on Wi-Fi, including a host-network companion for Docker;
      pairs by QR, or normalizes manual URLs
- [x] API key persists only in iOS Keychain / Android Keystore
- [x] Library/detail, item/profile/location edits, per-season monitoring,
      quality-copy management, search-now, Discover, and add flows
- [x] Wanted, Activity/queue progress and confirmed failed-row clearing,
      Calendar, System health, and web handoff
- [x] Per-device item sizing plus Auto/Light/Dark appearance; Auto follows the
      operating system and falls back to dark when its preference is unknown
- [x] Strict TypeScript and API/format/connection unit tests
- [x] Expo Doctor plus both native production bundles run in CI

Signed TestFlight and Play internal builds still require the release owner's
Apple/Google/Expo accounts and real-device QA; the code and build profiles are
ready, but credentials are deliberately not repository state.

## Phase 0 — Walking skeleton ✅ (shipped 2026-07-17)

- [x] Repo scaffold per blueprint §12 + GPL-3.0 license
- [x] Go module (`github.com/pjunod/monarr`) + Makefile build orchestration
- [x] Single static binary serving the embedded React shell (go:embed, SPA fallback page)
- [x] Spec-first `/api/v1` (OpenAPI 3 + oapi-codegen): `system/status`, `health`, `system/tasks`, `tasks/{name}/run`
- [x] SQLite via modernc: WAL mode, single-writer discipline, read pool
- [x] goose migrations embedded in the binary (up-only policy)
- [x] sqlc compile-time-checked queries (app_meta, scheduled_tasks)
- [x] Config: env > optional JSON file > defaults (port 7676, data dir, log level/format)
- [x] Structured logging via log/slog (text/json)
- [x] Typed in-process event bus (non-blocking fan-out, typed subscribe)
- [x] Scheduler: jittered intervals, manual trigger, panic recovery, state persisted across restarts
- [x] Health-check registry (database, data-directory, web-ui) publishing transitions on the bus
- [x] SSE event stream `/api/v1/events` (bus → UI live)
- [x] React + TS + Vite shell: Dashboard + System pages, live event feed, dark theme
- [x] Dockerfile: multi-stage, distroless, `/data` volume, port 7676
- [x] ADRs 0001–0006 + full architecture blueprint in `docs/`
- [x] Architecture dependency-arrows enforced by test (`internal/arch_test.go`)

## Phase 0.5 — Tests, hooks, CI ✅ (shipped 2026-07-17)

- [x] Go unit tests across all infra/app packages (run with `-race` in CI)
- [x] Web unit tests (vitest) for the UI API/formatting helpers
- [x] E2E suite: Playwright boots the real compiled binary, 7 specs incl. bus→SSE→browser round-trip
- [x] Git hooks in `.githooks/` (`make hooks`): pre-commit fmt/vet/typecheck, pre-push unit suites, graceful skip when a toolchain is missing
- [x] golangci-lint v2 pinned (`make lint`), zero findings
- [x] CI: 4 parallel jobs (unit + coverage summary + gen-drift check, lint, e2e, docker), least-privilege permissions, per-ref cancellation
- [x] STATUS.md as the explicit work ledger (this file)

## Phase 1 — Library ✅ (shipped 2026-07-17)

- [x] Migration 0002: `media_items` with `kind` CHECK (`movie|series|book` — book reserved per ADR 0006), external ids incl. isbn/olid/asin
- [x] `seasons` + `episodes` child tables
- [x] `media_files` + n:m file↔episode link table (ADR 0002)
- [x] `root_folders` + `settings` tables (TMDB API key lives in the DB, managed in UI)
- [x] `MetadataProvider` port (search + hydrate) — first port with a real consumer
- [x] TMDB adapter: auth, rate limiting, response caching
- [x] Add-movie flow: search TMDB → add → metadata hydrated
- [x] Add-series flow with full season/episode hydration
- [x] Library list/detail API endpoints
- [x] Library browse UI (posters, list, detail)
- [x] Root-folder management (API + UI)
- [x] Disk scan: walk root folders, parse on-disk files
- [x] Reconcile: diff disk vs DB, repair links, flag TMDB-ordering disagreements for manual review

## Phase 2 — Acquisition core ✅ (shipped 2026-07-17)

- [x] Release-name parser v1 (pure function, table-driven)
- [x] Golden corpus harness over `testdata/releases/` at 100% conformance (54 cases hand-seeded from upstream naming patterns; literal Sonarr/Radarr suite port remains an easy extension)
- [x] Parser fuzzing (must never panic on arbitrary bytes)
- [x] Wantable implementations (movie, episode, season) — season-pack fan-out modeled
- [x] `SearchPlanner` + `ReleaseMatcher` per media kind
- [x] Matcher: title normalization, year tolerance, season/episode + pack coverage
- [x] Decision engine v1 with machine-readable rejection reasons
- [x] Quality model + profiles (ordered groups, cutoff, upgrades)
- [x] Torznab/Newznab indexer adapter (search + caps test, one adapter for both; RSS lands with Phase 3)
- [x] Indexer management (API + UI)
- [x] Interactive search UI with rejection reasons attached
- [x] qBittorrent download-client adapter
- [x] SABnzbd download-client adapter
- [x] Queue tracker: client polling reconciled to the Download state machine
- [x] Importer: scan payload, map files→Wantables, per-file decisions
- [x] Renamer with Sonarr/Radarr-compatible tokens; hardlink-or-copy import

## Phase 2.5 — Books (ADR 0006/0018) ✅ (shipped 2026-07-18; editions 2026-08-12)

- [x] Metadata bake-off → **Open Library** (open data, no key, work-level ids, covers; Google Books rate-limits anonymous callers, Hardcover needs an account) — `BookProvider` port + adapter, fixture contract-tested
- [x] Book external IDs end-to-end (isbn13 from editions, work OLID, asin column reserved) — unique `(kind, olid)`
- [x] Book-mode parser rules + golden corpus (12 cases seeded from Readarr/scene naming patterns; literal Readarr suite port remains an easy extension) — author extraction, order-swap tolerant matching
- [x] Book `SearchPlanner`/`ReleaseMatcher` (author+title queries); persisted edition type routes ebooks to Torznab 7000/7020 and audiobooks to 3030
- [x] Format families: PDF/MOBI/AZW3/EPUB ebooks plus MP3/WMA/AAC/OGG/Opus/M4A/M4B/FLAC/WAV audiobooks; seeded **Ebook** (id 4) and **Audiobook** (id 5) profiles with independent defaults
- [x] `{Author Name}/{Book Title}` naming, Calibre-friendly single files (`Title - Author.ext`) and stable multipart audiobook names (`… - 001.ext`); import + scan grade book files by extension
- [x] First-class Ebook/Audiobook add choices, library tabs, badges, same-family profile pickers, API `bookType`, and web/native curation — e2e covers one work holding EPUB + M4B together and a separate three-part M4B import
- [x] Side-by-side editions (ADR 0018): one Open Library work owns ebook and audiobook targets independently; API `bookTypes`, per-edition search/grab/wanted/import/files, add-the-missing-edition flows, and shared-folder scan attribution

## Phase 3 — Automation ✅ (shipped 2026-07-18)

- [x] Wanted index (in-memory, invalidated on add/scan/import/delete events, lazily rebuilt; in-flight downloads and pack-covered episodes excluded) — `GET /wanted`
- [x] RSS sync loop per indexer (15 min, jittered): fetch → parse → match wanted → decide → auto-grab
- [x] Backlog search (12 h + manual): planned queries per wantable, best accepted release grabbed, 20/run cap
- [x] Failed-download handling: blocklist (unique release+indexer, `GET/DELETE /blocklist`) + automatic re-search on client failure or import error
- [x] Calendar (API + UI): airing episodes + movie/book release dates, agenda view with -7/+30 day paging
- [x] Calendar rebuilt (2026-08-02, v0.20.0, [ADR 0016](docs/adr/0016-air-times.md)): month grid carries state as colour with a `+N more` day panel and a legend; new rolling **Agenda** view (sticky day headers, poster rows, `SxxEyy`, network, status pill) on web and native; real air times from TVmaze's show-level schedule composed to UTC per date; `/calendar` gains `airDateUtc`/`network`/`posterPath`/`season`/`episode`/`episodeTitle`/`runtime`/`monitored`, all optional, `detail` untouched
- [x] Webhook notifier (stable lowercase JSON wire format)
- [x] Discord notifier (embeds via webhook URL)
- [x] Plex/Jellyfin library-refresh notifiers (poked on imports only, never chatter)
- [x] Scheduled SQLite online backups: daily `VACUUM INTO`, last 7 kept, listed on the System page — e2e proves the whole circle: delete file → rescan → wanted → RSS re-grab → re-import → webhook notified

## Phase 4 — Ecosystem compat ✅ (shipped 2026-07-18; stretch importer deferred)

- [x] `/sonarr/api/v3` personality: status/health/profiles/languageprofile/tag/rootfolder/queue/history/command + series list/get/add (tvdbId resolved via TMDB /find), series/lookup (`tvdb:` + text), episode + episodefile for Bazarr
- [x] `/radarr/api/v3` personality: movie list/get/add (tmdbId native), movie/lookup, moviefile; books never leak through either personality
- [x] X-Api-Key auth (header or ?apikey=; key auto-generated, shown in Settings), case-insensitive routes, plausible version strings (Sonarr 4.0.10 / Radarr 5.14.0)
- [x] Unknown-v3-request logging (counter + warn line naming the path) to guide shim expansion
- [x] Conformance harness: `test/conformance/` docker-compose vs real Jellyseerr + Prowlarr + Bazarr, plus an in-process fake-consumer suite replaying their exact call flows (Jellyseerr add flows, Prowlarr indexer sync incl. idempotent re-sync + PUT/DELETE, Bazarr enumeration) that runs in CI
- [ ] (stretch) one-shot *arr DB importer — deferred post-1.0; filesystem adoption (ADR 0005) is the supported migration path

## Phase 5 — Depth & parity tail ✅ (shipped 2026-07-18)

- [x] Custom-format scoring engine: regex rules with scores summed per release; breaks ties in search ranking and automation best-pick; API/UI CRUD, invalid patterns rejected up front, scores shown in interactive search
- [x] More download clients: **Transmission** (RPC + 409 session handshake), **Deluge** (web JSON-RPC), **NZBGet** (JSON-RPC append-by-URL, usenet routing) — contract-tested, in the client factory + settings UI
- [x] Import lists: TMDB Popular/Top Rated + public Trakt lists (per-list client id); `importlists.sync` (12 h) adds missing entries with list policy, duplicate-safe; API/UI CRUD
- [x] Anime absolute numbering: `[Group] Show - 15` parsing (v2/ranges), absolute matching on episodes, cumulative absolute numbers derived at TMDB hydration (AniDB/TVDB mapping tables remain the refinement path); corpus 70/70
- [x] Access tab: user login, mobile pairing QR, and separately revealed API
      key; opt-in `authRequired` gate on `/api/v1` uses salted-hash credentials
      plus browser sessions and refuses to enable without credentials
- [x] Mass editor: select-mode on the Library page → bulk monitor/unmonitor/profile via `POST /library/bulk`
- [x] Optional Prometheus `/metrics` (`MONARR_METRICS=true`): build info, items by kind, active queue, wanted total, uptime — hand-rolled exposition, no new dependency

## Phase 6 — Quality truth (ADR 0013 + 0014 — built 2026-07-25, v0.6.0)

Plan: [docs/plan-quality-truth.md](docs/plan-quality-truth.md). Fixes the
adopted-file blind spot (unknown on-disk quality is hunted as *missing* →
duplicate grabs) and replaces the allowed-list+cutoff profile model with
target-based profiles ("Any — upgrades until WEB-DL 1080p, then stops" was
the symptom).

- [x] `domain/mediainfo` native prober: MKV (EBML) + MP4 header walkers — resolution, codec, bit depth, HDR/DV, audio, duration/bitrate; fixture corpus + fuzz; no runtime deps (plan §4, M1)
- [x] Migration 0018: `media_files` media_info + provenance/confidence/probed_at; `DiskStateForItem` replaces ambiguous `BestQualityForItem` (plan §5/§7, M2)
- [x] Probe wiring: `library.probe` jobs from scan (deduped per file), inline probe on import; backfill = first post-upgrade scan (plan §6.1–6.2, M2)
- [x] Source inference + filename cross-check: measured resolution absolute; source inferred with confidence; tokens demoted to hints; provenance recorded (plan §4.3, M3)
- [x] Target-based `Profile` (floor/target/upgrades) + migration 0019 in-place seed rewrite, ids stable; "Any" retired → "1080p" (ADR 0014 §4, M4)
- [x] Decision engine + wanted index on the `Met/Acceptable/Upgrade` predicates — unknown ≠ missing; `TestUnknownQualityOnDiskIsNotHunted` pins the churn fix (M4)
- [x] Profile CRUD API + editor UI; QualityFacts measured pill + provenance badges; Files table quality columns (plan §6.3, M5)
- [x] Compat `/qualityprofile` synthesized from targets — fake-consumer suite green untouched (plan §6.4, M5)
- [x] Post-import verification: `quality_mismatch` history event (log-only); docs updated (usage/settings/architecture); ADRs 0013/0014 → Accepted (M6)

## Phase 7 — The nzbd integration (plan: `docs/archive/plan-integration.md`)

Making the three apps behave like one pipeline: events instead of timers at
every seam, and one id that names a transfer from grab to playable. nzbd's
side (N1–N7) is built; this is Monarr's.

- [x] **§5.1 native `nzbd` client** — `internal/adapters/nzbd`, a separate
      type from `nzbget` because it is a different API with different
      capabilities. Add / Statuses / Remove / Test against `/api/v1`, with
      post-processing reported as a named stage rather than a stall, and a
      job deleted by hand in nzbd reported as removed rather than as a bad
      release (which would blocklist it). Migration 0020 widens the type
      CHECK; UI, openapi and the web client types follow
- [x] **The transfer id** — `t-<downloads.id>-<6 hex>` on `downloads`,
      sent to nzbd as an add-time job param via the optional
      `ports.TaggedAdder` capability, and opening the handoff trace. The
      row is now inserted before the client add, because the id is built
      from the row id; a refused add cleans up its own row
- [x] **§5.2 push subscription** — `ports.Subscriber` + the nzbd SSE
      consumer (opaque `Last-Event-ID` resume, backoff reconnect, `reset`/
      `lagged` → reconcile by poll), a supervisor goroutine per push-mode
      client, `download_clients.mode` (`poll`|`push`, default poll), and a
      **Live updates** toggle that only appears for a client that can
      stream. Push runs beside the poll, never instead of it
- [x] **§5.3 one reconciler** — `reconcileDownload` extracted so poll and
      push share one brain, serialized per download id so the two channels
      cannot both import. The trace names the channel that delivered each
      step (`(event 913)` vs `(poll)`) — the difference between knowing
      push works and assuming it does
- [x] Verified against a **real nzbd**: a real download driven end to end
      while subscribed, receiving the actual `par_rename → rar_rename →
      post_unpack_rename` stages and a completion carrying the real final
      directory. The httptest fixtures are payloads captured from that run
- [x] **§5.4–5.5 import paths + the plurx notifier** — `ImportCompleted`
      carries the absolute paths that landed, the item's TMDB/IMDb ids,
      whether they are episodes, and the transfer id; book events also carry
      Curator's author, persisted medium, exact work/edition ids, and a
      strictly allowlisted Open Library cover URL. The three per-file
      importers return where each file went. The `plurx` notifier sits with
      Plex and Jellyfin in the import-only group but is not a refresh poke:
      it says "index this path, it is tmdb 949". Scoped `plx_` key, never an
      admin token. Retry covers the connection and 5xx and stops dead on
      401/403/422, and plurx's 422 — the path-mapping one everybody hits —
      is carried through verbatim with the roots it listed. Deliveries land
      recorded on a **persistent delivery queue** (`notifier_deliveries`,
      migration 24): first attempt immediately, retries at 5 s / 30 s / 2 m,
      terminal failures (401/403/422/404) not retried at all. The row is the
      point — the common miss is a host reboot where an in-memory retry dies
      with the process that owns it. The outcome lands on the download's
      handoff trace as a `notify_plurx` step, carrying plurx's own answer
      (`scanned → plurx item 1201`) via the optional
      `ports.DeliveryReporter`. `PUT /api/v1/notifiers/{id}` (edit in place —
      the log hangs off the id) and `GET .../deliveries` + a **Delivery log**
      on the notifier row
- [x] **§5.6 health checks** — one line per connection
      (`client:<name>`, `client:<name>:capacity`, `mediaserver:<name>`) via a
      new `RegisterGroup`, since what is worth checking is configured in the
      UI and not known when the registry is built. Probes run in parallel;
      a client that answers while nothing comes through it is reported at
      5 min / 30 min. nzbd capacity — low disk, quota, blocked servers,
      paused queue — via the optional `ports.CapacityReporter`, with an
      paused queue — via the optional `ports.CapacityReporter`. Chat
      notifiers and Plex/Jellyfin are listed but never probed: their only
      test is the action itself
- [x] **§5.7 Connections panel** — `GET /api/v1/system/connections` and a
      card on System: one row per remote app, `live | polling | degraded |
      unreachable | unprobed`, last-contact age and `event #913` for push
      clients, URLs redacted. Assembled from the last health probe plus the
      supervisor's live state rather than probing on demand, so the page
      does not become load on the servers it reports about. `polling` is
      styled as healthy, not as a lesser `live`: it is the fallback working
      as designed, and colouring it as a fault trains people to ignore the
      one that is

## Phase 8 — Plausibility (built 2026-07-27)

Real-world validation of the Phase 6 inference bands, which is exactly what
this section was waiting for — and the first thing it turned up was a hole.

A 500 MB file named `HIM (2025) [Remux 2160p].mkv` — a 96-minute feature —
imported, graded **REMUX 2160P**, and marked the movie satisfied. 500 MB over
96 minutes is 725 kbps: one sixtieth of a real 2160p remux, and less than the
TrueHD track it declares would need on its own.

The hole was specific. `Contradicts()` could only reject a remux claim when the
file had NO lossless audio, so declaring a TrueHD track made the claim
unfalsifiable — and a declared track is not a delivered one. Under that, every
band in `inference.go` is a lower-bound ladder bottoming out at
`SourceUnknown`, and `Resolve` accepts any filename token the measurements do
not actively contradict. "Inconclusive" and "obviously fabricated" reached the
same place: the name won.

The damage was never the badge. It was that a fake file **retired the want**.

- [x] `domain/mediainfo/plausible.go`: absolute bitrate floors per resolution
      tier (~3× below the lowest real encode), declared-audio-vs-total-bitrate
      contradiction, and measured-duration-vs-metadata-runtime. Skipped under
      60 s, where overall bitrate describes the container rather than the
      content — and a file that short is caught by the runtime rule anyway
- [x] `Contradicts()`: a remux claim below the bottom of the **encode** band is
      refused whatever the audio headers say
- [x] `ProvenanceImplausible`, distinct from `Failed`. Failed means we do not
      know, which is a reason to leave a file alone; this means we do know, and
      what we know is that it is not what it claims — a reason to keep hunting
- [x] `DiskStateForItem` + `episodeStates` skip implausible files, so the item
      goes back to wanted instead of sitting green over 500 MB. The row stays
      visible and stops voting
- [x] `SizeImplausible()` gates RSS and backlog before the grab; interactive
      search only warns. Gate the robot, never the person
- [x] Migration 0023: `media_files.source_release`/`source_indexer`, recorded at
      import — the link a blocklist needs, which used to live only in the
      queue row and only until it moved on
- [x] `DELETE /library/{id}/files/{fileId}` (`fromDisk`/`blocklist`/`search`) +
      Files-table row actions; the interactive search window pages, filters and
      collapses its rejections
- [x] `phase9b-plausibility.spec.ts` — 6 e2e covering the whole chain; 80/80 green

**Follow-ups from the first week in the field (v0.8.1):**

- [x] Apostrophes are elision, not word breaks. `NormalizeTitle` spaced every
      non-alphanumeric run, so "The Carpenter's Son" read as "carpenter s son"
      against a release's "carpenters son" and forty candidates came back
      "does not match". Accents now fold to ASCII and the three spellings of
      an ampersand unify, both the same class of bug
- [x] `place()` treated ANY file at the destination as already imported, so a
      same-quality replacement copied nothing and the row was rewritten from
      the OLD file's size — re-grabbing a bad file could never fix it. Only
      the same inode counts now; everything else is replaced through a temp
      name and an atomic rename
- [x] A movie payload with several video files took whichever sorted first
      alphabetically. It takes the largest
- [x] `short_delivery`: the advertised size was stored at grab time and never
      compared to what arrived. Without it "a 500 MB fake" and "a 13 GB
      release that arrived 500 MB short" are indistinguishable and want
      opposite responses
- [x] **Truncation detection**: a Matroska Segment states its finished length,
      so a file smaller than its own declaration is provably incomplete —
      not an inference, arithmetic on two known numbers. This is the rule that
      separates a fake from a cut-off download, which no bitrate threshold can
      do: a fake is built from a real release's header, so it carries the same
      tracks, chapters and stated runtime. Validated against the fixture
      corpus, every file of which is a truncated real file by construction
      (`SIZES.txt` records each original), plus two new whole controls
- [x] A provably cut-short payload is REFUSED at import rather than placed.
      The one refusal in the import path, because it is the one case that is
      proof and not judgement — and because placing it loops: the item returns
      to wanted, the next pass grabs, lands another stump, repeat. Not
      blocklisted; the release was never the problem

- [x] **Cleanup after import**: monarr never removed a payload from the
      download client, so every grab left a second full copy behind — real
      bytes, not a hardlink, whenever the download disk and the library are
      different filesystems. A user found 923 GB of it. Per-client
      `remove_completed` (migration 0025), defaulted ON for usenet and OFF for
      torrents, which are still seeding; removal runs inline at import and an
      hourly `downloads.cleanup` sweep drains the backlog that accumulated
      before the setting existed

**Field diagnosis, 2026-07-27 — the exactly-500 MiB movies are TRUNCATED, not
fake.** `ffprobe` on HIM (2025): "File ended prematurely", last video packet at
61.6 s of a 96-minute feature. 500 MiB over 61.6 s is ~68 Mbps, which is
exactly a 2160p DV remux — the content is genuine and the transfer stopped.
Two files measured: 523,975,057 and 523,967,206 bytes, both 499.70 MiB, 7.8 KB
apart. Just under a round 500 MiB with a few KB of per-release variance is a
500 MiB cap on the DOWNLOADED data, the extracted MKV coming out under it by
rar/par overhead. Upstream of monarr — nzbd or the provider — and still open

### Phase 8 — watched → monarr (master plan §11.1)

- [x] **`POST /api/v1/webhooks/plurx`** — matched by external id only, never
      by title; an unknown item is 200 `matched:false`, not a 404, because
      plurx retries failures and there is nothing to retry about a film
      Monarr never managed. Migration 26 records it per plurx user, thin by
      design (who, what, when), replacing on rewatch so the table is bounded
      by library size rather than by television
- [x] **Backlog prioritization** — anything watched in the last 30 days is
      searched first. The per-run cap makes that decisive rather than
      cosmetic: on a large backlog the order decides what gets searched at
      all. A stable partition, not a score — the signal is "somebody watched
      this", and a numeric intensity would be precision nobody could check
- [x] Decisions recorded in `docs/archive/plan-integration.md` §11.1: per-user with
      usernames (overriding the sketch's aggregate), and **no deletion path
      on either side**
- [ ] Show it on the item page — the data is stored and read, but not yet
      rendered anywhere

## Phase 9 — Discover (ADR 0015, built 2026-07-29)

Plan: [docs/plan-discover.md](docs/plan-discover.md) · decision:
[ADR 0015](docs/adr/0015-discovery.md).

Monarr could find anything you could already name and nothing you could
not: every path into the library started at a search box. The answer to
"what should I watch" lived in a browser tab, and the round trip back was
copy-paste-a-title.

- [x] `ports.DiscoverProvider` — a provider publishes its own **catalogue**
      (`Lists`), says whether it can serve it (`Configured`), and serves one
      page of one row (`Discover`). The catalogue-first shape is what makes a
      second provider additive: Trakt's rows appear because Trakt names them,
      not because the UI learned about Trakt
- [x] **TMDB, nine rows, no setup** — trending (movies and shows), in
      theaters, coming soon, on the air, popular ×2, top rated ×2. Reached
      through the existing rate-limited, cached request path; the import
      lists' `DiscoverMovies` re-expressed over the same helper, signature
      untouched, because its vocabulary is stored in `import_lists.type`
- [x] **Trakt, five more, optional** — being watched right now (×2), most
      anticipated (×2), box office. Kept separate rather than merged because
      they answer a different question: Trakt counts scrobbles, TMDB counts
      lookups, and a blended ranking would be unverifiable. The adapter
      gained what it never had — a `KeyFunc` so the id comes from settings,
      a rate limiter, and a response cache keyed on id **and** URL, since the
      id is a header and a rotated one must not be served the old answer
- [x] `internal/app/discover` — 30 min fresh, **stale served for up to 24 h
      when upstream fails**, then the error. A page that goes blank because
      TMDB had a bad thirty seconds reads as a bug; a day-old row is not
      "less fresh", it is wrong. Artwork for providers that hand out bare ids
      is hydrated by the new shallow `tmdb.Summary` at concurrency 6 — never
      `GetSeries`, which would fetch every season of every show for a poster
- [x] `GET /discover/lists` + `GET /discover/items?list=&page=`; an empty
      catalogue is a **200 with an empty array**, not a 503, because "no
      provider configured" is a setup screen and not a failure. `inLibrary`
      answered by a new narrow `TmdbIDsByKind` query rather than the full
      `Library.List` the search handler uses — fourteen rows a page load is
      where that would have become expensive
- [x] `/discover` page: lazy rows (an `IntersectionObserver` 400 px ahead, so
      nothing below the fold is fetched), horizontal strips with desktop-only
      arrows, All/Movies/Shows tabs, and an add dialog that is a second door
      into `library.Service.Add` — never a second policy
- [x] **Poster size (S / M / L, labelled)** in the page head of Discover
      *and* the Library page. The label is on screen and is also the group's
      accessible name, so the two cannot drift; `.page-head` now wraps at
      every width, because the picker was what tipped Library past a 1000px
      window and nowrap crushes the actions rather than moving them. One `--card-w` token on `<html>`, so two components that
      must agree on a number share an attribute rather than state, CSS
      resizes the grid without re-rendering it, and a pre-paint script keeps
      the library grid from reflowing a frame after it draws. M is exactly
      what both were before, so an untouched install is unchanged
- [x] Discover strips show their scrollbar only under the pointer, with its
      space reserved at all times — fourteen rows that each grew 8 px on
      hover would shove the page around under the cursor
- [x] `zz-discover.spec.ts` (8 e2e) + `zz-cardsize.spec.ts` (2, measuring
      real geometry) + unit tests for both adapters and all four cache
      behaviours; **90/90 e2e green**

**Two pre-existing failures on `main` repaired to get there** — both test-
or CSS-only, no behaviour change, and neither caused by this work:

- [x] `smoke.spec.ts` asserted that any health check beyond a fixed five must
      be named `client:*`. The `imports` check (commit 719892b) was never
      added to that list, so main was red. The list is now written once and
      used twice — the drift was two copies of it
- [x] `phase9-layout.spec.ts` was failing on 156 px queue rows. The Activity
      rewrite (68575a3) widened the progress column to 210 px, and with every
      other column at a stated minimum the release title is the one on
      `width: auto` — it was left about 60 px and wrapped a scene name **one
      character per line**. `col-actions` 268 → 190: buttons wrapping to a
      second line is a far smaller cost than an unreadable title

## Launch logistics

- [x] `monarr-media` GitHub org (free) + private repo + initial push
- [x] **Moved to `pjunod/monarr`; module path renamed to match** (2026-07-29).
      The repo left the org, and GitHub forwards traffic from a renamed
      repository only until someone claims the old name — so the redirect was
      never something to build a module path on. Everything is `pjunod/monarr`
      now: `go.mod`, 242 files of imports, `arch_test.go`'s module constant,
      both sets of LDFLAGS, the ghcr.io image, the four README badges, the
      clone URL in usage.md. Done while nothing outside the repo depends on the
      old path, which is the only time this is a cheap change — ADR 0001
      amendment 2
- [x] Container registry decision: ghcr.io (no Docker Hub subscription — ADR 0001 amendment)
- [ ] File dormant-username request with GitHub Support for `monarr`
- [ ] Register a domain
- [ ] Flip repo public + branch ruleset requiring the four CI checks
      — **when this happens, the orphan `badges` branch can be deleted**; the
      coverage badge no longer uses it (see below)
- [ ] goreleaser: tagged releases with binaries + ghcr.io images (target: end of Phase 1)
- [x] **Coverage at 86.5%, floor enforced at 85%** (2026-07-29). Was 68.4%
      against no floor. The gap was concentrated, not spread: `internal/api`
      held 1,126 of the 3,061 uncovered statements while every domain package
      was already 85–100%, and the reason was structural — `newTestServer`
      wired four dependencies, so every handler reading Store/Library/
      Acquisition/Discover was unreachable from a test. The harness had to
      exist before the tests could. `COVERAGE_MIN` + `scripts/coverage-gate.sh`
      now fail the build below 85% on every push, in `make coverage` and in CI
      after the per-package summary is written. Two things deliberately left
      under: `cmd/monarr` (204 statements of `main()` — needs a `run(ctx, cfg)`
      extraction, worth doing on its own) and the storage-failure branches in
      `internal/api`/`internal/app/library`, which only fault injection reaches
- [ ] Extract `run(ctx, cfg) error` from `cmd/monarr/main.go` so the
      composition root can be started and shut down in a test — the last
      package at 0%, and the only one whose coverage needs a refactor rather
      than a test
- [x] **Coverage badge renders while the repo is private** (2026-07-29). It
      pointed at `raw.githubusercontent.com/…/badges/coverage.svg`, and GitHub
      fetches an absolute image URL through its proxy *anonymously* — so on a
      private repo it 404s. The four workflow badges are served by github.com
      itself and honour the viewer's session, which is why only this one was
      broken. `coverage.svg` is now tracked on main and linked relatively,
      which is served the same authenticated way and keeps working after the
      repo goes public; CI commits it back only when the number moves,
      rebuilding on the current tip so it can never revert a real commit.
      Generation was already local (`scripts/coverage-badge.sh`, no Codecov)
      and is unchanged

## Activity: bounded, paged, sweepable ✅ (2026-07-31, v0.15.0)

The page rendered `ListRecentDownloads` — the last 100 rows, every state, one
table — and nothing in the schema had ever deleted a download row or a history
event. "Finished" was therefore a section that only grew, and the table under
it grew for the life of the install.

- [x] **Retention.** `activity.retention`, daily: terminal rows and history
  events older than the window are deleted. Window is a setting
  (Settings → Library, `activity.retention_days`), default 30 days, `0` keeps
  everything. Rows that are still moving are never swept, at any age.
- [x] **Paging and filtering.** `GET /queue?filter=active|imported|failed&q=&limit=&offset=`
  (no params = the old behaviour, byte for byte) and `GET /queue/summary` for
  the section counts, so a collapsed "Finished (1,284)" costs one aggregate
  instead of 1,284 rows. `GET /history` gained `offset` and now pages in SQL
  rather than filtering a fixed 200 in Go.
- [x] **The page.** Active work always whole and never paged; Finished and
  Failed collapsed to a count, opened deliberately, 25 rows at a time. Title
  filter across all groups. **Clear finished** and **Clear failed** (rows only,
  both confirmed) plus the existing per-row Remove as a dismiss.
- [x] Four e2e specs cover the page: three follow imported rows behind the
  collapsed section, and one pins the confirmed Clear failed action to the
  Failed grouping without disturbing Clear finished.

## The re-grab loop — monarr's half ✅ (fixed 2026-07-31, v0.14.0)

Field report 2026-07-31: a terabyte of duplicate downloads. Diagnosed live
against nzbd (`nzbd/docs/REGRAB_LOOP_PLAN.md`); four defects feeding one
loop, of which these four are monarr's (M1–M4 of that plan).

- [x] **M1 — an import that runs out of disk fails, visibly, and finishes
  itself later.** Per-file copy errors were recorded and stepped over, and
  an import that placed *some* files reported success — so a season pack
  imported its first half, the second half stayed listed as missing, the
  payload was cleaned up, and the next backlog pass grabbed the same pack
  again. Now an environmental failure (ENOSPC/EDQUOT, read-only or vanished
  mount) stops the import at the first occurrence, marks the download
  failed with `import stopped: N of M files placed`, leaves the payload
  alone, and a `downloads.import-retry` sweep finishes it once there is
  room — every 15 min, six attempts, then it waits for a person. Only
  environmental failures retry; a payload monarr cannot parse fails the
  same way forever.
- [x] **M2 — cleanup no longer takes the client's word for it.** When the
  download client will not or cannot delete an imported payload, monarr
  removes the completed folder itself — the folder it just imported from —
  and records it (`payload_removed`). Guarded: only the directory the
  client reported, only when "clean up after import" is on for that client,
  and never at, above, or inside a root folder.
- [x] **M3 — the same release is not grabbed twice, and replacement is
  bounded.** `Grab` now refuses a release already in flight (titles
  compared with separators folded) instead of relying on the in-flight
  filter at the automation entry points, which a manual grab, an overlapping
  pass, and an instant post-failure re-search all walked straight past. And
  the automatic replacement search stops after three failed grabs for one
  want inside twelve hours, recording `regrab_capped` — five releases of one
  movie in ten hours was the field shape.
- [x] **M4 — the pipeline is readable.** `history_events` had been written
  since phase 2 and read by nothing: `ListHistory` had no caller outside its
  own package. `GET /api/v1/history?mediaItemId=&limit=` now serves the
  per-release timeline — grab → client outcome → import result, plus the new
  `import_blocked` / `import_retried` / `regrab_capped` events. (UI panel
  not built yet; the data is now reachable.)

## Defects the coverage work found ✅ (fixed 2026-07-29, v0.12.0)

Writing tests for the handlers that had never been reachable turned up six real
defects, not one of them theoretical. Each has a regression test in
`internal/api/defects_test.go`, and each of those was checked to actually fail
with its fix reverted — a test that passes either way is not a regression test.

- [x] **A permanent auth lockout.** Enabling authentication checked for a
      username and never looked at the password, but `Login` refuses on
      `wantUser == "" || wantHash == ""`. A username with an empty password
      therefore turned auth ON and then answered every login attempt with "no
      credentials configured" — forever. On an install with no API key set, the
      only way back in was editing `app_meta` by hand. The guard now reads both
      settings, spelled the way `Login` spells them, so the two cannot drift
- [x] **The mass editor counted rows it had not changed.** `UPDATE … WHERE
      id = ?` succeeds against nothing, so `POST /library/bulk` answered
      `{"updated": 1}` for a library of none. The query is `:execrows` now and
      the store reports `ErrNotFound`
- [x] **Three refusals answered 500.** A relative manual-entry path, an unknown
      root-folder kind, and a blank manual title (that one wrapped in
      `ErrNotFound`, so it answered 404 — "the endpoint does not exist" for a
      short body). None carried a sentinel, so `libraryErr` reported a server
      fault for a typo. They are one fact about the caller, so they share one
      new sentinel, `library.ErrInvalidInput`, matched once
- [x] **Deleting a profile some kind defaults to answered 500.** The same class
      of refusal as deleting one still in use — a live reference the caller must
      clear first — but only `ErrProfileInUse` was mapped, so an actionable
      message arrived behind a status code that said the server broke.
      `ErrProfileIsDefault` now joins it as a 409
- [x] **Import lists never validated their type.** oapi-codegen already
      generates `Valid()` from the spec's enum and five sibling creates call it;
      this one did not, so a list of a type no syncer knows about stored
      happily, appeared in the UI, and silently never synced — `syncOne` answers
      "unknown list type" into a log nobody reads
- [x] **DELETE of an absent id answered 204 on five endpoints and 404 on
      seven**, and the spec documented 404. The five were both inconsistent and
      untrue: they reported removing something that was never there. All five
      queries are `:execrows` now, the spec documents their 404, and the compat
      personality's own "indexer not found" branch — written from the start and
      until now unreachable, because the store made delete a silent no-op —
      finally fires
- [x] **`make coverage` reported a number that depended on build-cache state.**
      It did not pass `-count=1`. `go test` caches coverage profiles, and with
      `-coverpkg=./...` a cached package's profile still carries blocks for every
      *other* package, measured against that package's source as it was when the
      entry was cached. Edit a file, re-run, and the merge sees two disjoint sets
      of line ranges for it: the denominator inflates and the percentage
      collapses. It read 71.9% against a true 86.5% — caught because the drop was
      too large to believe, confirmed by measuring `internal/api` alone (1,599
      statements versus 2,736 in the poisoned profile). CI was always immune (a
      fresh checkout has no cache); a laptop was not, and a gate that reports a
      cache-dependent number is worse than no gate
- [ ] Left deliberately: an `internal/infra/jobs` backoff that overflows at
      absurd attempt counts. Latent — unreachable in practice — and it needs its
      own decision about what the ceiling should be rather than a quick clamp

## Auto Search did nothing ✅ (fixed 2026-07-30)

Reported from the real deployment: pressing **Auto search** on a movie appeared
to do nothing, twice; interactive search on the same movie then listed copies,
and grabbing one from that list started downloading immediately. So releases
existed, the profile would take one, and the automatic path still declined to
act. Two defects — one causing it, one hiding it.

- [x] **A terminal download row counted as "in flight" forever.**
      `notInFlight` — the filter every search path runs its wantables
      through — read `ListInFlightDownloads`, which was
      `SELECT * FROM downloads WHERE state != 'imported'`. That is every row
      that ever failed. Three paths park a row at `failed` and none of them
      ever move it out: `handleFailure` (the client said the download broke),
      `handleRemoved` (the user deleted the job in their client), and a failed
      import handoff. Only the user deleting the row from the queue clears it.
      So a single failure, at any point in the past, made that wantable
      invisible to Auto Search, the 15-minute RSS sync, **and** the 12-hour
      backlog pass — permanently and silently. `handleRemoved`'s own comment
      promises the opposite ("the want stays wanted… the next deliberate search
      still finds it"); it did not. The suppression was redundant besides: a
      release that fails is blocklisted, which is what stops us re-grabbing
      *that* copy, while retiring the whole wantable stopped us grabbing a
      different one — the exact thing that should happen next. `notInFlight`
      now reads `ListActiveDownloads` (grabbed · downloading · downloaded ·
      awaiting_import · importing), so only work that is still moving suppresses
- [x] **The endpoint could not report any of this.** `POST
      /library/{id}/autosearch` answered a bare `202` and ran the search in a
      goroutine, so the UI printed "Searching in the background — grabs appear
      under Activity" whether the search grabbed a remux, declined everything
      it found, or never ran at all. That is why a total no-op went unnoticed
      for as long as it did. It is synchronous now and returns
      `AutoSearchResult`: every target, searched or skipped-and-why, with the
      funnel that makes the outcome diagnosable — releases **seen**, of those
      **matched** to this target, of those **accepted** by the profile.
      Whichever number went to zero first is the answer, and "no releases came
      back", "none of them were this film" and "the profile turned all of them
      down" are now three different sentences. Interactive search already
      blocked on the same fan-out against the same indexers, so there was never
      a latency argument for the 202 — only the assumption that there was
      nothing worth saying. Search-on-add stays fire-and-forget: nobody is
      waiting on that one
- [x] The indexer fan-out in `searchAndGrabBest` is concurrent now, as it long
      has been in interactive search. Serial was tolerable when only the
      nightly backlog called it; with a person waiting and a 30 s per-call
      bound, four indexers meant a two-minute worst case
- [x] Regressions in `internal/app/acquisition/autosearchreport_test.go` (a
      failed download no longer suppresses; an *active* one still does) and
      `web/src/autosearch.test.ts` (the three no-grab outcomes must read
      differently)

## Decisions to date

| ADR | Decision |
|---|---|
| [0001](docs/adr/0001-name-and-license.md) | Name **Monarr**, **GPL-3.0**, port **7676**; amended twice: images on ghcr.io, org/module now `pjunod/monarr` |
| [0002](docs/adr/0002-single-media-table.md) | One `media_items` table + `kind`; files↔episodes n:m; Wantable abstraction |
| [0003](docs/adr/0003-compat-personalities.md) | v3 compat as `/sonarr` + `/radarr` URL-base personalities, translation-only |
| [0004](docs/adr/0004-sqlite-only.md) | SQLite only (modernc, WAL, single writer); no Postgres |
| [0005](docs/adr/0005-filesystem-adoption.md) | Migration by filesystem adoption; *arr DB import is a stretch item |
| [0006](docs/adr/0006-books-third-media-kind.md) | Books as a third kind — ebooks + audiobooks, Phase 2.5 |
| [0007](docs/adr/0007-remote-database-postgres.md) | Postgres for cluster HA evaluated, deferred with revisit triggers — superseded by 0008 |
| [0008](docs/adr/0008-distributed-execution.md) | Multi-host execution: leased job queue first (on SQLite), Postgres for clustered mode, mount-aware routing |
| [0009](docs/adr/0009-root-folder-kinds.md) | Root folders carry a media kind; mixed roots always ask |
| [0010](docs/adr/0010-scan-adopts.md) | Scan proposes matches; adoption is bulk work with a confidence bar, not a to-do list |
| [0011](docs/adr/0011-series-metadata-provider.md) | Series metadata is a chain: TheTVDB when keyed, TVmaze free, TMDB always |
| [0012](docs/adr/0012-manual-entries.md) | Manual entries: a library record no provider backs, episodes read off the disk |
| [0013](docs/adr/0013-measured-quality.md) | **Proposed:** on-disk quality is measured (native probe); filename demoted to hint; unknown ≠ missing; don't churn |
| [0014](docs/adr/0014-target-profiles.md) | **Proposed:** a profile is a floor + target + upgrades switch; grabs capped at target resolution; "Any" retired |
| [0015](docs/adr/0015-discovery.md) | Discovery is a read-only browse surface: TMDB always, Trakt when keyed, nothing stored |
| [0016](docs/adr/0016-air-times.md) | Calendar schedule enrichment carries exact UTC air times where providers know them |
| [0017](docs/adr/0017-audiobook-curation.md) | Audiobook format routing and multipart import; profile-derived identity superseded by ADR 0018 |
| [0018](docs/adr/0018-side-by-side-book-editions.md) | Ebook and audiobook are persistent, independently curated editions of one Open Library work |

## Milestone commits

```
84e29a7..HEAD     Phase 6 (5 commits): native mediainfo prober + corpus/fuzz ·
                  migration 0018 + probe jobs · import measures what it
                  placed · target profiles + migration 0019 + churn fix ·
                  profile CRUD/editor + provenance UI
70d3ba9..84e29a7  Phase 5 (2 commits): custom formats · transmission/deluge/
                  nzbget · import lists · anime absolute · auth + sessions ·
                  mass editor · /metrics
1b8671b           Phase 4: /sonarr + /radarr v3 personalities, X-Api-Key,
                  conformance harness + fake-consumer suite
3037805..21662ec  Phase 3 (3 commits): wanted index · rss/backlog loops ·
                  blocklist + auto re-search · calendar · notifiers · backups
b661b2e..bde394d  Phase 2.5 (3 commits): book domain · Open Library adapter ·
                  books API/UI/e2e (ADR 0006)
16fea93..6b35b62  Phase 2 (8 commits): parser+corpus+fuzz · wantables/matcher/
                  decision · schema · torznab/qbit/sab adapters · services ·
                  API · UI · full-loop e2e (movie + season pack)
192ba45..743bb93  Phase 1 (6 commits): schema+storage · TMDB port/adapter ·
                  library service · API+wiring · UI · e2e flow suite
8bd785a  STATUS.md work ledger        b3c9af1  hooks degrade gracefully
0da9a51  test pyramid + hooks + CI    ce6b3d9  images via GHCR
f13d70d  module → monarr-media        1261c78  ADR 0006: books
154b461  Phase 0: walking skeleton
```
