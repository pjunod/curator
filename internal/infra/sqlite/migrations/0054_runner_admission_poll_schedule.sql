-- +goose Up
-- Persist receipt lookup pacing independently of Activity rows. A failed or
-- history-cleared Runner job must remain reconcilable without a burst of
-- sequential HTTP calls on every queue refresh.
CREATE TABLE runner_admission_poll_schedule (
    download_id INTEGER PRIMARY KEY REFERENCES downloads(id) ON DELETE CASCADE,
    generation_key TEXT NOT NULL,
    next_attempt_at INTEGER NOT NULL DEFAULT 0,
    attempts INTEGER NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT ''
);

-- +goose Down
CREATE TEMP TABLE runner_admission_poll_schedule_downgrade_guard (
    ok INTEGER NOT NULL CHECK (ok = 1)
);
INSERT INTO runner_admission_poll_schedule_downgrade_guard
SELECT CASE WHEN EXISTS (SELECT 1 FROM runner_admission_poll_schedule) THEN 0 ELSE 1 END;
DROP TABLE runner_admission_poll_schedule;
DROP TABLE runner_admission_poll_schedule_downgrade_guard;
