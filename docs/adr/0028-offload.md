# ADR 0028 — Offload: free the space, keep the title

**Status:** accepted (curator 0.48.0) · **Date:** 2026-11-12 ·
**Contract:** [offload](../PIPELINE_CONTRACT.md#45-offload--offloaded_paths-the-offload-event-and-get-it-back) (sections 4.5 to 4.7, 6)

**Read with:** [ADR 0027](0027-rematch-file-moves.md) (the operation, worker,
outbox and lease patterns this reuses) · [ADR 0024](0024-season-acquisition.md)
(season packs) · [ADR 0026](0026-pipeline-contract-v2.md) (outbox).

## Context

A show you finished watching, or a film you will not watch again, should stop
taking space without disappearing. Removing it from the library loses the
title, its watch history in Cinema and the memory of what was blocklisted;
unmonitoring it leaves the files. Nothing in Monarr says "the files are gone
on purpose, keep everything else, and let me get it back".

## Decision

1. **One stored state, `offloaded_at`.** On the rows that hold `monitored`:
   the movie item, a season and an episode (migration `0081`). It is its own
   state, not just "unmonitored": it is folded into the single `Mon`
   computation (`targetCopy`, `wantedEpisodes`, `buildWanted`, season
   discovery), so no search, RSS pass, upgrade or season pack ever grabs it.
   Offloading an item covers every copy.
2. **The way back is Re-acquire, never a tick.** Ticking *Monitored* on an
   offloaded row is refused (`409 offloaded`); the UI offers Re-acquire.
   Re-acquire(scope) clears whatever is offloaded inside the scope, monitors
   it, cancels the Offload's still-unconfirmed Cinema requests and searches at
   the current profile (a season pack-first, as ADR 0024). It answers
   `searching` at once; the search runs as admitted background work.
3. **Triggers, not code paths, clear it on a file.** A file attached to an
   offloaded wantable (`media_files` insert, a file's `media_item_id` moved,
   `media_file_episodes` insert) clears the flag in the store, so every attach
   path (import, manual import, Re-match, scan) is covered. A season's flag
   clears only when no episode of it is still offloaded. An episode inserted
   into an offloaded season inherits the flag. A whole-show Offload sets every
   season (specials included); a season that appears later on refresh arrives
   **unmonitored, not offloaded**.
4. **A multi-episode file widens the scope.** Offloading one episode of
   `S01E01-E02` offloads both, and the preview says so (`widened`).
5. **A durable operation beside Re-match's** (`offload_operations`,
   `offload_operation_files`), driven by the same recurring worker:
   - *Preview* has no side effects: every file including sidecars and
     untracked subtitles, bytes deleted and bytes freed (a file with another
     hard link frees nothing yet), the library volume's free space before and
     after, blockers, and a Cinema line (advisory).
   - *Start* is one transaction: the scope and file list, the offloaded flags
     and the operation-keyed Cinema sub-deliveries `op/<id>/<dir-hash>`
     (event `offload`, every chunk `probe_only`). The sub-deliveries are
     written **held** and released when the last file is gone, so Cinema never
     reads "still exists" for a file we have not yet deleted.
   - *Then the deletes*, resumable after a crash: unlink first, then the row
     (media before sidecars). The item folder is kept.
   - *Fences:* the per-copy acquisition reservations (so Re-acquire and
     Offload serialise), the title lease, and in-flight download, import,
     re-match and replacement. A show's season or episode scope is blocked by
     a download only when its reserved episodes overlap. An open Offload keeps
     library scans and Re-match away from the title.
6. **An unmounted share is a wait, not a delete.** Before believing `ENOENT`
   the worker confirms every root is listable and non-empty; otherwise the
   operation is `waiting` with a plain reason and nothing is marked deleted.
   An unlink that fails is `retrying` with the file and the reason, retried on
   the worker's tick. **Stop** cancels the remaining deletes: what is gone
   stays gone, a wantable that still has its files stops being offloaded, and
   Cinema is told only the deleted paths.
7. **Cinema, advisory.** Cinema advertises `offload` in its features. Curator
   sends `offloaded_paths` only when the stored features include it (features
   are re-read before the send). Without it a cinema that has `removed_paths`
   would drop the title and its watch history, so curator sends plain
   `removed_paths` and the dialog says so *beforehand*; with neither feature it
   sends nothing. Never a gate: the Dev tab shows what each notifier
   advertises and when it was read, and nothing is switched off by it.
8. **Re-acquire from Cinema.** Cinema's *Get it back* calls the plurx webhook
   with `event: "reacquire"` (kinds movie, show, season, episode; ids only;
   idempotent), answered by the same service. `reacquire_status` reads where
   it stands. This is the one acting event the webhook has; it only ever
   brings files back.
9. **First season only.** A new monitor mode `first_season`: `MonitorSeason`
   is the lowest season above 0 (or 1), `monitorNewSeason` is
   `number == MonitorSeason`. A later season arrives unmonitored and its
   action is "Monitor and search", not Re-acquire.

## Consequences

- Space comes back and the title, profile, copies, history, blocklist and
  Cinema's watch state stay. History records `offloaded` (files, bytes) and
  `reacquired`.
- Deletes are deletes: there is no trash. The dialog says so and asks for a
  tick before **Offload** enables.
- A library list gains **On disk · Offloaded · Everything** (default
  Everything, remembered per viewer); the monitored-aired counts exclude
  offloaded episodes, so a finished show does not read as incomplete.
- Cost: three columns, four triggers and two tables; the Mon computation has
  one more term in each of its three places, and a test per place.
