-- +goose Up
-- Multi-copy quality targets: keep the same movie/series at MORE than one
-- quality — e.g. the main 2160p copy plus a 720p copy for someone else.
-- Each copy has its own quality profile and its own automation lifecycle
-- (wanted → grab → import → upgrade), and either its own folder (root
-- folder chosen per copy) or the item's folder (filenames already carry
-- [Quality], so same-folder copies coexist and players group them as
-- versions).
--
-- copy_id on media_files/downloads attributes files and grabs to a copy;
-- NULL means the primary. Movies and series only — book "resolutions"
-- don't exist.

CREATE TABLE media_copies (
    id                 INTEGER PRIMARY KEY,
    media_item_id      INTEGER NOT NULL REFERENCES media_items(id) ON DELETE CASCADE,
    name               TEXT    NOT NULL DEFAULT '',
    quality_profile_id INTEGER NOT NULL,
    root_folder_id     INTEGER,
    path               TEXT    NOT NULL DEFAULT '', -- '' = share the item folder
    monitored          INTEGER NOT NULL DEFAULT 1,
    added_at           INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_media_copies_item ON media_copies(media_item_id);

ALTER TABLE media_files ADD COLUMN copy_id INTEGER;
ALTER TABLE downloads   ADD COLUMN copy_id INTEGER;
