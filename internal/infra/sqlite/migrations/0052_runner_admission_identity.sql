-- +goose Up
-- Bind terminal Runner outcomes to the exact download generation and Runner
-- installation observed for that admission. A reused token/handle cannot
-- authorize a different download after history removal or client repointing.
ALTER TABLE downloads ADD COLUMN runner_instance_id TEXT NOT NULL DEFAULT '';
ALTER TABLE downloads ADD COLUMN runner_admission_generation TEXT NOT NULL DEFAULT '';

-- +goose Down
CREATE TEMP TABLE runner_admission_identity_downgrade_guard (
    ok INTEGER NOT NULL CHECK (ok = 1)
);
INSERT INTO runner_admission_identity_downgrade_guard
SELECT CASE WHEN EXISTS (
    SELECT 1 FROM downloads
    WHERE runner_instance_id != '' OR runner_admission_generation != ''
) THEN 0 ELSE 1 END;
ALTER TABLE downloads DROP COLUMN runner_admission_generation;
ALTER TABLE downloads DROP COLUMN runner_instance_id;
DROP TABLE runner_admission_identity_downgrade_guard;
