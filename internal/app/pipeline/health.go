package pipeline

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pjunod/monarr/internal/app/health"
)

// M0 has no deadline_at column yet (M2 adds it), so "overdue" is judged by
// how long a row has sat in its state without a write, against these fixed
// per-state allowances taken from contract section 3.5. A row's updated_at
// moves on every state or progress change, so a row that is merely slow keeps
// resetting it; one that has been silent for the whole allowance is stuck.
// States not listed (planned, awaiting_import, imported, failed) have no
// allowance: the first two wait on a person or a plan, the last two are done.
const (
	allowGrabbed         = 10 * time.Minute // submitted, not yet visible to the client
	allowDownloading     = 30 * time.Minute // no progress write
	allowDownloadingTor  = 24 * time.Hour   // torrents idle legitimately
	allowImportInFlight  = 6 * time.Hour    // downloaded / importing
	overdueNamesInReport = 3
)

var overdueStates = []string{"grabbed", "downloading", "downloaded", "importing"}

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

// OverdueRow is one download that has outlived its state's allowance.
type OverdueRow struct {
	ID    int64
	Title string
	State string
	Age   time.Duration
	Limit time.Duration
}

// Overdue lists downloads older than their state's allowance, oldest first.
func (t *Timings) Overdue(ctx context.Context) ([]OverdueRow, error) {
	if t == nil || t.db == nil {
		return nil, nil
	}
	cands, err := t.db.OverdueCandidates(ctx, overdueStates)
	if err != nil {
		return nil, err
	}
	now := t.now()
	var out []OverdueRow
	for _, c := range cands {
		limit := allowance(c.State, c.Protocol)
		if limit == 0 {
			continue
		}
		age := now.Sub(time.UnixMilli(c.UpdatedAtMs))
		if age > limit {
			out = append(out, OverdueRow{ID: c.ID, Title: c.Title, State: c.State, Age: age, Limit: limit})
		}
	}
	return out, nil
}

// OverdueCheck is the pipeline:overdue health check. It is a warning, never
// an error, and names the rows so there is something to open.
func (t *Timings) OverdueCheck(ctx context.Context) health.Result {
	rows, err := t.Overdue(ctx)
	if err != nil {
		return health.Errorf("cannot read the download queue: %v", err)
	}
	if len(rows) == 0 {
		return health.OK()
	}
	names := make([]string, 0, overdueNamesInReport)
	for i, r := range rows {
		if i == overdueNamesInReport {
			break
		}
		names = append(names, fmt.Sprintf("#%d %q in %s for %s (allowance %s)",
			r.ID, r.Title, r.State, r.Age.Truncate(time.Minute), r.Limit))
	}
	more := ""
	if len(rows) > overdueNamesInReport {
		more = fmt.Sprintf(" and %d more", len(rows)-overdueNamesInReport)
	}
	return health.Warn("%d download(s) have outlived their state's allowance: %s%s. "+
		"Until the deadline column lands (pipeline M2) this judges the time since the row last changed, "+
		"against fixed allowances per state: grabbed 10m, downloading 30m (torrents 24h), downloaded/importing 6h",
		len(rows), strings.Join(names, "; "), more)
}
