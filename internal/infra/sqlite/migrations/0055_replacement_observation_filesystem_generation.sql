-- +goose Up
-- Bind explicit replacement admission to the exact filesystem object
-- observation without hashing potentially very large media in the API call.
ALTER TABLE replacement_file_observations ADD COLUMN filesystem_generation TEXT NOT NULL DEFAULT '';

-- +goose Down
CREATE TEMP TABLE replacement_observation_generation_downgrade_guard (
    ok INTEGER NOT NULL CHECK (ok = 1)
);
INSERT INTO replacement_observation_generation_downgrade_guard
SELECT CASE WHEN EXISTS (SELECT 1 FROM replacement_file_observations WHERE filesystem_generation!='') THEN 0 ELSE 1 END;
ALTER TABLE replacement_file_observations DROP COLUMN filesystem_generation;
DROP TABLE replacement_observation_generation_downgrade_guard;
