-- name: UpsertTaskRun :exec
INSERT INTO scheduled_tasks (
    name, interval_seconds, last_run_at, last_duration_ms,
    last_error, next_run_at, updated_at
)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (name) DO UPDATE SET
    interval_seconds = excluded.interval_seconds,
    last_run_at      = excluded.last_run_at,
    last_duration_ms = excluded.last_duration_ms,
    last_error       = excluded.last_error,
    next_run_at      = excluded.next_run_at,
    updated_at       = excluded.updated_at;

-- name: ListTasks :many
SELECT * FROM scheduled_tasks ORDER BY name;
