# Monarr — Project Status

## Failed downloads no longer freeze behind Runner — 2026-11-02 · v0.38.12

**Status:** in progress on `codex/runner-failure-handoff` with the paired
Runner change (pjunod/nzbd `codex/empty-failed-source`).

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

## Phase 7 — The nzbd integration (plan: `docs/plan-integration.md`)

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
- [x] Decisions recorded in `docs/plan-integration.md` §11.1: per-user with
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
