# ADR 0027 — Re-match: move files to the right item or episode

**Status:** accepted (curator 0.48.0) · **Date:** 2026-11-11 ·
**Contract:** [amendment: removed paths](../PIPELINE_CONTRACT.md#44-moves--removed_paths-gone-operation-keyed-sub-deliveries-amendment-2026-11-11) (sections 4.4, 5.3, 6)

**Read with:** [ADR 0020](0020-one-folder-one-item.md) (one folder, one item;
`{tmdb-N}`) · [ADR 0021](0021-file-lifecycle-recovery.md) (placement journal,
per-title revision lease) · [ADR 0025](0025-import-cancellation-and-deployment-drain.md)
(cancel) · [ADR 0026](0026-pipeline-contract-v2.md) (outbox, replacement grabs).

## Context

A folder adopted as the wrong title (Dune 1984 instead of Dune 2021), or one
episode filed under the wrong episode or show, can be fixed today only by
removing the file from the library and importing it again. Placement, the
journal and the outbox already exist; there is no operation that moves a
file between items, and Cinema cannot be told that a path went away
(a scan of a missing folder is `path_unreadable`, retried forever).

## Decision

1. **One primitive: move files to a target.** `Rematch{source files[],
   target item, per-file target episodes/edition}`. The target is an
   existing library item, or a new one added through the normal add path
   (default profile and root of the right kind) inside the operation.
   *Re-match…* on the item page is all of the item's files; *Move to…* in the
   Files panel is one or more files. Same operation, same code.
2. **Source item rule.** The item the files leave keeps its identity and
   history. It stays, wanted, if the user added it. It is removed only if
   folder adoption created it and it now has no files, copies with files,
   downloads or open operations. Needs `media_items.origin`
   (`user | adopted`; backfill `user`, so legacy rows are never auto-removed).
   The preview states which. A removed item's folder is `rmdir`ed only if
   empty; anything else is left and listed. Its history rows stay (ids are
   audit references).
3. **New journaled placement kind.** `internal/app/acquisition/rematch.go`
   beside `placement.go` and `removefile.go`. Rows in `import_placements`
   with `kind: rematch_move` (ADR 0021 journal; states `rematch_planned → rematch_moved → rematch_committed → rematch_done`, never the import's `prepared|committed|retained`, so no import path reads them) plus a `rematch_operations`
   row (operation id, request id + fingerprint like `CreateReplacement`,
   state, per-file result). States: `planned → moving → moved →
   committed → complete`, or `cancel_pending → cancelled | failed`
   (cinema's confirmation is the sub-deliveries' own `visible_at`). Journal before
   the filesystem step; one SQLite transaction commits file rows and
   file-episode links to the target, writes history, the blocklist entry
   (item 7) and the outbox rows (item 9), and bumps `lifecycle_item_revision`
   for both titles. `reconcilePlacements` resumes by observed state: source
   present and destination absent, redo; destination present with matching
   identity/digest and source gone, commit; both present (copy), verify
   digest then finish. Resume never moves a file whose source identity
   changed (it fails the file, `review`).
4. **Fencing.** Take the title lease on both items, in ascending id order
   (`lockImportTargets(ids…)`; `heldImportTarget` becomes a set; the
   "no path takes two item locks" rule is amended to "two only through this
   helper, sorted, bounded wait"). Preview records both
   `lifecycle_item_revision`s; execution refuses a stale pair
   (`409 stale`, re-preview). Refused with `409 rematch_blocked {reason,
   ref, retry}` while either item has a download in a non-terminal state,
   a running import attempt, or a replacement operation not in
   `complete|cancelled|failed` on a source wantable. The message names
   which, and the UI offers "wait for it" (retries the preview).
5. **Rename or verified copy.** Same filesystem (storage identity of source
   and destination parents): `rename`, crash-safe via the journal. Otherwise
   copy to a temp name in the destination folder, fsync, verify SHA-256 and
   size against the source, publish by rename, commit, then unlink the
   source. Bytes copied appear in the item status panel (import-activity
   row); **Stop** is the existing cancel-import (ADR 0025): the temp file is
   removed, the source is untouched, the operation is `cancelled`. Capacity
   check as for recovery (size + 1 GiB). Never overwrite: a destination that
   exists is a preview conflict (picker or left out).
6. **Names and folder.** The preview lists every `from → to` before anything
   happens; destination names come from the target's naming rules. A target
   folder chosen by this operation always carries the `{tmdb-N}` hint
   (`{tvdb-N}`, `{imdb-ttN}`, `{olid-…}`, `{monarr-<id>}` for items no
   provider backs), whether or not another item holds the plain name, so the
   next scan or Cinema match keeps the pairing (ADR 0020, item 3). Sidecar
   files with the media file's stem travel with it.
7. **Blocklist the vacated wantable.** For each moved file with a stable
   source (`source_guid` + `source_indexer_id`) the release is blocklisted
   for the source item/episodes through the replacement-operation path: a
   `replacement_operations` row, intent `rematch`, scope `episodes` (movies
   and books: `download`), `blacklist_source_release=1`, created already
   `complete` in the commit transaction, writing `release_blacklist_identity`.
   No search, no removal step; the migration widens the `intent` CHECK. The
   preview shows the releases and a checkbox, default on. Files without a
   stable source (adopted) have nothing to blocklist and the preview says so.
8. **Shapes.** *Multi-episode files* move whole and map to every episode they
   covered (n:m, ADR 0002); a target missing any of those episodes puts the
   file in the picker or leaves it out; it is never split. Files that do not
   fit the new title are listed with a picker; until each is picked or
   explicitly left out nothing moves. Only an explicitly left-out file stays
   on disk and is removed from the library (history records it).
   *Books:* a move is between editions of the same medium (ADR 0018); the
   parts of a multipart audiobook move as one unit; work/edition ids go to
   Cinema. *Manual entries* (ADR 0012) may be source or target; nothing here
   needs a provider.
9. **History and Cinema.** A `rematched` history event on both items:
   operation id, from/to item, every `{from, to, episodes, mode}`, blocklisted
   releases, left-out files. In the same transaction, per notifier, write
   operation-keyed sub-deliveries `op/<operation-id>/<dir-hash>`, event
   `moved`, one per library root touched: `path` = common parent, `expected_files`
   = placed files, `removed_paths` = files (and the folder, if removed) that
   left (contract amendment). Done when Cinema reports every expected file
   `placed` and every removed path `gone` or `unknown` (or a lasting warning, see below). The panel shows
   "moved, cinema confirming" then "in cinema" from `visible_at`. Chat-shaped
   notifiers receive one `moved` message.
10. **API.** `POST /library/{id}/rematch/preview`, `POST /library/{id}/rematch`
    (idempotent by `requestId`), `GET /rematch/{operationId}`, cancel via
    import cancel. OpenAPI, `docs/usage.md`, `docs/integration.md` updated.

## Consequences

- No new background worker: the placement reconciler and the outbox
  dispatcher drive resume and delivery.
- A rematch against an older Cinema moves files and confirms the new folder,
  but cannot remove the old entry (amendment, compatibility); the preview
  says so beforehand. Nothing is gated; the Developer tab shows the
  Cinema capability as advisory.
- The migration adds `media_items.origin`, rebuilds `replacement_operations`
  for the `rematch` intent, and adds `rematch_operations` (next free number
  after every branch's claim: 0080, with 0081 for dismissed rows).
- A rename is atomic per file, not per operation; a crash mid-operation
  leaves some files moved and the journal resumes the rest.
- ADR 0020's "existing folders are never renamed" holds: the vacated folder
  is never renamed, only emptied.

## Decisions taken (2026-11-11)

- `media_items.origin` (`user | adopted`), existing rows backfilled `user`,
  never auto-removed. Folder adoption marks the items it creates `adopted`.
- Re-match across two library roots is allowed: one Cinema request per root.
- `replacement_operations` gains intent `rematch`, recorded `complete`
  (blocklist only; no search, no removal step).
- Two title leases only through `lockImportTargets`: sorted ids, bounded
  wait per title, all or none.
- Cinema capability `features: ["removed_paths"]` from the connection test;
  the per-answer check is the backstop. Cinema deletes an item whose last
  file is gone.

## Implementation notes (0.48.0)

- **Same kind only.** A movie's files move to a movie, a series' to a
  series, a book's to an edition of the same medium.
- **Into an existing item: import's rule.** A moved file that meets a file
  the target already has (the same copy; for a series, the same episodes)
  replaces it when it ranks higher in the target's profile and its language
  is acceptable — `PlanItemImport`'s test — and is kept beside it otherwise;
  the preview says which and why. A replacement is journaled on the move
  (`replace`, copy, episodes) and runs after the commit through import's
  `removeExistingFiles` (bytes, row, placed subtitles), keeping every file
  the operation placed; a restart before the move is `rematch_done` repeats
  it. The replaced paths are among the sub-delivery's `removed_paths`.
  "Never overwrite" still holds: a file at the destination path itself is a
  preview conflict.
- **A removed source blocklists nothing.** Blocklist rows cascade with
  their item; when the source is removed the preview says there is nothing
  to blocklist.
- **A new title is added by the API before execution** (default profile, the
  source's root when it takes the kind, else the first root that does), then
  the move runs against it; the preview fingerprint check refuses the move if
  what was added is not what was previewed. If execution then refuses
  (blocked, stale), the added title stays in the library as a user-added,
  wanted item.
- **Stop** is `POST /rematch/{id}/cancel` (the operation is not an import
  attempt, so the import cancel endpoint does not address it); it is
  accepted until commit. Already-moved files are put back (renames reversed,
  copies' destinations removed); sources are never touched.
- **Removed paths are files only.** The vacated folder is removed after the
  commit only when it is empty; it is not named in `removed_paths`, so a
  folder that keeps an unrelated file cannot hold the sub-delivery at
  `present` forever.
- **The request** names the event as `"event": "moved"`; `source` stays
  `monarr`.
- **Driven until terminal.** Open operations are driven by a worker
  started with the importers (immediately, then every 30 s), admitted as
  work so a deployment drain sees them; titles busy with an import are
  waited for and the operation says so. An operation completes only when
  every move is `rematch_done` (copied originals removed, replaced files
  gone); a rollback that cannot put a file back stays `cancel_pending`,
  naming what is where. Either is retried on every pass. Finished
  operations' journal rows are pruned.
- **Scans.** A library scan skips the titles of an open operation and the
  folder it chose.
- **Cinema requests.** Expected files are split across requests like
  removed paths (≤ 64 and ≤ 16 KiB each), so every file is probed. Whether a
  request carried `removed_paths` is stored on the row
  (`notifier_deliveries.removed_sent`). Cinema's features are re-read before
  a moved request unless read in the last 10 minutes; a stored
  `removed_paths` that cannot be confirmed holds the request (retried), never
  a common-parent request on a stale feature.
- **`probe_only` on every chunk.** Each moved request, including a chunk of
  only expected files, sends `"probe_only": true`, so cinema never walks the
  common parent. Deploy order: every cinema node is upgraded before curator
  sends `removed_paths`.
- **Cinema's answer per removed path.** `gone` and `unknown` satisfy. `retained`
  carries a `code`: `expected_not_placed`, `prune_bound` and `conversion_hold`
  are transient (the row warns and retries on the delivery schedule);
  `root_identity_unset` and `root_identity_mismatch` are lasting (`cinema kept
  <path>: <reason>; it will drop it at its next full scan`, not retried).
  `present` carries a `reason` (`exists`, `symlink_in_chain`,
  `case_variant` are lasting warnings; `reappeared_before_removal` retries).
  `record_kept` is a lasting warning in cinema's words. Lasting warnings ride on the
  visible row and show in the operation's cinema line.
- **Replacements cannot be cancelled.** A replacement waiting for a release
  or blocked on cleanup holds the title; the blocker says so instead of
  promising that waiting ends it.
- **Following a running move.** `GET /library/{id}/rematch` is the item's
  open operation; the dialog follows it (progress, Stop) when reopened, and
  a preview blocked by another move offers *Show its progress*.
- **Mobile** has no per-file Files panel; its Re-match sheet chooses all
  files or one file (Move to…).
- **Status panel.** The operation's bytes, Stop and Cinema confirmation
  show in the Re-match dialog and `GET /rematch/{id}`; wiring them into the
  item status panel (0.46.0) lands with the rebase onto it.
