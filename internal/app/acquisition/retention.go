package acquisition

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
)

// Keeping Activity finite.
//
// Every grab monarr has ever made was still in the `downloads` table, and
// every event it ever recorded was still in `history_events`. Nothing
// deleted either. The page rendered whatever the last hundred rows were, so
// the symptom was mild for a long time and then wasn't: a "Finished"
// section that is a hundred receipts deep answers no question anybody has,
// and the table under it grows for as long as the install lives.
//
// So: terminal rows and old events age out, on a daily sweep, with a window
// the user can set (Settings → Activity retention; `0` keeps everything).
// Only terminal rows. Whatever is still moving is never swept out from
// under itself, however old it looks — a download stuck in `grabbed` for
// six weeks is a bug to look at, not litter to hide.

const (
	// JobRetention is the scheduled sweep's task name.
	JobRetention = "activity.retention"
	// RetentionInterval is how often it runs. Daily: this is housekeeping,
	// and an hour either way changes nothing.
	RetentionInterval = 24 * time.Hour
	// RetentionSetting is the app_meta key holding the window in days.
	RetentionSetting = "activity.retention_days"
	// DefaultRetentionDays is the window when nobody has set one. Long
	// enough that "what happened to that grab last month" is still
	// answerable, short enough that the table stays a working record
	// rather than an archive.
	DefaultRetentionDays = 30
)

// RetentionDays is the configured window, or the default. `0` means keep
// everything, and is a deliberate choice a user can make — not the absence
// of a setting.
func (s *Service) RetentionDays(ctx context.Context) int {
	raw, err := s.db.GetMeta(ctx, RetentionSetting)
	if err != nil {
		return DefaultRetentionDays
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 0 {
		return DefaultRetentionDays
	}
	return n
}

// SetRetentionDays stores the window. 0 disables the sweep.
func (s *Service) SetRetentionDays(ctx context.Context, days int) error {
	if days < 0 {
		days = 0
	}
	return s.db.SetMeta(ctx, RetentionSetting, strconv.Itoa(days))
}

// PruneActivity ages out terminal downloads and old history events.
func (s *Service) PruneActivity(ctx context.Context) error {
	days := s.RetentionDays(ctx)
	if days == 0 {
		return nil // "keep everything" is an answer, not an oversight
	}
	before := time.Now().AddDate(0, 0, -days)
	rows, err := s.db.PruneTerminalDownloads(ctx, before)
	if err != nil {
		return err
	}
	events, err := s.db.PruneHistory(ctx, before)
	if err != nil {
		return err
	}
	if rows > 0 || events > 0 {
		s.log.Info("activity retention: aged out",
			"downloads", rows, "events", events, "older_than_days", days)
	}
	return nil
}

// ClearFinished drops the imported rows from the queue view on demand.
//
// The rows only. An imported download's row is a receipt: the files are in
// the library, tracked by the library's own records, and the download
// client's copy was dealt with at import time. Nothing here reads a path.
func (s *Service) ClearFinished(ctx context.Context) (int64, error) {
	n, err := s.db.ClearImportedDownloads(ctx)
	if err != nil {
		return 0, err
	}
	s.log.Info("activity: cleared finished rows", "rows", n)
	return n, nil
}

// QueuePage is one page of the queue for the Activity page.
func (s *Service) QueuePage(ctx context.Context, filter sqlite.QueueFilter, search string, limit, offset int) ([]sqlite.Download, error) {
	return s.db.QueuePage(ctx, filter, search, limit, offset)
}

// QueueCounts is how many rows are in each state.
func (s *Service) QueueCounts(ctx context.Context) (map[string]int64, error) {
	return s.db.QueueCounts(ctx)
}

// HistoryPage is one page of the per-release timeline (item 0 = all).
func (s *Service) HistoryPage(ctx context.Context, item int64, limit, offset int) ([]sqlite.HistoryEvent, error) {
	return s.db.HistoryPage(ctx, item, limit, offset)
}
