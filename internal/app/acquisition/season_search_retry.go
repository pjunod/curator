package acquisition

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

// A failed discovery is not evidence that a missing episode is unavailable.
// Start a fresh background comparison after a bounded backoff, while retaining
// the failed job as history. Normal completed-search cooldowns remain intact.
func failedSeasonRetryDelay(recoveries int) time.Duration {
	return min(5*time.Minute*time.Duration(1<<min(max(recoveries, 0), 4)), time.Hour)
}

func (s *Service) recoverFailedSeasonSearches(ctx context.Context) error {
	const batch = 20
	rows, err := s.db.R.QueryContext(ctx, `SELECT j.id,j.payload,j.updated_at FROM jobs j
 WHERE j.kind=? AND j.state='failed' AND j.id>?
 AND j.updated_at<=? AND j.dedupe_key IS NOT NULL
 AND NOT EXISTS(SELECT 1 FROM jobs newer WHERE newer.kind=j.kind AND newer.dedupe_key=j.dedupe_key AND newer.id>j.id)
 ORDER BY j.id LIMIT ?`, SeasonSearchJobKind, s.seasonRetryCursor.Load(), time.Now().Add(-5*time.Minute).UnixMilli(), batch)
	if err != nil {
		return err
	}
	type failedSearch struct {
		id      int64
		payload string
		at      int64
	}
	var failed []failedSearch
	for rows.Next() {
		var f failedSearch
		if err = rows.Scan(&f.id, &f.payload, &f.at); err != nil {
			break
		}
		failed = append(failed, f)
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return err
	}
	// Rotate through terminal records so an unmonitored or satisfied season
	// cannot permanently hide later eligible failures behind the batch limit.
	if len(failed) < batch {
		s.seasonRetryCursor.Store(0)
	} else {
		s.seasonRetryCursor.Store(failed[len(failed)-1].id)
	}
	for _, f := range failed {
		if err = ctx.Err(); err != nil {
			return err
		}
		var old seasonCheckpoint
		if json.Unmarshal([]byte(f.payload), &old) != nil || old.Version != 1 || old.ItemID <= 0 || old.Season < 0 {
			continue
		}
		if time.Since(time.UnixMilli(f.at)) < failedSeasonRetryDelay(old.SearchRetryCount) {
			continue
		}
		fresh := seasonCheckpoint{Version: 1, ItemID: old.ItemID, CopyID: old.CopyID, Season: old.Season, Trigger: "failed_search", BackgroundPriority: 50, SearchRetryCount: old.SearchRetryCount + 1}
		_, pack, profile, targets, _, e := s.seasonSnapshot(ctx, fresh)
		if errors.Is(e, sqlite.ErrNotFound) {
			continue
		}
		if e != nil {
			continue
		} // Storage/configuration is re-read on the next pass.
		missing := false
		for _, ep := range targets {
			missing = missing || !ep.OnDisk()
		}
		if !missing {
			continue
		} // Includes unmonitored, offloaded, reserved and regrab-capped episodes.
		fresh.Fingerprint = discoveryFingerprint(pack, profile, targets)
		raw, e := json.Marshal(fresh)
		if e != nil {
			return e
		}
		// The queue's unique active key preserves concurrent manual work and any
		// provider retry deadline it already owns.
		_, e = s.db.EnqueueFailedJobReplacement(ctx, f.id, domain.Job{Kind: SeasonSearchJobKind, Payload: string(raw), DedupeKey: seasonJobKey(old.ItemID, old.CopyID, old.Season), Priority: 50, MaxAttempts: 3})
		if e != nil {
			return e
		}
	}
	return nil
}
