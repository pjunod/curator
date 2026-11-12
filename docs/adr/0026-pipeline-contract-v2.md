# ADR 0026 — Pipeline contract v2: Runner facts, provisional terminals, publications

**Status:** accepted (curator M2, v0.39.0) · **Date:** 2026-11-07 ·
**Supersedes:** the "Durable job control contract (v1)" section of
[integration.md](../integration.md), the Delivery v1 acknowledgement protocol,
and the per-row admission-receipt pacing · **Contract:**
[PIPELINE_CONTRACT.md](../PIPELINE_CONTRACT.md) section 3

**Read with:** [integration.md](../integration.md) (the operator reference:
facts and terminals are section 4, publications and cleanup section 5,
Cinema section 6) · [ADR 0021](0021-file-lifecycle-recovery.md) (partly
superseded by this record) · [ADR 0025](0025-import-cancellation-and-deployment-drain.md)
(import cancellation, unchanged) · the superseded plans are kept in
[../archive/](../archive/) with a pointer to this record.

## Context

Curator decided a Runner download's fate from three overlapping sources (the
queue poll, the last hundred history rows, and a per-row request-receipt
lookup paced by its own backoff table), each guarded by its own fence: the
control refusal matrix, the `DeliveryExpected` import gate, custody holds
that waited on Runner's cleanup before re-grabbing, and eight receipt-path
parks. Imports of Runner payloads went through a lease/verify/ack/import
cycle that acknowledged the delivery before placing its files. A failure's
next grab ran inline in `handleFailure`, after a chain of writes none of which
shared a transaction. The audit traced re-grab loops, one-way parks and lost
grabs to exactly those seams.

## Decision

1. **One reducer.** `observe(dl, fact)` is the only function that changes a
   Runner row from an observation. Facts (`admitted`, `status`, `terminal`,
   `publication_ready`, `gone`) come from the event stream and from the
   poll's three reads (queue, ordered history, deliveries feed) and are
   reduced the same way. A client is on the reducer by capability
   (`ports.RunnerFeeds`); every other client keeps the established path,
   with its failures recorded the same way (item 4).
2. **Identity is the transfer token.** A fact whose token is not the row's is
   dropped and counted (`monarr_runner_fact_mismatch_total`).
3. **Once, durably.** The terminal is `downloads.runner_outcome`, written by
   one guarded UPDATE (`runner_outcome = '' OR provisional = 1`); in the same
   transaction the work it implies is written as a uniquely keyed row — a
   `publications` row for completed, a `replacement_grabs` row for a failure
   that re-grabs — together with the blocklist entry and the history row.
   A trigger refuses any rewrite of a non-provisional outcome.
4. **Failure is the next grab.** The replacement grab worker executes
   `replacement_grabs` rows idempotently: movies and books are searched;
   episodes are searched for a single-episode release, never a pack; only an
   episode with no single release goes to the season planner. Only
   `blames_release` failures blocklist and spend fetched bytes against the
   budget; cancelled, lost and an indexer outage never re-grab.
5. **Curator's own terminals are provisional** (`submit`, `lost`, and the
   rows migration 0072 converted from parks). Any later Runner fact re-opens
   them. Curator never authors `hold_expired`: a hold past Runner's deadline
   plus five minutes is `held (stale)` and is reconciled, not failed.
6. **The ordered history reader owns the cursor** (`runner_history_cursors`:
   incarnation, since_seq). Events apply their terminal at once and never
   move it; a changed incarnation or a pruned gap looks every outstanding
   transfer up by its token.
7. **Publications are the import unit.** One per Runner generation, imported
   through the v1 adapter (`.runner-manifest.json`, else the v1 delivery
   record) with a per-file outcome (`placed`, `discarded{superseded,
   duplicate, rejected}`, `unresolved`). Destination conflicts are decided by
   `wantable_attempts` — the download Curator last chose for each wantable,
   recorded by triggers on every insert — never by generation ids or time;
   ambiguity is `unresolved`, retained and shown in Resolve.
8. **The historical cohort is frozen at cutover.** Every v1 receipt and every
   completed directory present at the first M2 start is a `historical`
   publication; nothing deletes it automatically until the M7 review sets
   `cleanup_approved_at`.

## Consequences

- Deleted: the v1 import gate and the receipt lease/verify/ack/import
  scheduling; automatic receipt-bound cleanup scheduling; the
  admission-receipt poll pacing (`runner_admission_poll_schedule` is unused
  and dropped in M4); every receipt-path park and both removal parks; the
  control refusal matrix (`storeControl` keeps the newest control for
  display); the custody wait in the replacement worker; the history
  `limit=100` read; Curator's request for Runner to clean its source.
- A row's deadline (`deadline_at`) is written on state entry and drives the
  `pipeline:overdue` health check.
- Until M4, Curator deletes nothing from a publication: imported live
  publications stay on disk until per-file cleanup lands.
- Legacy (non-Runner) clients keep parked rows for the season planner's
  uncertain-submission fence; M4 retires the columns.

## Addendum — M4, per-file cleanup (v0.40.0, 2026-11-07)

9. **Cleanup is per file and evidence-bound** (contract section 3.4). One
   implementation (`cleanupPublication`) serves the post-import cleanup, the
   hourly sweep, Resolve and Storage discards and the receipt retry. A file
   leaves only when it is `placed` with the manifest digest recorded and the
   library copy still matching it, or `discarded`; before each unlink the
   generation, the file identity and the completed root's storage identity
   are re-checked; the directory is removed when only the manifest is left.
   Evidence that no longer holds demotes the file to `unresolved`.
10. **The historical cohort waits for its review**: automatic deletion and
    operator discards refuse it until `cleanup_approved_at` is set (the M7
    approval, `POST /downloads/publications/approve-cleanup`).
11. **The migration-0040 review cohort is folded into the historical cohort**
    and its tables dropped; the storage-change fences it carried for orphan
    paths moved to `storage_change_review_paths`. The storage-identity checks
    are unchanged on every delete path.
12. **Not dropped in M4**: `downloads.parked_reason`, `parked_at`,
    `superseded`, `delivery_expected`, `request_token_idempotent`,
    `exact_transfer_correlation`. M2 kept live code on each (season planner
    fences and operator cancellation for every client, the Runner delivery
    request parameter and native-torrent precondition, the submission retry,
    qBittorrent tag correlation); dropping them is a redesign of those paths,
    not a cleanup, and is left for Paul to schedule.

Consequences: the receipt-bound cleanup ledger, explicit deletion batches and
their abandon action are gone; Activity retention hides rather than deletes a
row whose publication still has files on disk; non-Runner clients keep their
whole-payload cleanup, which never touches a publication directory.

## Addendum — M5 and M6a, the outbox and the deletion pass (v0.41.0, v0.42.0)

13. **The outbox is the only path to Cinema** (contract section 4): one
    `notifier_deliveries` sub-delivery per (notifier, publication, directory),
    written in the import's transaction, sent with an attempt-numbered
    `request_key` and `expected_files`, and done only when Cinema reports every
    file placed with the recorded bytes and `mtime_ns`. The bus carries no
    import notification.
14. **One retry ladder helper** (`internal/domain/backoff`) spaces runner
    submission, manual admission, the blocked-import retry and the outbox;
    each caller's schedule is unchanged. It is a domain package because the
    submission deadlines are computed inside `infra/sqlite`, which
    `internal/app/pipeline` imports.
15. **`GET /system/pipeline` no longer has `measured` or `note`**: every seam
    has an end, so the field could only ever be true.
16. **The integration page was rewritten from the contract** and the
    superseded plans moved to `docs/archive/`; `docs/README.md` indexes every
    file and `internal/docs` fails the build when an index entry or a link
    goes stale.

## Addendum — no wedge: hash once, scratch stages, sidecars (v0.44.0, 2026-11-11)

Field data from nuc3 (2026-11-11): ~8 imports in 55 minutes, 12 bytes read per
byte written, 76 publications failed "existing stage retained; explicit retry
or custody review required", 10-14 minutes without imports after every
restart, and publications left in Resolve for their subtitles and `.nfo`s.

17. **Hash once.** The manifest digest is the authority for a publication's
    bytes. The placement does not read the source to learn it: the stage
    writer hashes its stream and compares it with the manifest (one read, one
    write per file); on one filesystem the stage is a hard link and nothing is
    copied. A cross-volume publication's linked file is hashed once, so
    "verified once if `cross_volume`" (contract section 2.5) still holds.
    Resume (`reuseCommittedImport`), crash recovery (`preparePlacementPublication`,
    `reconcilePlacements`) and per-file cleanup decide by the recorded
    identity — size, inode and mtime (ctime is not content: a link or chmod
    moves it) — and hash only when no record exists. Decision 9's "the
    library copy still matching it" is now "still the object the placement
    journal recorded". The verification read budget paces only background
    proofs; reads an import or operator waits on are unpaced.
18. **A stage is scratch.** A stage left by an interrupted attempt is
    reused only when the journal recorded it as fully copied and proven;
    otherwise the next attempt deletes it and copies again. This amends ADR
    0025 section 1's "held custody survives retry" for media stages of the
    same epoch: the writer that held it has returned (one import per
    download) or its process is gone. Revoked authorizations
    (`retained_for_review`) are unchanged.
19. **The stall deadline counts from the import's start** (`StartPublicationImport`),
    not discovery; queued time never fails a publication. Migration 0076
    requeues every publication that failed only for a retained stage, a
    queue-time stall or an obfuscated single-episode name.
20. **Reconciliation does not hold the library.** The placement journal is
    reconciled in the background after the workers start, each row under its
    own title's import target lock. A row whose target is not provably its
    published object stays prepared; the title's next import redoes the copy.
21. **Sidecars are decided by the import.** Subtitles are placed beside
    their video in the publication's own import (before the outbox row is
    written), named `<video stem>.<lang>[.forced][.sdh].<ext>`; every other
    non-media file is `discarded{rejected}`. Review #13's "a sidecar is never
    placed automatically" is superseded: no sidecar is `unresolved`.
    Sidecars earlier builds left unresolved in imported publications become
    pending (migration 0076) and are decided by the cleanup pass; replacing
    a video retires the subtitles Curator placed for it, by their `placed`
    record only.
22. **Obfuscated single-episode grabs map by the grab record**: one media
    file (samples excluded) and a grab for one episode name it, unless the
    release title names a different episode.
23. **Every outcome is final without a person** (adversarial review r1):
    a file the importer cannot place is `discarded{rejected}` with its
    reason; a file of another attempt is `discarded{superseded}` (no record
    at all imports by quality); a publication that gave the library nothing,
    whose files do not match the manifest (missing, resized, copied bytes
    differ), whose attempts failed on the payload five times, or that is
    still unfinished six hours after its first start ends `import_failed`
    with a replacement grab in the same write (`EndPublicationForRegrab`,
    `PublicationImportWrite.RegrabWantables`). Per-file cleanup no longer
    demotes to `unresolved`: a placed file whose library copy was replaced or
    removed is `discarded{superseded}`; one whose record remains but whose
    bytes are missing or differ stays, reported, and is checked again.
    Decision 7's "ambiguity is unresolved" and decision 9's "evidence that no
    longer holds demotes the file" are superseded; `unresolved` remains only
    for files a person left undecided in a manual or approval-held import.
24. **The link path never chmods Runner's inode** and falls back to a copy
    on any problem. A link moves the source's ctime, so this build needs the
    Runner build whose identity comparisons drop ctime (Fable plan W1).
25. **Environmental stops wait; endings keep what was placed** (review r2):
    a publication whose last stop was the environment (`last_stop =
    environment`: ErrImportBlocked, the completed root's storage identity,
    a manifest unreadable for now) is never ended by its deadline, and the
    sweep never discards pending media. Only typed payload failures
    (`errPlacementConflict`) count toward the attempt cap, spaced on
    `backoff.Retry` (`retry_at`); a malformed manifest (`errManifestInvalid`)
    or a file missing or resized against it ends at once; a file whose bytes
    do not match it (found while it is placed) is that file's outcome, the
    rest of the payload is still imported, and the publication then ends
    (v0.46.1: the result no longer depends on manifest order). A multipart
    audiobook is the exception: all its parts are verified against the
    manifest before any is placed, and one mismatch places none. Ending
    (`endPublication`) records the publication's committed placements through
    `completePublication` (history, outbox to Cinema), discards the rest and
    writes a replacement grab the worker filters by what is still wanted. A
    file of another attempt is superseded only while that attempt is live or
    its file is in the library; a person's retry resets attempts and
    deadline.
26. **Holds are visible, recorded once, and backed off** (review r3): an
    environment hold (`HoldPublicationForEnvironment`; now also a publication
    outside the configured completed roots and one whose manifest is still
    missing after 30 minutes) sets `hold_since` and `retry_at` on the retry
    ladder, records itself once (`import_held`) and its clearing once
    (`import_hold_cleared`), and restarts the deadline when the next attempt
    starts. It is exposed as `held`/`heldReason`/`heldSince` on the queue's
    publications and `heldPublications` on `GET /system/pipeline`.
    `CompletePublicationImport` completes only a publication still
    `discovered`/`importing` (`ErrPublicationMoved` otherwise), the deadline
    sweep re-reads the publication under the row's lock, and
    `endPublication` holds instead of ending when it cannot read the
    manifest or check a placement.

## Addendum — v0.45.1, nothing waits on a lost fact (claude/unwedge-legacy, 2026-11-12)

27. **Media an ended import left pending is imported, not kept and not
    discarded blindly** (amends decision 25's "the sweep never discards
    pending media"). Every v0.44 ending decides each manifest file, so
    pending media in an imported, notified or import_failed publication was
    left by an earlier build. The cleanup pass discards it only when every
    want of its download is filled in the library at its profile's standard
    (or no longer monitored) or a replacement download imported it;
    otherwise it returns the publication to `discovered` so the import
    decides the file. The requeue counts as an attempt, so the attempt cap
    bounds it: with no attempt left the media is discarded. Both writes are
    conditional on the publication being exactly as read, so a person's
    Retry import wins the race.
28. **Every waiting publication has an exit.** The hourly cleanup offers
    every `discovered`/`importing` publication to the import workers when
    its client is an enabled Runner, independently of that client's poll,
    and ends it otherwise (`endPublication`: committed placements recorded,
    the rest discarded, the want grabbed again). Migration 0077 re-decides
    the publications earlier builds failed with reasons this one no longer
    has: requeued with fresh limits where an enabled Runner client owns
    them (the import, not the download's state, decides whether each want
    is still open), ended with a replacement grab otherwise.
29. **The completed roots are read for unclaimed publications** (adoption).
    A payload directory with a valid `.runner-manifest.json` naming a Curator
    transfer token and no publication is attributed to its row exactly once,
    on Runner's word: a row whose terminal is written adopts it as a
    salvage; a row without one asks Runner's request receipt and records the
    terminal it gives (completed when it names this directory, otherwise the
    directory is a salvage), waiting while Runner is unreachable or the
    transfer is still running. A token with no row is an **orphan** only once
    its manifest is 24 h old and every enabled Runner client reports it
    unknown or ended: it is recorded as a live publication with no download,
    its files `discarded{rejected}` with the reason, and retired by the
    per-file cleanup (the attempt is gone from Curator's records and its
    want was grabbed again when it failed). A failed row whose salvage is
    pending is hidden, not deleted, by Clear failed, Remove and retention
    until Runner settles it (a receipt it no longer has, a salvage no longer
    pending, or seven days pending).
30. **A publication is one payload directory; record-only rows end.** A
    completed root, its `completed` container or an ancestor (also through
    the roots' bind-mount aliases) is refused as a publication path, fail
    closed when the roots cannot be read, and ignored as a claim. A
    record-only publication (no download row, manifest or files: the
    migration-0040 cohort and cutover-frozen directories) at such a path, or
    whose directory is gone while its storage is verified present before and
    after the check (inside a root, the root's storage identity; outside,
    the mount of a verified completed root), is deleted — freeing its path,
    with a `publication_closed` history event — and nothing on disk is
    touched. This does not change decision 10: a historical publication's
    files are deleted only after its review batch is approved. A different
    payload published at a removed publication's path is a new publication,
    never attached to the removed one.

