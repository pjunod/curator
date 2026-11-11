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
-- no operator action. The first import attempt starts a fresh six-hour
-- deadline.
--
-- Only import_failed publications are touched: imported, notified, removed
-- and superseded rows are never re-opened. The download row is moved by the
-- import itself (importing, then imported or failed) when it runs.
UPDATE publications
   SET state = 'discovered',
       error = '',
       deadline_at = CAST(strftime('%s', 'now') AS INTEGER) * 1000 + 21600000,
       updated_at = CAST(strftime('%s', 'now') AS INTEGER) * 1000
 WHERE state = 'import_failed'
   AND (error LIKE '%existing stage retained%'
     OR error LIKE '%custody review required%'
     OR error LIKE '%unbound stage requires%'
     OR error LIKE '%staging identity mismatch%'
     OR error LIKE '%staging generation changed%'
     OR error LIKE '%staging object changed%'
     OR error LIKE '%file changed during verification%'
     OR error LIKE '%cannot determine episodes from%'
     OR error LIKE 'stalled: not imported within 6 h of discovery%');

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

-- +goose Down
DROP INDEX IF EXISTS import_placements_target;
