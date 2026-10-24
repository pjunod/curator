-- +goose Up
-- A storage identity may only be enrolled after a verified, present-file
-- observation. It protects later absence checks from adopting an accessible
-- directory below a silently detached mount.
CREATE TABLE replacement_storage_identity_pins (
    root_path TEXT PRIMARY KEY,
    storage_identity TEXT NOT NULL,
    source_observation_id TEXT NOT NULL,
    pinned_at INTEGER NOT NULL
) STRICT;

-- The existing observation table did not permit the explicit absent state.
-- Rebuild it while retaining all rows and indexes.
ALTER TABLE replacement_file_observations RENAME TO replacement_file_observations_0050;
CREATE TABLE replacement_file_observations (
    observation_id TEXT PRIMARY KEY,
    request_id TEXT NOT NULL UNIQUE,
    media_item_id INTEGER NOT NULL,
    media_file_id INTEGER NOT NULL,
    file_generation TEXT NOT NULL,
    source_generation TEXT NOT NULL DEFAULT '',
    path TEXT NOT NULL,
    storage_identity TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL CHECK (state IN ('pending','ready','absent','stale','failed')),
    sha256 TEXT NOT NULL DEFAULT '',
    file_identity TEXT NOT NULL DEFAULT '',
    size INTEGER NOT NULL DEFAULT 0,
    error TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    filesystem_generation TEXT NOT NULL DEFAULT ''
) STRICT;
INSERT INTO replacement_file_observations(
    observation_id,request_id,media_item_id,media_file_id,file_generation,
    source_generation,path,storage_identity,state,sha256,file_identity,size,
    error,created_at,updated_at,filesystem_generation
)
SELECT observation_id,request_id,media_item_id,media_file_id,file_generation,
    source_generation,path,storage_identity,state,sha256,file_identity,size,
    error,created_at,updated_at,filesystem_generation
FROM replacement_file_observations_0050;
DROP TABLE replacement_file_observations_0050;
CREATE INDEX replacement_file_observations_pending ON replacement_file_observations(state,created_at);

-- +goose Down
CREATE TEMP TABLE replacement_storage_identity_downgrade_guard (ok INTEGER NOT NULL CHECK (ok=1));
INSERT INTO replacement_storage_identity_downgrade_guard
SELECT CASE WHEN EXISTS (SELECT 1 FROM replacement_storage_identity_pins)
    OR EXISTS (SELECT 1 FROM replacement_file_observations WHERE state='absent')
    THEN 0 ELSE 1 END;
DROP TABLE replacement_storage_identity_downgrade_guard;
DROP INDEX replacement_file_observations_pending;
ALTER TABLE replacement_file_observations RENAME TO replacement_file_observations_0058;
CREATE TABLE replacement_file_observations (
    observation_id TEXT PRIMARY KEY,
    request_id TEXT NOT NULL UNIQUE,
    media_item_id INTEGER NOT NULL,
    media_file_id INTEGER NOT NULL,
    file_generation TEXT NOT NULL,
    source_generation TEXT NOT NULL DEFAULT '',
    path TEXT NOT NULL,
    storage_identity TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL CHECK (state IN ('pending','ready','stale','failed')),
    sha256 TEXT NOT NULL DEFAULT '',
    file_identity TEXT NOT NULL DEFAULT '',
    size INTEGER NOT NULL DEFAULT 0,
    error TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    filesystem_generation TEXT NOT NULL DEFAULT ''
) STRICT;
INSERT INTO replacement_file_observations(
    observation_id,request_id,media_item_id,media_file_id,file_generation,
    source_generation,path,storage_identity,state,sha256,file_identity,size,
    error,created_at,updated_at,filesystem_generation
)
SELECT observation_id,request_id,media_item_id,media_file_id,file_generation,
    source_generation,path,storage_identity,state,sha256,file_identity,size,
    error,created_at,updated_at,filesystem_generation
FROM replacement_file_observations_0058;
DROP TABLE replacement_file_observations_0058;
CREATE INDEX replacement_file_observations_pending ON replacement_file_observations(state,created_at);
DROP TABLE replacement_storage_identity_pins;
