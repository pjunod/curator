-- +goose Up
-- Keep future monitoring intent independently of the temporary series pause.
-- Existing episode/season choices survive; future seasons default to monitored.
ALTER TABLE media_items ADD COLUMN monitor TEXT NOT NULL DEFAULT 'all';
ALTER TABLE media_items ADD COLUMN monitor_since TEXT NOT NULL DEFAULT '';
ALTER TABLE media_items ADD COLUMN monitor_season INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE media_items DROP COLUMN monitor_season;
ALTER TABLE media_items DROP COLUMN monitor_since;
ALTER TABLE media_items DROP COLUMN monitor;
