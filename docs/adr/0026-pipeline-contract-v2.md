# ADR 0026 — Pipeline contract v2: Runner facts, provisional terminals, publications

**Status:** accepted (curator M2, v0.39.0) · **Date:** 2026-11-07 ·
**Supersedes:** the "Durable job control contract (v1)" section of
[integration.md](../integration.md), the Delivery v1 acknowledgement protocol,
and the per-row admission-receipt pacing · **Contract:**
[PIPELINE_CONTRACT.md](../PIPELINE_CONTRACT.md) section 3

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
