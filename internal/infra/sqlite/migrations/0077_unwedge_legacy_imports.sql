-- +goose Up
-- Imports an older build failed for a reason the current rules no longer
-- have are decided again (claude/unwedge-legacy, field data 2026-11-12).
--
-- Before v0.44 a publication import that could not place a file left it
-- unresolved and failed the publication "N file(s) unresolved: <file>:
-- <why>" — "superseded by download N which has not imported yet", "the file
-- is cut short", "existing destination bytes have no measured replacement
-- authorization", "no acquisition record for ..." — or "the publication
-- holds no importable media (samples and sidecars only)". v0.44 gives every
-- file a final outcome at import (placed, or discarded{superseded |
-- duplicate | rejected}) and never leaves one for a person, but these rows
-- were written before it: their unresolved (or still pending) files keep
-- the per-file cleanup from retiring the directory, and no automatic path
-- reads an import_failed row again, so they sat forever. Migration 0076
-- matched only the stage/custody and stall reasons.
--
-- Each such publication is decided by the current rules, by the import
-- itself: it goes back to discovered with fresh limits (no counted attempts,
-- no deadline until its import starts, no hold, no retry wait) and its
-- undecided files become pending. The import is what tests whether each
-- file's want is still open: a want the library already fills is a
-- duplicate (discarded{duplicate}), a file of another attempt that is live
-- or in the library is discarded{superseded}, everything else is placed or
-- rejected with its reason, and a publication that gives the library nothing
-- ends import_failed with its want grabbed again. Nothing here infers a want
-- from a download's state.
--
-- Only a publication whose download belongs to an enabled Runner client is
-- requeued: that is the client whose publications the import workers are
-- offered (the poll, and the hourly offer of every discovered publication).
-- One whose client is gone, disabled or not Runner can never be imported
-- from its client, so it ends now instead: its undecided files are
-- discarded{rejected} with that reason (the hourly sweep retires the
-- directory) and a completed publication's want is grabbed again, exactly as
-- EndPublicationForRegrab does.
--
-- Historical rows that have a download row are decided the same way (an
-- import is not a deletion: their files still wait for the review batch
-- before cleanup deletes them); historical rows with no download row are
-- record-only and end in the hourly pass. Never touched: imported, notified,
-- discovered and importing rows; a publication a person is deciding in
-- Resolve (a file left "not selected for import" by a manual import, or
-- "kept by an operator"). A publication whose files are all already placed
-- or discarded needs nothing: the hourly sweep retires it. Running this again
-- changes nothing (every matched row has left import_failed or holds no
-- undecided file).
CREATE TEMP TABLE legacy_0077 AS
SELECT p.id AS publication_id, p.download_id, p.kind, d.generation_key, d.wantables,
       EXISTS (SELECT 1 FROM download_clients c
                WHERE c.id = d.client_id AND c.enabled = 1 AND c.type = 'nzbd') AS runner
  FROM publications p JOIN downloads d ON d.id = p.download_id
 WHERE p.state = 'import_failed'
   AND (p.error GLOB '[0-9]* file(s) unresolved: *'
     OR p.error = 'the publication holds no importable media (samples and sidecars only)'
     OR EXISTS (SELECT 1 FROM publication_files f
                 WHERE f.publication_id = p.id AND f.removed_at = 0 AND f.outcome = 'unresolved'))
   AND EXISTS (SELECT 1 FROM publication_files f
                WHERE f.publication_id = p.id AND f.removed_at = 0 AND f.outcome IN ('pending', 'unresolved'))
   AND NOT EXISTS (SELECT 1 FROM publication_files f
                    WHERE f.publication_id = p.id AND f.removed_at = 0 AND f.outcome = 'unresolved'
                      AND (f.reason = 'not selected for import; place or discard it in Resolve'
                        OR f.reason = 'kept by an operator in Resolve'));

-- No client to import from: ended, its want grabbed again.
UPDATE publication_files
   SET outcome = 'discarded', discard_reason = 'rejected', library_path = '', placed_sha256 = '',
       reason = 'not imported: its download client is gone, disabled or not Runner; not kept',
       updated_at = CAST(strftime('%s', 'now') AS INTEGER) * 1000
 WHERE removed_at = 0 AND outcome IN ('pending', 'unresolved')
   AND publication_id IN (SELECT publication_id FROM legacy_0077 WHERE NOT runner);
UPDATE publications
   SET error = 'not imported: its download client is gone, disabled or not Runner',
       updated_at = CAST(strftime('%s', 'now') AS INTEGER) * 1000
 WHERE id IN (SELECT publication_id FROM legacy_0077 WHERE NOT runner);
INSERT OR IGNORE INTO replacement_grabs (download_id, wantable_ids, created_at, next_at)
SELECT download_id, wantables, CAST(strftime('%s', 'now') AS INTEGER) * 1000, CAST(strftime('%s', 'now') AS INTEGER) * 1000
  FROM legacy_0077 WHERE NOT runner AND kind = 'completed' AND json_array_length(wantables) > 0;

-- The rest are imported again. Custody an interrupted attempt left would
-- make the retry refuse silently: the stage is scratch, and the
-- authorization of a placement that never committed is disposed of, so the
-- retry starts a fresh epoch (as 0076).
UPDATE import_attempt_stages SET state = 'released'
 WHERE state = 'held' AND EXISTS (SELECT 1 FROM legacy_0077 w
   WHERE w.runner AND w.download_id = import_attempt_stages.download_id AND w.generation_key = import_attempt_stages.generation_key);
UPDATE import_publication_authorizations
   SET outcome = 'reconciled', resolution = 'operator_disposed', resolved_at = CAST(strftime('%s', 'now') AS INTEGER) * 1000
 WHERE (outcome = 'pending' OR resolution = 'retained_for_review')
   AND EXISTS (SELECT 1 FROM legacy_0077 w
   WHERE w.runner AND w.download_id = import_publication_authorizations.download_id AND w.generation_key = import_publication_authorizations.generation_key);
UPDATE publication_files
   SET outcome = 'pending', discard_reason = '', reason = '',
       updated_at = CAST(strftime('%s', 'now') AS INTEGER) * 1000
 WHERE removed_at = 0 AND outcome = 'unresolved'
   AND publication_id IN (SELECT publication_id FROM legacy_0077 WHERE runner);
UPDATE publications
   SET state = 'discovered', error = '', deadline_at = 0, import_attempts = 0,
       last_stop = '', retry_at = 0, hold_since = 0, holds = 0,
       updated_at = CAST(strftime('%s', 'now') AS INTEGER) * 1000
 WHERE id IN (SELECT publication_id FROM legacy_0077 WHERE runner);
DROP TABLE legacy_0077;

-- A removed publication's directory is gone: a file an older build left
-- unresolved or pending in it is not there to decide. It is recorded as
-- discarded and removed, so nothing lists it for a person.
UPDATE publication_files
   SET outcome = 'discarded', discard_reason = 'rejected',
       reason = 'its publication was removed before an older build decided it',
       removed_at = CAST(strftime('%s', 'now') AS INTEGER) * 1000,
       updated_at = CAST(strftime('%s', 'now') AS INTEGER) * 1000
 WHERE removed_at = 0 AND outcome IN ('pending', 'unresolved')
   AND publication_id IN (SELECT id FROM publications WHERE state = 'removed');

-- +goose Down
-- A data repair: the rows it decided are not put back into a wedge.
SELECT 1;
