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

// OverdueCutoffs are the "last changed before" instants (Unix ms) past which a
// download in each old state is overdue. The allowances behind them live in the
// pipeline package.
type OverdueCutoffs struct{ Grabbed, Downloading, Torrent, ImportInFlight int64 }

// OverdueRow is a download that has outlived its state's allowance.
type OverdueRow struct {
	ID          int64
	Title       string
	State       string
	Protocol    string
	UpdatedAtMs int64
}

// overdueWhere selects rows still moving through the old state machine that
// have been silent past their cutoff. Superseded rows and rows parked for an
// operator are excluded: ParkDownload does not touch updated_at, so a parked
// row would otherwise warn forever about something already surfaced elsewhere.
const overdueWhere = `superseded=0 AND parked_at=0 AND (
	(state='grabbed' AND updated_at<?1) OR
	(state='downloading' AND protocol!='torrent' AND updated_at<?2) OR
	(state='downloading' AND protocol='torrent' AND updated_at<?3) OR
	(state IN ('downloaded','importing') AND updated_at<?4))`

// OverdueCount counts overdue downloads with one aggregate.
func (d *DB) OverdueCount(ctx context.Context, c OverdueCutoffs) (n int, err error) {
	err = d.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM downloads WHERE `+overdueWhere,
		c.Grabbed, c.Downloading, c.Torrent, c.ImportInFlight).Scan(&n)
	return n, err
}

// OverdueRows lists the oldest overdue downloads, at most limit of them.
func (d *DB) OverdueRows(ctx context.Context, c OverdueCutoffs, limit int) ([]OverdueRow, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT id, release_title, state, protocol, updated_at FROM downloads WHERE `+
		overdueWhere+` ORDER BY updated_at LIMIT ?5`, c.Grabbed, c.Downloading, c.Torrent, c.ImportInFlight, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []OverdueRow
	for rows.Next() {
		var r OverdueRow
		if err := rows.Scan(&r.ID, &r.Title, &r.State, &r.Protocol, &r.UpdatedAtMs); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
