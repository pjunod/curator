package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// RecordDownloadSubmission atomically binds the exact client handle and the
// submitted phase after Add acknowledges a durable request token. A crash can
// leave either the pre-submit/uncertain row for idempotent replay or this
// fully bound state, never submitted-without-handle.
func (d *DB) RecordDownloadSubmission(ctx context.Context, downloadID int64, generationKey, transfer, handle string) error {
	if downloadID <= 0 || generationKey == "" || transfer == "" || handle == "" {
		return fmt.Errorf("download submission receipt requires exact generation, transfer, and handle")
	}
	res, err := d.W.ExecContext(ctx, `UPDATE downloads SET handle=?,submission_phase='submitted',runner_submission_retry_at=0,runner_submission_retry_attempts=0,runner_submission_blocked_at=0,updated_at=?
		WHERE id=? AND generation_key=? AND transfer=? AND submission_phase IN ('submitting','uncertain','submitted')
		AND (handle='' OR handle=?)`, handle, time.Now().UnixMilli(), downloadID, generationKey, transfer, handle)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return err
		}
		return ErrRunnerAdmissionIdentityConflict
	}
	return nil
}

// RejectDownloadSubmission atomically retires an exact request after a
// definitive client refusal. State and phase cannot be split by a crash.
func (d *DB) RejectDownloadSubmission(ctx context.Context, downloadID int64, generationKey, transfer, reason string) error {
	if downloadID <= 0 || generationKey == "" || transfer == "" {
		return fmt.Errorf("submission refusal requires exact generation and transfer")
	}
	res, err := d.W.ExecContext(ctx, `UPDATE downloads SET state='failed',submission_phase='rejected',error=?,updated_at=?
		WHERE id=? AND generation_key=? AND transfer=? AND submission_phase IN ('pending','submitting','uncertain','submitted')
		AND state NOT IN ('failed','imported','superseded') AND superseded=0 AND parked_at=0`, reason, time.Now().UnixMilli(), downloadID, generationKey, transfer)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return err
		}
		return ErrRunnerAdmissionIdentityConflict
	}
	return nil
}

// RecordHandlelessSubmission persists a successful Add response from a client
// that later exposes the exact transfer token in Statuses but does not return
// a native handle from Add. Submitted-with-empty-handle is a distinct durable
// state: admission is accepted, and recovery must only reconcile by token.
func (d *DB) RecordHandlelessSubmission(ctx context.Context, downloadID int64, generationKey, transfer string) error {
	if downloadID <= 0 || generationKey == "" || transfer == "" {
		return fmt.Errorf("handleless submission receipt requires exact generation and transfer")
	}
	now := time.Now()
	res, err := d.W.ExecContext(ctx, `UPDATE downloads SET handle='',submission_phase='submitted',runner_submission_retry_at=0,runner_submission_retry_attempts=0,runner_submission_blocked_at=0,
		manual_retry_at=CASE WHEN intent='manual_additional' THEN ? ELSE manual_retry_at END,
		manual_retry_attempts=CASE WHEN intent='manual_additional' THEN 1 ELSE manual_retry_attempts END,
		manual_blocked_at=CASE WHEN intent='manual_additional' AND manual_blocked_at=0 THEN ? ELSE manual_blocked_at END,
		error='client accepted the request; waiting for exact transfer-token correlation',updated_at=?
		WHERE id=? AND generation_key=? AND transfer=? AND exact_transfer_correlation=1 AND submission_phase IN ('submitting','uncertain','submitted') AND handle=''`,
		now.Add(30*time.Second).UnixMilli(), now.UnixMilli(), now.UnixMilli(), downloadID, generationKey, transfer)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return err
		}
		return ErrRunnerAdmissionIdentityConflict
	}
	return nil
}

type RunnerSubmissionRetry struct {
	DownloadID    int64
	GenerationKey string
	Attempts      int
	BlockedAt     time.Time
}

func (d *DB) ListRunnerSubmissionRetries(ctx context.Context, now time.Time, limit int) ([]RunnerSubmissionRetry, error) {
	if limit <= 0 {
		limit = 32
	}
	rows, err := d.R.QueryContext(ctx, `SELECT id,generation_key,runner_submission_retry_attempts,runner_submission_blocked_at
		FROM downloads WHERE request_token_idempotent=1 AND intent!='manual_additional' AND submission_phase IN ('submitting','uncertain')
		AND submission_phase!='rejected' AND MAX(runner_submission_retry_at,provider_retry_after)<=? AND state NOT IN ('failed','imported','superseded')
		AND parked_at=0 AND superseded=0
		ORDER BY runner_submission_retry_at,runner_submission_retry_attempts,added_at,id LIMIT ?`, now.UnixMilli(), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []RunnerSubmissionRetry
	for rows.Next() {
		var item RunnerSubmissionRetry
		var blocked int64
		if err := rows.Scan(&item.DownloadID, &item.GenerationKey, &item.Attempts, &blocked); err != nil {
			return nil, err
		}
		if blocked > 0 {
			item.BlockedAt = time.UnixMilli(blocked)
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (d *DB) RunnerSubmissionRetryDue(ctx context.Context, downloadID int64, generationKey string, now time.Time) (bool, error) {
	var due int
	err := d.R.QueryRowContext(ctx, `SELECT CASE WHEN MAX(runner_submission_retry_at,provider_retry_after)<=? THEN 1 ELSE 0 END FROM downloads WHERE id=? AND generation_key=? AND request_token_idempotent=1 AND intent!='manual_additional' AND submission_phase IN ('submitting','uncertain') AND parked_at=0 AND superseded=0 AND state NOT IN ('failed','imported','superseded')`, now.UnixMilli(), downloadID, generationKey).Scan(&due)
	if err != nil {
		return false, err
	}
	return due == 1, nil
}

func (d *DB) ScheduleRunnerSubmissionRetry(ctx context.Context, downloadID int64, generationKey string, now time.Time, detail string, providerDeadline ...time.Time) (RunnerSubmissionRetry, error) {
	var retry RunnerSubmissionRetry
	var blocked int64
	err := d.W.QueryRowContext(ctx, `SELECT runner_submission_retry_attempts,runner_submission_blocked_at FROM downloads
		WHERE id=? AND generation_key=? AND request_token_idempotent=1 AND intent!='manual_additional' AND submission_phase IN ('submitting','uncertain')
		AND parked_at=0 AND superseded=0 AND state NOT IN ('failed','imported','superseded')`, downloadID, generationKey).Scan(&retry.Attempts, &blocked)
	if err != nil {
		return retry, err
	}
	if blocked == 0 {
		blocked = now.UnixMilli()
	}
	backoff := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute}
	index := retry.Attempts
	if index >= len(backoff) {
		index = len(backoff) - 1
	}
	next := now.Add(backoff[index])
	providerAt := int64(0)
	if len(providerDeadline) > 0 && providerDeadline[0].After(now) {
		providerAt = providerDeadline[0].UnixMilli()
		if providerDeadline[0].After(next) {
			next = providerDeadline[0]
		}
	}
	res, err := d.W.ExecContext(ctx, `UPDATE downloads SET runner_submission_retry_attempts=runner_submission_retry_attempts+1,
		runner_submission_retry_at=?,provider_retry_after=MAX(provider_retry_after,?),runner_submission_blocked_at=?,error=?,updated_at=?
		WHERE id=? AND generation_key=? AND request_token_idempotent=1 AND intent!='manual_additional' AND submission_phase IN ('submitting','uncertain')
		AND parked_at=0 AND superseded=0 AND state NOT IN ('failed','imported','superseded')`,
		next.UnixMilli(), providerAt, blocked, detail, now.UnixMilli(), downloadID, generationKey)
	if err != nil {
		return retry, err
	}
	if n, e := res.RowsAffected(); e != nil || n != 1 {
		if e != nil {
			return retry, e
		}
		return retry, ErrRunnerAdmissionIdentityConflict
	}
	retry.DownloadID, retry.GenerationKey, retry.Attempts, retry.BlockedAt = downloadID, generationKey, index+1, time.UnixMilli(blocked)
	return retry, nil
}

// UpdateRunnerSubmissionWarning only updates the error detail for the exact
// still-retryable row. It cannot overwrite state or revive a cancelled row.
func (d *DB) UpdateRunnerSubmissionWarning(ctx context.Context, downloadID int64, generationKey, warning string) error {
	res, err := d.W.ExecContext(ctx, `UPDATE downloads SET error=?,updated_at=?
		WHERE id=? AND generation_key=? AND request_token_idempotent=1 AND intent!='manual_additional'
		AND submission_phase IN ('submitting','uncertain') AND parked_at=0 AND superseded=0
		AND state NOT IN ('failed','imported','superseded')`, warning, time.Now().UnixMilli(), downloadID, generationKey)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return err
		}
		return ErrNotFound
	}
	return nil
}

var ErrRunnerAdmissionIdentityConflict = errors.New("runner admission identity conflicts with the pinned request")

// RunnerAdmissionPollOrder places never-polled and oldest-due request receipts
// first so a bounded poll batch cannot repeatedly favor the front of the
// ordinary download list while later rows starve.
func (d *DB) RunnerAdmissionPollOrder(ctx context.Context, clientID int64) ([]int64, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT d.id FROM downloads d
		LEFT JOIN runner_admission_poll_schedule s ON s.download_id=d.id AND s.generation_key=d.generation_key
		WHERE d.client_id=? AND d.request_token_idempotent=1
		ORDER BY coalesce(s.next_attempt_at,0),coalesce(s.attempts,0),d.added_at,d.id`, clientID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ClaimRunnerAdmissionPoll reserves the next bounded receipt lookup for this
// exact download generation. Failed lookups use a durable 30s, 1m, 2m, 5m
// progression; successful reads are paced at five minutes while cleanup and
// terminal state settle.
func (d *DB) ClaimRunnerAdmissionPoll(ctx context.Context, downloadID int64, generationKey string, now time.Time) (bool, error) {
	if downloadID <= 0 || generationKey == "" {
		return false, fmt.Errorf("runner admission poll requires a download generation")
	}
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `INSERT INTO runner_admission_poll_schedule(download_id,generation_key,next_attempt_at,attempts,last_error)
		SELECT id,?,0,0,'' FROM downloads WHERE id=? AND generation_key=?
		ON CONFLICT(download_id) DO UPDATE SET generation_key=excluded.generation_key,next_attempt_at=0,attempts=0,last_error=''
		WHERE runner_admission_poll_schedule.generation_key!=excluded.generation_key`, generationKey, downloadID, generationKey); err != nil {
		return false, err
	}
	var currentGeneration string
	var nextAttempt int64
	var attempts int
	if err = tx.QueryRowContext(ctx, `SELECT generation_key,next_attempt_at,attempts FROM runner_admission_poll_schedule WHERE download_id=?`, downloadID).Scan(&currentGeneration, &nextAttempt, &attempts); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, ErrRunnerAdmissionIdentityConflict
		}
		return false, err
	}
	if currentGeneration != generationKey {
		return false, ErrRunnerAdmissionIdentityConflict
	}
	if nextAttempt > now.UnixMilli() {
		return false, tx.Commit()
	}
	backoff := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute}
	index := attempts
	if index < 0 {
		index = 0
	}
	if index >= len(backoff) {
		index = len(backoff) - 1
	}
	var result sql.Result
	result, err = tx.ExecContext(ctx, `UPDATE runner_admission_poll_schedule SET next_attempt_at=?,attempts=attempts+1,last_error='lookup pending'
		WHERE download_id=? AND generation_key=? AND next_attempt_at<=?`, now.Add(backoff[index]).UnixMilli(), downloadID, generationKey, now.UnixMilli())
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if changed == 0 {
		return false, tx.Commit()
	}
	return true, tx.Commit()
}

func (d *DB) RecordRunnerAdmissionPollResult(ctx context.Context, downloadID int64, generationKey string, found bool, lookupErr error, now time.Time) error {
	lastError := ""
	if lookupErr != nil {
		lastError = lookupErr.Error()
	} else if !found {
		lastError = "Runner admission receipt not found yet"
	}
	if found && lookupErr == nil {
		_, err := d.W.ExecContext(ctx, `UPDATE runner_admission_poll_schedule SET next_attempt_at=?,attempts=0,last_error=''
			WHERE download_id=? AND generation_key=?`, now.Add(5*time.Minute).UnixMilli(), downloadID, generationKey)
		return err
	}
	// Claim already persisted a retry deadline; this only annotates the result.
	_, err := d.W.ExecContext(ctx, `UPDATE runner_admission_poll_schedule SET last_error=? WHERE download_id=? AND generation_key=?`, lastError, downloadID, generationKey)
	return err
}

// BindRunnerAdmission pins the first authoritative receipt to both the
// immutable download generation/transfer token and the configured Runner
// installation. Replays are accepted only when every identity component is
// unchanged. Delivery receipts may have pinned the installation earlier.
func (d *DB) BindRunnerAdmission(ctx context.Context, downloadID int64, generationKey, transfer, runnerInstanceID, admissionGeneration string) error {
	if downloadID <= 0 || generationKey == "" || transfer == "" || runnerInstanceID == "" || admissionGeneration == "" {
		return fmt.Errorf("runner admission binding requires download, generation, transfer, installation, and admission generation")
	}
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var clientID int64
	if err = tx.QueryRowContext(ctx, `SELECT client_id FROM downloads WHERE id=? AND generation_key=? AND transfer=?`,
		downloadID, generationKey, transfer).Scan(&clientID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrRunnerAdmissionIdentityConflict
		}
		return err
	}
	var pinned string
	err = tx.QueryRowContext(ctx, `SELECT runner_instance_id FROM runner_client_instances WHERE client_id=?`, clientID).Scan(&pinned)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.ExecContext(ctx, `INSERT INTO runner_client_instances(client_id,runner_instance_id,updated_at) VALUES(?,?,?)`, clientID, runnerInstanceID, time.Now().UnixMilli())
		if err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if pinned == "" {
		res, e := tx.ExecContext(ctx, `UPDATE runner_client_instances SET runner_instance_id=?,updated_at=? WHERE client_id=? AND runner_instance_id=''`, runnerInstanceID, time.Now().UnixMilli(), clientID)
		if e != nil {
			return e
		}
		if n, e := res.RowsAffected(); e != nil || n != 1 {
			return ErrRunnerAdmissionIdentityConflict
		}
	} else if pinned != runnerInstanceID {
		return ErrRunnerAdmissionIdentityConflict
	}

	var currentRunner, currentGeneration string
	err = tx.QueryRowContext(ctx, `SELECT runner_instance_id,runner_admission_generation FROM downloads WHERE id=? AND generation_key=? AND transfer=?`,
		downloadID, generationKey, transfer).Scan(&currentRunner, &currentGeneration)
	if err != nil {
		return err
	}
	if currentRunner != "" || currentGeneration != "" {
		if currentRunner != runnerInstanceID || currentGeneration != admissionGeneration {
			return ErrRunnerAdmissionIdentityConflict
		}
		return tx.Commit()
	}
	res, err := tx.ExecContext(ctx, `UPDATE downloads SET runner_instance_id=?,runner_admission_generation=?,updated_at=?
		WHERE id=? AND generation_key=? AND transfer=? AND runner_instance_id='' AND runner_admission_generation=''`,
		runnerInstanceID, admissionGeneration, time.Now().UnixMilli(), downloadID, generationKey, transfer)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return ErrRunnerAdmissionIdentityConflict
	}
	return tx.Commit()
}
