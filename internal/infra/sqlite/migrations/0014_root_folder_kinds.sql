-- +goose Up
-- ADR 0009: a root folder carries the media kind it holds. Sonarr and Radarr
-- never needed this because the application was the answer; Monarr merged the
-- applications (ADR 0002) and the discriminator was never re-homed.
--
-- 'mixed' means "ask, do not assume" and is exactly today's behaviour, so the
-- default keeps every existing install working identically on upgrade.

ALTER TABLE root_folders ADD COLUMN kind TEXT NOT NULL DEFAULT 'mixed'
    CHECK (kind IN ('movie', 'series', 'book', 'mixed'));

-- Backfill by inference: a root whose items are all one kind takes that kind.
-- A root that is empty, or spans kinds, stays 'mixed' — guessing there is
-- exactly the mistake ADR 0009 §2 refuses to make.
UPDATE root_folders SET kind = (
    SELECT MIN(mi.kind) FROM media_items mi
     WHERE mi.root_folder_id = root_folders.id
)
WHERE (
    SELECT COUNT(DISTINCT mi.kind) FROM media_items mi
     WHERE mi.root_folder_id = root_folders.id
) = 1;

-- +goose Down
ALTER TABLE root_folders DROP COLUMN kind;
