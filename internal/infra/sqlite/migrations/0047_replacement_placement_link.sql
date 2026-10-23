-- +goose Up
ALTER TABLE media_file_placements ADD COLUMN replacement_operation_id TEXT NOT NULL DEFAULT '';
ALTER TABLE media_file_placements ADD COLUMN source_file_generation TEXT NOT NULL DEFAULT '';
ALTER TABLE media_files ADD COLUMN source_file_generation TEXT NOT NULL DEFAULT '';
CREATE INDEX media_file_placements_replacement ON media_file_placements(replacement_operation_id,file_generation);

-- +goose Down
CREATE TEMP TABLE replacement_placement_downgrade_guard (ok INTEGER NOT NULL CHECK (ok=1));
INSERT INTO replacement_placement_downgrade_guard
SELECT CASE WHEN EXISTS (SELECT 1 FROM media_file_placements WHERE replacement_operation_id!='' OR source_file_generation!='')
    OR EXISTS (SELECT 1 FROM media_files WHERE source_file_generation!='') THEN 0 ELSE 1 END;
DROP TABLE replacement_placement_downgrade_guard;
DROP INDEX media_file_placements_replacement;
ALTER TABLE media_file_placements DROP COLUMN replacement_operation_id;
ALTER TABLE media_file_placements DROP COLUMN source_file_generation;
ALTER TABLE media_files DROP COLUMN source_file_generation;
