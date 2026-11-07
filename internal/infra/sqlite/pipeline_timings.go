package sqlite

import (
	"context"
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
		`UPDATE pipeline_timings SET ended_at=MAX(?,started_at) WHERE kind=? AND transfer=? AND ended_at=0`,
		endedAt, kind, transfer, endedAt)
	return err
}

// PipelineHistogram aggregates the closed timings of one kind in SQL: for each
// bound (milliseconds, ascending) the number of timings at or under it, plus
// the sum of all durations and their count. One row is read however many
// timings exist, which is what a metrics scrape needs.
func (d *DB) PipelineHistogram(ctx context.Context, kind string, boundsMs []int64) (cum []int64, sumMs, count int64, err error) {
	cols := []string{"COUNT(*)", "COALESCE(SUM(ended_at-started_at),0)"}
	args := make([]any, 0, len(boundsMs)+1)
	for _, b := range boundsMs {
		cols = append(cols, "COALESCE(SUM(CASE WHEN ended_at-started_at<=? THEN 1 ELSE 0 END),0)")
		args = append(args, b)
	}
	args = append(args, kind)
	dest := make([]any, len(cols))
	cum = make([]int64, len(boundsMs))
	dest[0], dest[1] = &count, &sumMs
	for i := range cum {
		dest[2+i] = &cum[i]
	}
	err = d.R.QueryRowContext(ctx,
		"SELECT "+strings.Join(cols, ", ")+" FROM pipeline_timings WHERE kind=? AND ended_at>0", args...).Scan(dest...)
	return cum, sumMs, count, err
}

// PipelineDurationsMs returns the closed durations (milliseconds) of one kind
// that ended at or after sinceMs, ascending. It reads every matching row, so
// it is for the last-24-hour tiles, not for a metrics scrape.
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

// DiscardPipelineTiming deletes an open timing that turned out to have nothing
// to wait for. A closed timing is never touched.
func (d *DB) DiscardPipelineTiming(ctx context.Context, kind, transfer string) error {
	_, err := d.W.ExecContext(ctx, `DELETE FROM pipeline_timings WHERE kind=? AND transfer=? AND ended_at=0`, kind, transfer)
	return err
}

// PipelineWaiting counts open timings of one kind that started at or after
// sinceMs (0 = any age) and the start of the oldest one (0 when none).
func (d *DB) PipelineWaiting(ctx context.Context, kind string, sinceMs int64) (n int, oldestStart int64, err error) {
	err = d.R.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(MIN(started_at),0) FROM pipeline_timings WHERE kind=? AND ended_at=0 AND started_at>=?`,
		kind, sinceMs).Scan(&n, &oldestStart)
	return n, oldestStart, err
}

// OverdueRow is a download or a publication past its deadline.
type OverdueRow struct {
	ID         int64
	Title      string
	State      string
	Protocol   string
	DeadlineMs int64
	// Publication is set for a publication past its six hours; ID is then the
	// publication's own id.
	Publication bool
}

// The overdue predicate of contract section 3.5: a download in a
// non-terminal state whose deadline_at (written on every state entry) has
// passed, plus a publication still discovered/importing past its deadline.
// A legacy-client row parked or superseded for an operator waits on a person
// (like awaiting_import) and has no deadline until M4 retires the fence.
const overdueDownloadsWhere = `state NOT IN ('imported','notified','failed') AND deadline_at>0 AND deadline_at<?1 AND superseded=0 AND parked_at=0`
const overduePublicationsWhere = `state IN ('discovered','importing') AND download_id IS NOT NULL AND deadline_at>0 AND deadline_at<?1`

// OverdueCount counts overdue downloads and publications.
func (d *DB) OverdueCount(ctx context.Context, nowMs int64) (n int, err error) {
	err = d.R.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM downloads WHERE `+overdueDownloadsWhere+`) +
		(SELECT COUNT(*) FROM publications WHERE `+overduePublicationsWhere+`)`, nowMs).Scan(&n)
	return n, err
}

// OverdueRows lists the most overdue rows first, at most limit of them.
func (d *DB) OverdueRows(ctx context.Context, nowMs int64, limit int) ([]OverdueRow, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT id, release_title, state, protocol, deadline_at, 0 FROM downloads WHERE `+overdueDownloadsWhere+`
		UNION ALL SELECT id, path, state, kind, deadline_at, 1 FROM publications WHERE `+overduePublicationsWhere+`
		ORDER BY 5 LIMIT ?2`, nowMs, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []OverdueRow
	for rows.Next() {
		var r OverdueRow
		var pub int
		if err := rows.Scan(&r.ID, &r.Title, &r.State, &r.Protocol, &r.DeadlineMs, &pub); err != nil {
			return nil, err
		}
		r.Publication = pub == 1
		out = append(out, r)
	}
	return out, rows.Err()
}
