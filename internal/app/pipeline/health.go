package pipeline

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pjunod/monarr/internal/app/health"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

// M0 has no deadline_at column yet (M2 adds it), so "overdue" is judged by
// how long a row has gone without a write, against these fixed per-state
// allowances taken from contract section 3.5. updated_at is rewritten on every
// poll in which the download client still lists the row (UpdateDownloadState
// sets it unconditionally), so this detects a download Curator has stopped
// hearing about — one that vanished from the client's queue, or an import
// that never finished — NOT a transfer that is stalled or held inside the
// client: a Runner job held at 0% for days is still reported every poll and
// is never overdue here. M2's deadline_at, written on state entry, is what
// catches held and stalled jobs.
// States not listed (planned, awaiting_import, imported, failed) have no
// allowance: the first two wait on a person or a plan, the last two are done.
const (
	allowGrabbed         = 10 * time.Minute // submitted, not yet visible to the client
	allowDownloading     = 30 * time.Minute // client stopped listing it
	allowDownloadingTor  = 24 * time.Hour   // torrents idle legitimately
	allowImportInFlight  = 6 * time.Hour    // downloaded / importing
	overdueNamesInReport = 3
)

func allowance(state, protocol string) time.Duration {
	switch state {
	case "grabbed":
		return allowGrabbed
	case "downloading":
		if protocol == "torrent" {
			return allowDownloadingTor
		}
		return allowDownloading
	case "downloaded", "importing":
		return allowImportInFlight
	}
	return 0
}

func (t *Timings) cutoffs() sqlite.OverdueCutoffs {
	now := t.now()
	at := func(d time.Duration) int64 { return now.Add(-d).UnixMilli() }
	return sqlite.OverdueCutoffs{
		Grabbed: at(allowGrabbed), Downloading: at(allowDownloading),
		Torrent: at(allowDownloadingTor), ImportInFlight: at(allowImportInFlight),
	}
}

// OverdueRow is one download that has outlived its state's allowance.
type OverdueRow struct {
	ID    int64
	Title string
	State string
	Age   time.Duration
	Limit time.Duration
}

// OverdueCount is the number of overdue downloads, one aggregate query.
func (t *Timings) OverdueCount(ctx context.Context) (int, error) {
	if t == nil || t.db == nil {
		return 0, nil
	}
	return t.db.OverdueCount(ctx, t.cutoffs())
}

// Overdue lists up to limit overdue downloads, oldest first.
func (t *Timings) Overdue(ctx context.Context, limit int) ([]OverdueRow, error) {
	if t == nil || t.db == nil {
		return nil, nil
	}
	rows, err := t.db.OverdueRows(ctx, t.cutoffs(), limit)
	if err != nil {
		return nil, err
	}
	now := t.now()
	out := make([]OverdueRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, OverdueRow{ID: r.ID, Title: r.Title, State: r.State,
			Age: now.Sub(time.UnixMilli(r.UpdatedAtMs)), Limit: allowance(r.State, r.Protocol)})
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
		names = append(names, fmt.Sprintf("#%d %q in %s for %s (allowance %s)",
			r.ID, r.Title, r.State, r.Age.Truncate(time.Minute), r.Limit))
	}
	more := ""
	if total > len(rows) {
		more = fmt.Sprintf(" and %d more", total-len(rows))
	}
	return health.Warn("%d download(s) have outlived their state's allowance: %s%s. "+
		"Until the deadline column lands (pipeline M2) this means Curator has not heard about the row from the download client "+
		"for longer than its state allows (grabbed 10m, downloading 30m, torrents 24h, downloaded/importing 6h); "+
		"a job that is held or stalled inside the client is still reported each poll and is not caught by this check",
		total, strings.Join(names, "; "), more)
}
