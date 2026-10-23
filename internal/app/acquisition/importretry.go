package acquisition

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
)

// Finishing an import that the machine interrupted.
//
// An import that stopped because the destination was full is the one
// failure that fixes itself: somebody frees space, or the previous sweep's
// cleanup does, and the same payload imports without a single decision
// changing. Before this, it sat in `failed` forever waiting for a human to
// notice — and the item stayed "missing", so automation grabbed the same
// release again instead. That is the re-grab loop with monarr's name on it.
//
// Environmental import failures stay retryable until the underlying issue is
// fixed. A bounded sweep and increasing delay protect the worker pool while
// avoiding a permanent stop after an arbitrary number of attempts.

const (
	// JobImportRetry is the scheduled sweep's task name.
	JobImportRetry = "downloads.import-retry"
	// ImportRetryInterval is how often the sweep runs.
	ImportRetryInterval = 30 * time.Second
	// importRetryPerSweep bounds one pass, like the cleanup sweep: a dozen
	// 60 GB season packs is not something to start all at once.
	importRetryPerSweep = 5
	// stepImportRetry marks a retry in the handoff trace, and is what the
	// attempt counter counts.
	stepImportRetry = "import_retry"
)

// importRetryBackoff is the minimum gap between two attempts at the same
// download — long enough that a volume someone is clearing has time to be
// cleared. A var so a test can retry without sleeping through it; nothing
// in the daemon writes to it.
var importRetryBackoff = 30 * time.Second

// HistoryImportRetried records a retry, so a download that eventually
// imported after three attempts says so rather than looking like it just
// worked.
const HistoryImportRetried = "import_retried"

// RetryBlockedImports re-runs the imports that stopped on the environment
// and are worth trying again. Returns nothing on an empty backlog; a
// failure to retry one download never stops the others.
func (s *Service) RetryBlockedImports(ctx context.Context) error {
	rows, err := s.db.ListImportRetryCandidates(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	started := 0
	for _, dl := range rows {
		if started >= importRetryPerSweep {
			return nil
		}
		if !blockedImport(dl) {
			continue
		}
		path := dl.ImportPath
		if path == "" {
			path = dl.SavePath
		}
		legacy, reviewErr := s.db.LegacyCleanupReview(ctx, dl.GenerationKey, path)
		if reviewErr != nil || legacy {
			// Keep the migration-era cohort out of even automatic state
			// transitions until its path and copy have been reviewed.
			continue
		}
		attempts := importAttempts(dl)
		if last, ok := lastImportAttempt(dl); ok && now.Sub(last) < importRetryDelay(attempts) {
			continue
		}
		started++
		s.log.Info("import retry: the destination may have room now",
			"download", dl.ID, "release", dl.ReleaseTitle, "attempt", attempts+1)
		decisionCtx, releaseStorage := s.lockStorageDecision(ctx)
		row, err := s.db.GetDownload(decisionCtx, dl.ID)
		if err != nil || !blockedImport(row) {
			releaseStorage()
			continue
		}
		path = row.ImportPath
		if path == "" {
			path = row.SavePath
		}
		legacy, reviewErr = s.db.LegacyCleanupReview(decisionCtx, row.GenerationKey, path)
		if reviewErr != nil || legacy {
			releaseStorage()
			continue
		}
		s.advance(decisionCtx, &row, "importing", row.Progress, "", stepImportRetry,
			fmt.Sprintf("retrying the import (attempt %d)", attempts+1))
		_ = s.db.AddHistory(ctx, HistoryImportRetried, row.MediaItemID, row.ReleaseTitle,
			map[string]any{"attempt": attempts + 1})
		releaseStorage()
		if err := s.runImport(ctx, row); err != nil {
			s.log.Debug("import retry: still blocked",
				"download", row.ID, "release", row.ReleaseTitle, "err", err)
		}
	}
	return nil
}

// blockedImport reports whether a row is a failed import that stopped on
// the machine rather than on the payload — the only kind worth repeating
// unchanged. The marker is the error text `failImport` recorded, which is
// `ErrImportBlocked`'s message: keeping it in the row rather than in a new
// column is what lets this ship without a migration, and the text is
// asserted in a test so it cannot drift silently.
func blockedImport(dl sqlite.Download) bool {
	return dl.State == "failed" &&
		!dl.DeliveryExpected &&
		strings.Contains(dl.Error, ErrImportBlocked.Error()) &&
		(dl.ImportPath != "" || dl.SavePath != "")
}

func importRetryDelay(attempts int) time.Duration {
	if importRetryBackoff == 0 { // test seam; production leaves the default intact.
		return 0
	}
	delays := [...]time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute}
	if attempts < 0 {
		attempts = 0
	}
	if attempts >= len(delays) {
		return delays[len(delays)-1]
	}
	return delays[attempts]
}

// ImportRetryStatus is the durable Activity-facing retry view. The trace
// stores attempts and their timestamps, so dismissal of the queue row does
// not reset the schedule.
func (s *Service) ImportRetryStatus(dl sqlite.Download) (attempts int, blockedSince, retryAt time.Time, retryable bool) {
	if !blockedImport(dl) {
		return 0, time.Time{}, time.Time{}, false
	}
	attempts = importAttempts(dl)
	for _, event := range dl.Handoff {
		if event.Step == stepFailed && strings.Contains(event.Detail, ErrImportBlocked.Error()) {
			at := time.UnixMilli(event.At)
			if blockedSince.IsZero() || at.Before(blockedSince) {
				blockedSince = at
			}
		}
	}
	if last, ok := lastImportAttempt(dl); ok {
		retryAt = last.Add(importRetryDelay(attempts))
	}
	return attempts, blockedSince, retryAt, true
}

// importAttempts counts the retries already spent on a row.
func importAttempts(dl sqlite.Download) int {
	n := 0
	for _, e := range dl.Handoff {
		if e.Step == stepImportRetry {
			n++
		}
	}
	return n
}

// lastImportAttempt is when the import last ran — a retry if there was one,
// otherwise the original attempt.
func lastImportAttempt(dl sqlite.Download) (time.Time, bool) {
	var at int64
	for _, e := range dl.Handoff {
		if e.Step == stepImportRetry || e.Step == stepImporting || e.Step == stepFailed {
			if e.At > at {
				at = e.At
			}
		}
	}
	if at == 0 {
		return time.Time{}, false
	}
	return time.UnixMilli(at), true
}
