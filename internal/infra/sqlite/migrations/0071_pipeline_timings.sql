-- +goose Up
-- Pipeline contract v2, M0: how long the three seams between Runner, Curator
-- and Cinema actually take, persisted per transfer so "last 24 h p95" is a
-- query rather than a scrape artefact (docs/PIPELINE_CONTRACT.md section 3.5).
--
--   failure_to_next_grab      a download failed -> the replacement grab was submitted
--   runner_done_to_import     the client reported the payload finished -> imported
--   import_to_cinema_visible  imported -> Cinema reports the files placed (M5)
--
-- Times are Unix milliseconds like every other timestamp in this database.
-- ended_at = 0 means "still waiting"; those rows are reported as a separate
-- count and never silently dropped from the picture. The transfer column is
-- the row's transfer token, or "download-<id>" for a row that never had one.
-- One row per (kind, transfer): the first start wins.
CREATE TABLE pipeline_timings (
    id          INTEGER PRIMARY KEY,
    transfer    TEXT NOT NULL,
    download_id INTEGER NOT NULL DEFAULT 0,
    kind        TEXT NOT NULL CHECK (kind IN ('failure_to_next_grab','runner_done_to_import','import_to_cinema_visible')),
    started_at  INTEGER NOT NULL,
    ended_at    INTEGER NOT NULL DEFAULT 0,
    note        TEXT NOT NULL DEFAULT ''
) STRICT;
CREATE INDEX idx_pipeline_timings_kind_started ON pipeline_timings (kind, started_at DESC);
CREATE UNIQUE INDEX idx_pipeline_timings_kind_transfer ON pipeline_timings (kind, transfer);

-- +goose Down
DROP TABLE pipeline_timings;
