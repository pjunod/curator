-- +goose Up
ALTER TABLE replacement_operation_files ADD COLUMN size_bytes INTEGER NOT NULL DEFAULT 0;
UPDATE replacement_operation_files
SET size_bytes = COALESCE((SELECT size FROM replacement_file_observations o WHERE o.observation_id=replacement_operation_files.observation_id), 0);

-- +goose Down
CREATE TEMP TABLE replacement_file_size_downgrade_guard (ok INTEGER NOT NULL CHECK (ok = 1));
INSERT INTO replacement_file_size_downgrade_guard
SELECT CASE WHEN EXISTS (SELECT 1 FROM replacement_operation_files WHERE size_bytes != 0) THEN 0 ELSE 1 END;
ALTER TABLE replacement_operation_files DROP COLUMN size_bytes;
DROP TABLE replacement_file_size_downgrade_guard;
