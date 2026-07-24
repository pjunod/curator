-- name: EnqueueJob :one
-- A dedupe_key collision is the mechanism, not an error: it means an
-- identical job is already queued or running, so the caller treats the
-- constraint failure as "already scheduled".
INSERT INTO jobs (
    kind, payload, priority, run_after, max_attempts,
    dedupe_key, required_capability, affinity_node,
    created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id;

-- name: CompleteJob :exec
UPDATE jobs
   SET state = 'done', lease_owner = '', lease_expires_at = 0,
       last_error = '', updated_at = ?, finished_at = ?
 WHERE id = ?;

-- name: RetryJob :exec
-- Back to queued with a later run_after; the attempt has already been
-- counted by the claim.
UPDATE jobs
   SET state = 'queued', lease_owner = '', lease_expires_at = 0,
       last_error = ?, run_after = ?, updated_at = ?
 WHERE id = ?;

-- name: FailJob :exec
UPDATE jobs
   SET state = 'failed', lease_owner = '', lease_expires_at = 0,
       last_error = ?, updated_at = ?, finished_at = ?
 WHERE id = ?;

-- name: HeartbeatJob :execrows
-- Extends a lease only while this owner still holds it, so a job whose
-- lease was already reclaimed cannot resurrect its own claim.
UPDATE jobs
   SET lease_expires_at = ?, updated_at = ?
 WHERE id = ? AND lease_owner = ? AND state = 'leased';

-- name: GetJob :one
SELECT * FROM jobs WHERE id = ?;

-- name: ListJobsByState :many
SELECT * FROM jobs WHERE state = ? ORDER BY priority, run_after LIMIT ?;

-- name: CountJobsByState :many
SELECT state, COUNT(*) AS count FROM jobs GROUP BY state;

-- name: DeleteFinishedJobsBefore :execrows
DELETE FROM jobs
 WHERE state IN ('done', 'failed') AND finished_at > 0 AND finished_at < ?;
