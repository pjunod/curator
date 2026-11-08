package pipeline

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pjunod/monarr/internal/app/health"
)

// The overdue predicate of contract section 3.5: a download whose state's
// deadline_at has passed, or a publication still waiting to import past its
// six hours. deadline_at is written on state entry, not on every poll, so a
// job that is held or stalled inside Runner — still listed every poll — is
// overdue once its state's deadline passes: submitting 10 min of "no such
// token", downloading 30 min without progress or a new post-processing
// stage (a stage reported again is not movement; torrents 24 h), held at
// Runner's own deadline plus 5 min ("held (stale)"), downloaded/importing
// 6 h. planned, awaiting_import (a person's to decide), imported, notified
// and failed have none.
const overdueNamesInReport = 3

// OverdueRow is one download (or publication) past its deadline.
type OverdueRow struct {
	ID          int64
	Title       string
	State       string
	Late        time.Duration
	Publication bool
}

// OverdueCount is the number of overdue rows (monarr_pipeline_overdue_rows).
func (t *Timings) OverdueCount(ctx context.Context) (int, error) {
	if t == nil || t.db == nil {
		return 0, nil
	}
	return t.db.OverdueCount(ctx, t.now().UnixMilli())
}

// Overdue lists up to limit overdue rows, the most overdue first.
func (t *Timings) Overdue(ctx context.Context, limit int) ([]OverdueRow, error) {
	if t == nil || t.db == nil {
		return nil, nil
	}
	now := t.now()
	rows, err := t.db.OverdueRows(ctx, now.UnixMilli(), limit)
	if err != nil {
		return nil, err
	}
	out := make([]OverdueRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, OverdueRow{ID: r.ID, Title: r.Title, State: r.State, Publication: r.Publication,
			Late: now.Sub(time.UnixMilli(r.DeadlineMs))})
	}
	return out, nil
}

// OverdueCheck is the pipeline:overdue health check. It is a warning, never
// an error, and names the rows so there is something to open.
func (t *Timings) OverdueCheck(ctx context.Context) health.Result {
	total, err := t.OverdueCount(ctx)
	if err != nil {
		return health.Errorf("cannot read the download queue: %v", err)
	}
	if total == 0 {
		return health.OK()
	}
	rows, err := t.Overdue(ctx, overdueNamesInReport)
	if err != nil {
		return health.Errorf("cannot read the download queue: %v", err)
	}
	names := make([]string, 0, overdueNamesInReport)
	for _, r := range rows {
		what := fmt.Sprintf("download #%d %q in %s", r.ID, r.Title, r.State)
		if r.Publication {
			what = fmt.Sprintf("publication #%d %s (%s)", r.ID, r.Title, r.State)
		}
		names = append(names, fmt.Sprintf("%s, %s past its deadline", what, r.Late.Truncate(time.Minute)))
	}
	more := ""
	if total > len(rows) {
		more = fmt.Sprintf(" and %d more", total-len(rows))
	}
	return health.Warn("%d row(s) are past their state's deadline: %s%s. Deadlines are set on state entry: "+
		"submitting 10m, downloading 30m without progress (torrents 24h), held at Runner's deadline + 5m, "+
		"downloaded/importing and an unimported publication 6h",
		total, strings.Join(names, "; "), more)
}
