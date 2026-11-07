package sqlite

import (
	"context"
	"fmt"
	"strings"
)

// Pipeline timing kinds (migration 0071, contract section 3.5).
const (
	TimingFailureToNextGrab     = "failure_to_next_grab"
	TimingRunnerDoneToImport    = "runner_done_to_import"
	TimingImportToCinemaVisible = "import_to_cinema_visible"
)

// StartPipelineTiming opens a timing for (kind, transfer). The first start
// wins: a second observation of the same event must not move the clock.
func (d *DB) StartPipelineTiming(ctx context.Context, kind, transfer string, downloadID, startedAt int64) error {
	_, err := d.W.ExecContext(ctx,
		`INSERT INTO pipeline_timings (transfer, download_id, kind, started_at) VALUES (?,?,?,?) ON CONFLICT (kind, transfer) DO NOTHING`,
		transfer, downloadID, kind, startedAt)
	return err
}

// EndPipelineTiming closes an open timing. A timing that was never started,
// or is already closed, is left alone.
func (d *DB) EndPipelineTiming(ctx context.Context, kind, transfer string, endedAt int64) error {
	_, err := d.W.ExecContext(ctx,
		`UPDATE pipeline_timings SET ended_at=? WHERE kind=? AND transfer=? AND ended_at=0 AND ?>=started_at`,
		endedAt, kind, transfer, endedAt)
	return err
}

// PipelineDurationsMs returns the closed durations (milliseconds) of one kind
// that ended at or after sinceMs; sinceMs 0 means all of them.
func (d *DB) PipelineDurationsMs(ctx context.Context, kind string, sinceMs int64) ([]int64, error) {
	rows, err := d.R.QueryContext(ctx,
		`SELECT ended_at-started_at FROM pipeline_timings WHERE kind=? AND ended_at>0 AND ended_at>=? ORDER BY ended_at-started_at`,
		kind, sinceMs)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// PipelineWaiting counts open timings of one kind that started at or after
// sinceMs (0 = any age) and the start of the oldest one (0 when none).
func (d *DB) PipelineWaiting(ctx context.Context, kind string, sinceMs int64) (n int, oldestStart int64, err error) {
	err = d.R.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(MIN(started_at),0) FROM pipeline_timings WHERE kind=? AND ended_at=0 AND started_at>=?`,
		kind, sinceMs).Scan(&n, &oldestStart)
	return n, oldestStart, err
}

// OverdueCandidate is a download row the M0 overdue check judges by age.
type OverdueCandidate struct {
	ID          int64
	Title       string
	State       string
	Protocol    string
	UpdatedAtMs int64
}

// OverdueCandidates lists the rows that are still moving through the old
// state machine: not terminal, not superseded, not waiting on an operator.
// The age rule lives in the pipeline package.
func (d *DB) OverdueCandidates(ctx context.Context, states []string) ([]OverdueCandidate, error) {
	if len(states) == 0 {
		return nil, nil
	}
	args := make([]any, len(states))
	for i, s := range states {
		args[i] = s
	}
	q := fmt.Sprintf(`SELECT id, release_title, state, protocol, updated_at FROM downloads
		WHERE superseded=0 AND state IN (%s) ORDER BY updated_at`, strings.TrimSuffix(strings.Repeat("?,", len(states)), ","))
	rows, err := d.R.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []OverdueCandidate
	for rows.Next() {
		var c OverdueCandidate
		if err := rows.Scan(&c.ID, &c.Title, &c.State, &c.Protocol, &c.UpdatedAtMs); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
