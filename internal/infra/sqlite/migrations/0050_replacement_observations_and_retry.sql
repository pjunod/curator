-- +goose Up
ALTER TABLE replacement_operations ADD COLUMN retry_attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE replacement_operations ADD COLUMN blocked_since INTEGER NOT NULL DEFAULT 0;
ALTER TABLE replacement_operations ADD COLUMN next_attempt_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE replacement_operation_files ADD COLUMN file_identity TEXT NOT NULL DEFAULT '';
ALTER TABLE replacement_operation_files ADD COLUMN observation_id TEXT NOT NULL DEFAULT '';
ALTER TABLE replacement_operation_files ADD COLUMN storage_identity TEXT NOT NULL DEFAULT '';
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
    updated_at INTEGER NOT NULL
) STRICT;
CREATE INDEX replacement_file_observations_pending ON replacement_file_observations(state,created_at);

-- +goose Down
CREATE TEMP TABLE replacement_observation_downgrade_guard (ok INTEGER NOT NULL CHECK (ok=1));
INSERT INTO replacement_observation_downgrade_guard
SELECT CASE WHEN EXISTS (SELECT 1 FROM replacement_operations WHERE retry_attempts!=0 OR blocked_since!=0 OR next_attempt_at!=0)
    OR EXISTS (SELECT 1 FROM replacement_operation_files WHERE file_identity!='' OR observation_id!='' OR storage_identity!='')
    OR EXISTS (SELECT 1 FROM replacement_file_observations) THEN 0 ELSE 1 END;
DROP TABLE replacement_observation_downgrade_guard;
DROP INDEX replacement_file_observations_pending;
DROP TABLE replacement_file_observations;
ALTER TABLE replacement_operations DROP COLUMN next_attempt_at;
ALTER TABLE replacement_operations DROP COLUMN blocked_since;
ALTER TABLE replacement_operations DROP COLUMN retry_attempts;
ALTER TABLE replacement_operation_files DROP COLUMN observation_id;
ALTER TABLE replacement_operation_files DROP COLUMN file_identity;
ALTER TABLE replacement_operation_files DROP COLUMN storage_identity;
