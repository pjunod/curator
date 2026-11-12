-- +goose Up
-- The item page's status panel (0.44.0, GET /library/{id}/status).
--
-- search_records: one row per search of one wantable, written by the shared
-- search-and-grab path (auto search, background automation, Wanted runs,
-- replacement grabs, interactive grabs, season comparisons). Newest 20 are
-- kept per wantable; the writer prunes.
--   trigger      manual | automatic | wanted | replacement | interactive | season
--   seen/matched/accepted  the search funnel (searchTally)
--   top_rejection          the commonest reason a matched release was refused
--   grabbed/download_id    what it led to ('' / 0 = nothing grabbed)
--   skipped                set = not searched, and why (unmonitored, downloading, ...)
CREATE TABLE search_records (
    id            INTEGER PRIMARY KEY,
    media_item_id INTEGER NOT NULL REFERENCES media_items(id) ON DELETE CASCADE,
    copy_id       INTEGER NOT NULL DEFAULT 0,
    wantable_id   TEXT NOT NULL,
    season        INTEGER NOT NULL DEFAULT -1,
    episode       INTEGER NOT NULL DEFAULT 0,
    at            INTEGER NOT NULL,
    trigger       TEXT NOT NULL,
    seen          INTEGER NOT NULL DEFAULT 0,
    matched       INTEGER NOT NULL DEFAULT 0,
    accepted      INTEGER NOT NULL DEFAULT 0,
    top_rejection TEXT NOT NULL DEFAULT '',
    grabbed       TEXT NOT NULL DEFAULT '',
    download_id   INTEGER NOT NULL DEFAULT 0,
    skipped       TEXT NOT NULL DEFAULT '',
    error         TEXT NOT NULL DEFAULT ''
) STRICT;
CREATE INDEX search_records_wantable ON search_records (wantable_id, id DESC);
CREATE INDEX search_records_item ON search_records (media_item_id, id DESC);

-- Runner's own transfer numbers, relayed (contract v2 job fields rate_bps and
-- remaining_bytes); ETA is their ratio, computed when read. runner_stage_at is
-- when the current post-processing stage was first reported.
ALTER TABLE downloads ADD COLUMN rate_bps INTEGER NOT NULL DEFAULT 0;
ALTER TABLE downloads ADD COLUMN remaining_bytes INTEGER NOT NULL DEFAULT 0;
ALTER TABLE downloads ADD COLUMN runner_stage_at INTEGER NOT NULL DEFAULT 0;

-- Per-item cinema read-back. media_item_id lets the item page ask for its own
-- sub-deliveries (backfilled from the download); file_results_json keeps
-- Cinema's answer for every expected file,
-- [{path, state: placed|skipped|missing, item_id, bytes, mtime_ns, reason}],
-- so each episode of a season pack has its own in-cinema state.
ALTER TABLE notifier_deliveries ADD COLUMN media_item_id INTEGER NOT NULL DEFAULT 0;
ALTER TABLE notifier_deliveries ADD COLUMN file_results_json TEXT NOT NULL DEFAULT '[]';
UPDATE notifier_deliveries
   SET media_item_id = COALESCE((SELECT d.media_item_id FROM downloads d WHERE d.id = notifier_deliveries.download_id), 0)
 WHERE download_id != 0;
CREATE INDEX notifier_deliveries_item ON notifier_deliveries (media_item_id, id DESC) WHERE media_item_id != 0;

CREATE INDEX idx_history_item ON history_events (media_item_id, ts DESC);

-- +goose Down
DROP INDEX idx_history_item;
DROP INDEX notifier_deliveries_item;
ALTER TABLE notifier_deliveries DROP COLUMN file_results_json;
ALTER TABLE notifier_deliveries DROP COLUMN media_item_id;
ALTER TABLE downloads DROP COLUMN runner_stage_at;
ALTER TABLE downloads DROP COLUMN remaining_bytes;
ALTER TABLE downloads DROP COLUMN rate_bps;
DROP TABLE search_records;
