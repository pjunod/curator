-- +goose Up
-- Clearing Activity must not discard the live ownership needed by cleanup.
-- Retained rows remain imported and never suppress new acquisition work.
ALTER TABLE downloads ADD COLUMN activity_dismissed INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE downloads DROP COLUMN activity_dismissed;
