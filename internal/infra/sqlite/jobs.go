package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	sqlitegen "github.com/pjunod/monarr/internal/infra/sqlite/gen"
)

// ErrDuplicateJob means an identical job (same dedupe_key) is already queued
// or leased. That is the mechanism working, not a failure: dedupe keys buy
// "exactly one cluster-wide" without electing a leader (ADR 0008 §2).
var ErrDuplicateJob = errors.New("job already queued")

// EnqueueJob adds a job. A dedupe collision returns ErrDuplicateJob.
func (d *DB) EnqueueJob(ctx context.Context, j domain.Job) (int64, error) {
	now := time.Now()
	runAfter := j.RunAfter
	if runAfter.IsZero() {
		runAfter = now
	}
	maxAttempts := j.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	priority := j.Priority
	if priority == 0 {
		priority = 100
	}
	payload := j.Payload
	if payload == "" {
		payload = "{}"
	}
	id, err := d.Write.EnqueueJob(ctx, sqlitegen.EnqueueJobParams{
		Kind:               j.Kind,
		Payload:            payload,
		Priority:           priority,
		RunAfter:           runAfter.UnixMilli(),
		MaxAttempts:        maxAttempts,
		DedupeKey:          nullString(j.DedupeKey),
		RequiredCapability: nullString(j.RequiredCapability),
		AffinityNode:       nullString(j.AffinityNode),
		CreatedAt:          now.UnixMilli(),
		UpdatedAt:          now.UnixMilli(),
	})
	if err != nil {
		if isConstraint(err) {
			return 0, ErrDuplicateJob
		}
		return 0, err
	}
	return id, nil
}

// ClaimJob atomically leases the next runnable job for owner, or returns
// ErrNotFound when there is nothing to do.
//
// Hand-written rather than generated because the claim is the one
// dialect-specific statement in the design (ADR 0008 §1). SQLite has no
// SKIP LOCKED and does not need one: BEGIN IMMEDIATE makes this atomic
// because SQLite serialises writers outright, so concurrent claimants queue
// instead of skipping. The Postgres version of this method is the same
// statement with FOR UPDATE SKIP LOCKED, and every call site is unchanged.
func (d *DB) ClaimJob(ctx context.Context, owner string, capabilities []string, lease time.Duration) (domain.Job, error) {
	now := time.Now()
	// A node always advertises its own name as a capability so that
	// affinity and capability routing can share one matching rule.
	caps := append([]string{owner}, capabilities...)
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(caps)), ",")

	//nolint:gosec // placeholders are generated '?' markers, never user input
	query := fmt.Sprintf(`
		UPDATE jobs
		   SET state = 'leased', lease_owner = ?, lease_expires_at = ?,
		       attempts = attempts + 1, updated_at = ?
		 WHERE id = (
		     SELECT id FROM jobs
		      WHERE state = 'queued'
		        AND run_after <= ?
		        AND (required_capability IS NULL OR required_capability IN (%s))
		        AND (affinity_node IS NULL OR affinity_node = ?)
		      ORDER BY priority, run_after
		      LIMIT 1
		 )
		RETURNING id, kind, payload, state, priority, run_after, attempts,
		          max_attempts, last_error, dedupe_key, required_capability,
		          affinity_node, lease_owner, lease_expires_at,
		          created_at, updated_at, finished_at`, placeholders)

	args := []any{owner, now.Add(lease).UnixMilli(), now.UnixMilli(), now.UnixMilli()}
	for _, c := range caps {
		args = append(args, c)
	}
	args = append(args, owner)

	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return domain.Job{}, err
	}
	defer func() { _ = tx.Rollback() }()

	row := tx.QueryRowContext(ctx, query, args...)
	job, err := scanJob(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Job{}, ErrNotFound
		}
		return domain.Job{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.Job{}, err
	}
	return job, nil
}

// ReclaimExpiredLeases returns leased-but-expired jobs to the queue, failing
// the ones that are out of attempts. A node that dies mid-job strands
// nothing; without this, a crash would park work forever.
func (d *DB) ReclaimExpiredLeases(ctx context.Context) (int64, error) {
	now := time.Now().UnixMilli()
	res, err := d.W.ExecContext(ctx, `
		UPDATE jobs
		   SET state = CASE WHEN attempts >= max_attempts THEN 'failed' ELSE 'queued' END,
		       lease_owner = '', lease_expires_at = 0,
		       last_error = 'lease expired', updated_at = ?,
		       finished_at = CASE WHEN attempts >= max_attempts THEN ? ELSE 0 END
		 WHERE state = 'leased' AND lease_expires_at < ?`, now, now, now)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CompleteJob marks a job done.
func (d *DB) CompleteJob(ctx context.Context, id int64) error {
	now := time.Now().UnixMilli()
	return d.Write.CompleteJob(ctx, sqlitegen.CompleteJobParams{
		UpdatedAt: now, FinishedAt: now, ID: id,
	})
}

// RetryJob returns a job to the queue with a later run_after. The attempt
// was already counted when the job was claimed.
func (d *DB) RetryJob(ctx context.Context, id int64, cause string, runAfter time.Time) error {
	return d.Write.RetryJob(ctx, sqlitegen.RetryJobParams{
		LastError: cause, RunAfter: runAfter.UnixMilli(),
		UpdatedAt: time.Now().UnixMilli(), ID: id,
	})
}

// FailJob marks a job permanently failed.
func (d *DB) FailJob(ctx context.Context, id int64, cause string) error {
	now := time.Now().UnixMilli()
	return d.Write.FailJob(ctx, sqlitegen.FailJobParams{
		LastError: cause, UpdatedAt: now, FinishedAt: now, ID: id,
	})
}

// HeartbeatJob extends a lease. It reports false when the lease is no longer
// held — the job was reclaimed while this worker was still running it, and
// the worker should stop rather than write a result someone else owns.
func (d *DB) HeartbeatJob(ctx context.Context, id int64, owner string, lease time.Duration) (bool, error) {
	n, err := d.Write.HeartbeatJob(ctx, sqlitegen.HeartbeatJobParams{
		LeaseExpiresAt: time.Now().Add(lease).UnixMilli(),
		UpdatedAt:      time.Now().UnixMilli(),
		ID:             id,
		LeaseOwner:     owner,
	})
	return n > 0, err
}

// GetJob returns one job or ErrNotFound.
func (d *DB) GetJob(ctx context.Context, id int64) (domain.Job, error) {
	r, err := d.Read.GetJob(ctx, id)
	if err != nil {
		return domain.Job{}, wrapNotFound(err)
	}
	return jobFromRow(r), nil
}

// ListJobsByState returns up to limit jobs in claim order.
func (d *DB) ListJobsByState(ctx context.Context, state string, limit int64) ([]domain.Job, error) {
	rows, err := d.Read.ListJobsByState(ctx, sqlitegen.ListJobsByStateParams{
		State: state, Limit: limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]domain.Job, 0, len(rows))
	for _, r := range rows {
		out = append(out, jobFromRow(r))
	}
	return out, nil
}

// CountJobsByState powers the queue view: how much work is waiting, running,
// and failed. Failure visibility is one of the reasons the queue is worth
// having on a single instance.
func (d *DB) CountJobsByState(ctx context.Context) (map[string]int64, error) {
	rows, err := d.Read.CountJobsByState(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for _, r := range rows {
		out[r.State] = r.Count
	}
	return out, nil
}

// PruneFinishedJobs deletes terminal jobs older than the cutoff.
func (d *DB) PruneFinishedJobs(ctx context.Context, before time.Time) (int64, error) {
	return d.Write.DeleteFinishedJobsBefore(ctx, before.UnixMilli())
}

func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func jobFromRow(r sqlitegen.Job) domain.Job {
	j := domain.Job{
		ID:          r.ID,
		Kind:        r.Kind,
		Payload:     r.Payload,
		State:       domain.JobState(r.State),
		Priority:    r.Priority,
		RunAfter:    time.UnixMilli(r.RunAfter),
		Attempts:    r.Attempts,
		MaxAttempts: r.MaxAttempts,
		LastError:   r.LastError,
		LeaseOwner:  r.LeaseOwner,
		CreatedAt:   time.UnixMilli(r.CreatedAt),
		UpdatedAt:   time.UnixMilli(r.UpdatedAt),
	}
	if r.DedupeKey.Valid {
		j.DedupeKey = r.DedupeKey.String
	}
	if r.RequiredCapability.Valid {
		j.RequiredCapability = r.RequiredCapability.String
	}
	if r.AffinityNode.Valid {
		j.AffinityNode = r.AffinityNode.String
	}
	if r.LeaseExpiresAt > 0 {
		j.LeaseExpiresAt = time.UnixMilli(r.LeaseExpiresAt)
	}
	if r.FinishedAt > 0 {
		j.FinishedAt = time.UnixMilli(r.FinishedAt)
	}
	return j
}

// scanJob reads the hand-written claim's RETURNING row in column order.
func scanJob(row *sql.Row) (domain.Job, error) {
	var r sqlitegen.Job
	err := row.Scan(
		&r.ID, &r.Kind, &r.Payload, &r.State, &r.Priority, &r.RunAfter,
		&r.Attempts, &r.MaxAttempts, &r.LastError, &r.DedupeKey,
		&r.RequiredCapability, &r.AffinityNode, &r.LeaseOwner,
		&r.LeaseExpiresAt, &r.CreatedAt, &r.UpdatedAt, &r.FinishedAt,
	)
	if err != nil {
		return domain.Job{}, err
	}
	return jobFromRow(r), nil
}
