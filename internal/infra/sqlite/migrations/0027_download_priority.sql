-- +goose Up
-- A NULL value inherits the selected quality profile's download priority.
ALTER TABLE media_items ADD COLUMN download_priority INTEGER
    CHECK (download_priority IS NULL OR download_priority IN (-100, -50, 0, 50, 100, 900));
