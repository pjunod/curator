-- +goose Up
ALTER TABLE downloads ADD COLUMN runner_submission_retry_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE downloads ADD COLUMN runner_submission_retry_attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE downloads ADD COLUMN runner_submission_blocked_at INTEGER NOT NULL DEFAULT 0;

-- +goose Down
CREATE TEMP TABLE runner_submission_retry_downgrade_guard (ok INTEGER NOT NULL CHECK (ok = 1));
INSERT INTO runner_submission_retry_downgrade_guard
SELECT CASE WHEN EXISTS (SELECT 1 FROM downloads WHERE runner_submission_retry_at != 0 OR runner_submission_retry_attempts != 0 OR runner_submission_blocked_at != 0) THEN 0 ELSE 1 END;
ALTER TABLE downloads DROP COLUMN runner_submission_blocked_at;
ALTER TABLE downloads DROP COLUMN runner_submission_retry_attempts;
ALTER TABLE downloads DROP COLUMN runner_submission_retry_at;
DROP TABLE runner_submission_retry_downgrade_guard;
