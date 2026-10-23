-- +goose Up
-- Library files keep their immutable placement generation and their original
-- download custody after Activity rows are dismissed or removed.
ALTER TABLE media_files ADD COLUMN file_generation TEXT NOT NULL DEFAULT '';
ALTER TABLE media_files ADD COLUMN source_generation TEXT NOT NULL DEFAULT '';
ALTER TABLE media_files ADD COLUMN source_transfer TEXT NOT NULL DEFAULT '';
ALTER TABLE media_files ADD COLUMN source_payload_generation TEXT NOT NULL DEFAULT '';
ALTER TABLE media_files ADD COLUMN source_guid TEXT NOT NULL DEFAULT '';
ALTER TABLE media_files ADD COLUMN source_indexer_id INTEGER NOT NULL DEFAULT 0;
UPDATE media_files SET file_generation=lower(hex(randomblob(16))) WHERE file_generation='';

-- +goose StatementBegin
CREATE TRIGGER media_files_fill_file_generation
AFTER INSERT ON media_files WHEN NEW.file_generation=''
BEGIN
    UPDATE media_files SET file_generation=lower(hex(randomblob(16))) WHERE id=NEW.id;
END;
-- +goose StatementEnd

CREATE TABLE media_file_placements (
    placement_id TEXT PRIMARY KEY,
    media_file_id INTEGER NOT NULL,
    file_generation TEXT NOT NULL UNIQUE,
    source_generation TEXT NOT NULL DEFAULT '',
    source_transfer TEXT NOT NULL DEFAULT '',
    source_payload_generation TEXT NOT NULL DEFAULT '',
    source_guid TEXT NOT NULL DEFAULT '',
    source_indexer_id INTEGER NOT NULL DEFAULT 0,
    path TEXT NOT NULL,
    sha256 TEXT NOT NULL,
    committed_at INTEGER NOT NULL
) STRICT;

ALTER TABLE downloads ADD COLUMN replacement_operation_id TEXT NOT NULL DEFAULT '';
ALTER TABLE downloads ADD COLUMN source_guid TEXT NOT NULL DEFAULT '';
ALTER TABLE downloads ADD COLUMN source_indexer_id INTEGER NOT NULL DEFAULT 0;

CREATE TABLE release_blacklist_identity (
    id INTEGER PRIMARY KEY,
    media_item_id INTEGER NOT NULL REFERENCES media_items(id) ON DELETE CASCADE,
    guid TEXT NOT NULL,
    indexer_id INTEGER NOT NULL,
    reason TEXT NOT NULL DEFAULT '',
    added_at INTEGER NOT NULL,
    UNIQUE(media_item_id,indexer_id,guid)
) STRICT;

CREATE TABLE replacement_operations (
    operation_id TEXT PRIMARY KEY,
    request_id TEXT NOT NULL UNIQUE,
    fingerprint TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('accepted','removing','cleanup_blocked','searching','waiting_for_release','replacing','complete','cancelled','failed')),
    intent TEXT NOT NULL CHECK (intent IN ('replace_bad','replace_quality')),
    scope TEXT NOT NULL CHECK (scope IN ('episodes','download')),
    -- Item ids are immutable audit references; deletion must not be blocked by history.
    media_item_id INTEGER NOT NULL,
    copy_id INTEGER NOT NULL DEFAULT 0,
    source_generation TEXT NOT NULL,
    source_transfer TEXT NOT NULL DEFAULT '',
    source_release TEXT NOT NULL DEFAULT '',
    source_indexer TEXT NOT NULL DEFAULT '',
    source_guid TEXT NOT NULL DEFAULT '',
    source_indexer_id INTEGER NOT NULL DEFAULT 0,
    blacklist_source_release INTEGER NOT NULL CHECK (blacklist_source_release IN (0,1)),
    reason_code TEXT NOT NULL,
    reason TEXT NOT NULL,
    episode_ids TEXT NOT NULL DEFAULT '[]',
    result TEXT NOT NULL DEFAULT '{}',
    last_error TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;

CREATE TABLE replacement_operation_files (
    operation_id TEXT NOT NULL REFERENCES replacement_operations(operation_id) ON DELETE RESTRICT,
    media_file_id INTEGER NOT NULL,
    file_generation TEXT NOT NULL,
    source_generation TEXT NOT NULL,
    path TEXT NOT NULL,
    sha256 TEXT NOT NULL DEFAULT '',
    episode_ids TEXT NOT NULL DEFAULT '[]',
    disposition TEXT NOT NULL DEFAULT 'selected' CHECK (disposition IN ('selected','preserved','excess')),
    removal_state TEXT NOT NULL DEFAULT 'protected' CHECK (removal_state IN ('protected','pending','removed','blocked')),
    PRIMARY KEY (operation_id, media_file_id, file_generation)
) STRICT;

CREATE INDEX replacement_operation_state ON replacement_operations(state, updated_at);

CREATE TABLE replacement_operation_steps (
    id INTEGER PRIMARY KEY,
    operation_id TEXT NOT NULL REFERENCES replacement_operations(operation_id) ON DELETE RESTRICT,
    step TEXT NOT NULL,
    state TEXT NOT NULL,
    attempt INTEGER NOT NULL DEFAULT 1,
    error TEXT NOT NULL DEFAULT '',
    started_at INTEGER NOT NULL,
    ended_at INTEGER NOT NULL DEFAULT 0,
    next_attempt_at INTEGER NOT NULL DEFAULT 0
) STRICT;

-- The lifecycle history and immutable provenance are independent of Activity.
-- Refuse downgrade while either would be discarded.
-- +goose Down
CREATE TEMP TABLE replacement_downgrade_guard (ok INTEGER NOT NULL CHECK (ok=1));
INSERT INTO replacement_downgrade_guard
SELECT CASE WHEN EXISTS (SELECT 1 FROM replacement_operations)
    OR EXISTS (SELECT 1 FROM media_file_placements)
    OR EXISTS (SELECT 1 FROM release_blacklist_identity)
    OR EXISTS (SELECT 1 FROM media_files WHERE source_generation!='' OR source_transfer!='' OR source_payload_generation!='')
    THEN 0 ELSE 1 END;
DROP TABLE replacement_downgrade_guard;
DROP TABLE replacement_operation_steps;
DROP TABLE replacement_operation_files;
DROP INDEX replacement_operation_state;
DROP TABLE replacement_operations;
DROP TABLE media_file_placements;
DROP TABLE release_blacklist_identity;
ALTER TABLE downloads DROP COLUMN source_indexer_id;
ALTER TABLE downloads DROP COLUMN source_guid;
ALTER TABLE downloads DROP COLUMN replacement_operation_id;
-- +goose StatementBegin
DROP TRIGGER media_files_fill_file_generation;
-- +goose StatementEnd
ALTER TABLE media_files DROP COLUMN source_payload_generation;
ALTER TABLE media_files DROP COLUMN source_indexer_id;
ALTER TABLE media_files DROP COLUMN source_guid;
ALTER TABLE media_files DROP COLUMN source_transfer;
ALTER TABLE media_files DROP COLUMN source_generation;
ALTER TABLE media_files DROP COLUMN file_generation;
