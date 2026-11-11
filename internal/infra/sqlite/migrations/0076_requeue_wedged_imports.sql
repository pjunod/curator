-- +goose Up
-- Imports that failed only because Curator wedged itself are retried
-- automatically (claude/no-wedge, field data 2026-11-11).
--
-- Until this build a stage left by an interrupted import (a restart mid-copy)
-- failed every later attempt of the same placement with "existing stage
-- retained; explicit retry or custody review required" (and its siblings:
-- an unbound stage, a stage whose identity or generation moved), and a
-- publication that merely waited in the queue behind slow imports was failed
-- "stalled: not imported within 6 h of discovery". Neither is a fact about the
-- payload. The stage is now scratch (deleted and written again), the stall
-- deadline counts from the import's own start, and obfuscated single-episode
-- grabs map by the grab record; so every publication that failed only for
-- those reasons goes back to discovered and the poll imports it again, with
-- no operator action. A publication has no deadline until its first import
-- starts (it then has six hours, never reset by a retry, and at most
-- MaxPublicationImportAttempts starts; past either it ends import_failed and
-- its wantables are grabbed again), so every discovered row's deadline is
-- cleared here too.
--
-- Only import_failed publications are touched: imported, notified, removed
-- and superseded rows are never re-opened. The download row is moved by the
-- import itself (importing, then imported or failed) when it runs.
ALTER TABLE publications ADD COLUMN import_attempts INTEGER NOT NULL DEFAULT 0;

CREATE TEMP TABLE wedged_0076 AS
SELECT p.id AS publication_id, p.download_id, d.generation_key,
       (json_array_length(d.wantables) > 0 AND NOT EXISTS (
          SELECT 1 FROM json_each(d.wantables) w
           WHERE NOT EXISTS (
             SELECT 1 FROM wantable_attempts a JOIN downloads l ON l.id = a.download_id
              WHERE a.wantable_id = w.value AND a.download_id <> d.id AND l.state IN ('imported', 'notified')))) AS satisfied
  FROM publications p JOIN downloads d ON d.id = p.download_id
 WHERE p.state = 'import_failed'
   AND (p.error LIKE '%existing stage retained%'
     OR p.error LIKE '%custody review required%'
     OR p.error LIKE '%unbound stage requires%'
     OR p.error LIKE '%staging identity mismatch%'
     OR p.error LIKE '%staging generation changed%'
     OR p.error LIKE '%staging object changed%'
     OR p.error LIKE '%file changed during verification%'
     OR p.error LIKE '%cannot determine episodes from%'
     OR p.error LIKE 'stalled: not imported within 6 h of discovery%');

-- A want a later import already satisfied is not imported again: the
-- publication's undecided files are discarded{superseded} and it is marked
-- imported, so the hourly cleanup retires the directory.
UPDATE publication_files
   SET outcome = 'discarded', discard_reason = 'superseded', library_path = '', placed_sha256 = '',
       reason = 'a later import satisfied every want of this download',
       updated_at = CAST(strftime('%s', 'now') AS INTEGER) * 1000
 WHERE removed_at = 0 AND outcome IN ('pending', 'unresolved')
   AND publication_id IN (SELECT publication_id FROM wedged_0076 WHERE satisfied);
UPDATE publications
   SET state = 'imported', error = 'superseded: a later import satisfied every want of this download',
       updated_at = CAST(strftime('%s', 'now') AS INTEGER) * 1000
 WHERE id IN (SELECT publication_id FROM wedged_0076 WHERE satisfied);

-- The rest go back to discovered. The custody an interrupted attempt left
-- (a held sidecar stage, a pending or retained-for-review authorization)
-- would make the retry refuse silently; the stage is scratch and the
-- authorization of a placement that never committed is disposed of, so the
-- retry starts a fresh epoch.
UPDATE import_attempt_stages SET state = 'released'
 WHERE state = 'held' AND EXISTS (SELECT 1 FROM wedged_0076 w
   WHERE NOT w.satisfied AND w.download_id = import_attempt_stages.download_id AND w.generation_key = import_attempt_stages.generation_key);
UPDATE import_publication_authorizations
   SET outcome = 'reconciled', resolution = 'operator_disposed', resolved_at = CAST(strftime('%s', 'now') AS INTEGER) * 1000
 WHERE (outcome = 'pending' OR resolution = 'retained_for_review')
   AND EXISTS (SELECT 1 FROM wedged_0076 w
   WHERE NOT w.satisfied AND w.download_id = import_publication_authorizations.download_id AND w.generation_key = import_publication_authorizations.generation_key);
UPDATE publications
   SET state = 'discovered',
       error = '',
       deadline_at = 0,
       updated_at = CAST(strftime('%s', 'now') AS INTEGER) * 1000
 WHERE id IN (SELECT publication_id FROM wedged_0076 WHERE NOT satisfied);
DROP TABLE wedged_0076;

UPDATE publications SET deadline_at = 0 WHERE state = 'discovered';

-- Sidecars an earlier build left for Resolve in a publication that already
-- imported ("sidecar with no automatic placement", "not media of this
-- item's kind") are re-decided by the new rules: they become pending, and the
-- hourly cleanup pass places each subtitle beside the video the publication
-- already placed and discards the rest, so the directory can be retired. The
-- placed video and every other outcome are untouched.
UPDATE publication_files
   SET outcome = 'pending', reason = '', updated_at = CAST(strftime('%s', 'now') AS INTEGER) * 1000
 WHERE outcome = 'unresolved' AND removed_at = 0
   AND (reason = 'sidecar with no automatic placement; place or discard it in Resolve'
     OR reason = 'not media of this item''s kind')
   AND publication_id IN (SELECT id FROM publications WHERE state IN ('imported', 'notified'));

-- Publication cleanup decides a library copy by its placement record, looked
-- up by target.
CREATE INDEX IF NOT EXISTS import_placements_target ON import_placements(target);

-- The subtitles Curator placed beside a library video are found by a range
-- on their library path (a replaced video takes its subtitles with it).
CREATE INDEX IF NOT EXISTS publication_files_placed_library ON publication_files(library_path) WHERE outcome = 'placed';

-- +goose Down
DROP INDEX IF EXISTS import_placements_target;
DROP INDEX IF EXISTS publication_files_placed_library;
ALTER TABLE publications DROP COLUMN import_attempts;
