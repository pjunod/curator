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
   through their `.runner-manifest.json` (until M6b also through the v1
   adapter: Runner's v1 delivery record) with a per-file outcome (`placed`, `discarded{superseded,
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

## Addendum — M6b, the v1 strings and the v1 manifest source go (v0.43.0)

17. **Curator reads Runner's typed terminal only.** The `translate()` branch for
    `job_pp_finished`, the ordered history reader and the receipt lookup no
    longer fall back to the v1 `pp_status` / `final_dir` strings
    (`ports.LegacyTerminal` is deleted); a row without `outcome`, or a
    completed row without its publication, is not a terminal. A `/deliveries`
    row with no `kind` is a completed terminal.
18. **The `v1_record` manifest source is retired.** A publication's manifest is
    its `.runner-manifest.json`. Migration 0075 refuses while a publication
    that is not `removed` still has `manifest_source = 'v1_record'`, then
    normalises the removed ones. It is merged only after Runner's M6b is
    deployed and the M7 review has resolved the historical cohort.
19. **Not done in M6b**: dropping `runner_delivery_receipts` and
    `runner_delivery_file_outcomes`. The receipts lifecycle
    (`/downloads/deliveries*`, the Activity delivery panel, the joined
    timeline, Completed folders) still reads them and has no replacement in
    the plan; dropping the tables is a decision to retire that screen, left
    for Paul.

