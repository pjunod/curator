package sqlite

import (
	"context"
	"fmt"
	"slices"
	"time"
)

type IndexerBudget struct {
	Unlimited                                 bool
	Total, Interactive, RSS, Search, Headroom int
	RSSInterval                               time.Duration
}

func RequestBudget(cap int) IndexerBudget {
	if cap == 0 {
		return IndexerBudget{Unlimited: true, RSSInterval: 15 * time.Minute}
	}
	i := min(20, cap/10)
	r := min(96, (cap-i)/2)
	s := min(100, max(0, cap-i-r))
	b := IndexerBudget{Total: cap, Interactive: i, RSS: r, Search: s, Headroom: cap - i - r - s}
	if r > 0 {
		b.RSSInterval = time.Duration((1440+r-1)/r) * time.Minute
	}
	return b
}

// BudgetDeferred has a typed wake time and does not consume job attempts.
type BudgetDeferred struct {
	At     time.Time
	Reason string
}

func (e *BudgetDeferred) Error() string {
	return e.Reason + "; retry at " + e.At.UTC().Format(time.RFC3339)
}
func (e *BudgetDeferred) DeferredUntil() time.Time { return e.At }

// ReserveIndexerRequest counts possibly sent calls before network I/O. The
// transaction serializes bucket debits across manual, RSS and automatic work.
func (d *DB) ReserveIndexerRequest(ctx context.Context, id int64, bucket string, automatic bool, now time.Time) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var cap int
	var retry int64
	if err = tx.QueryRowContext(ctx, `SELECT daily_request_cap,retry_at FROM indexers WHERE id=?`, id).Scan(&cap, &retry); err != nil {
		return err
	}
	if now.UnixMilli() < retry {
		return &BudgetDeferred{time.UnixMilli(retry), "provider retry delay"}
	}
	b := RequestBudget(cap)
	actual := bucket
	if !b.Unlimited {
		used := map[string]int{}
		short := 0
		shortRelease := now.Add(12 * time.Hour)
		next := now.Add(24 * time.Hour)
		rows, err := tx.QueryContext(ctx, `SELECT bucket,units,requested_at,automatic FROM indexer_request_usage WHERE indexer_id=? AND requested_at>? ORDER BY requested_at`, id, now.Add(-24*time.Hour).UnixMilli())
		if err != nil {
			return err
		}
		for rows.Next() {
			var k string
			var n, a int
			var ts int64
			if err = rows.Scan(&k, &n, &ts, &a); err != nil {
				break
			}
			used[k] += n
			if a != 0 && ts > now.Add(-12*time.Hour).UnixMilli() {
				short += n
				release := time.UnixMilli(ts).Add(12 * time.Hour)
				if release.Before(shortRelease) {
					shortRelease = release
				}
			}
			release := time.UnixMilli(ts).Add(24 * time.Hour)
			if release.Before(next) {
				next = release
			}
		}
		if err == nil {
			err = rows.Err()
		}
		_ = rows.Close()
		if err != nil {
			return err
		}
		total := 0
		for _, n := range used {
			total += n
		}
		available := false
		// Calls made while unlimited (or under a different cap) may exceed
		// a single bucket's new allocation. Count the combined search pool
		// before borrowing so those calls cannot consume the RSS reserve.
		searchPoolAvailable := total-used["rss"] < b.Total-b.RSS
		if total < b.Total && (bucket == "rss" || searchPoolAvailable) {
			switch bucket {
			case "rss":
				available = used[bucket] < b.RSS
			case "search":
				available = used[bucket] < b.Search && (!automatic || short < (b.Search+1)/2)
			case "interactive":
				switch {
				case used["interactive"] < b.Interactive:
					available = true
				case used["headroom"] < b.Headroom:
					actual = "headroom"
					available = true
				case used["search"] < b.Search:
					actual = "search"
					available = true
				}
			}
		}
		if !available {
			if automatic && short > 0 && shortRelease.Before(next) {
				next = shortRelease
			}
			return &BudgetDeferred{next, "indexer " + bucket + " request allowance exhausted"}
		}
	}
	auto := 0
	if automatic {
		auto = 1
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO indexer_request_usage(indexer_id,requested_at,bucket,units,automatic) VALUES(?,?,?,1,?)`, id, now.UnixMilli(), actual, auto); err != nil {
		return err
	}
	return tx.Commit()
}
func (d *DB) SetIndexerRetryAt(ctx context.Context, id int64, at time.Time) error {
	_, err := d.W.ExecContext(ctx, `UPDATE indexers SET retry_at=max(retry_at,?) WHERE id=?`, at.UnixMilli(), id)
	return err
}
func (d *DB) RSSDue(ctx context.Context, id int64, now time.Time) (bool, time.Duration, error) {
	var cap int
	var due, retry int64
	err := d.R.QueryRowContext(ctx, `SELECT daily_request_cap,next_rss_at,retry_at FROM indexers WHERE id=?`, id).Scan(&cap, &due, &retry)
	b := RequestBudget(cap)
	return now.UnixMilli() >= max(due, retry) && (b.Unlimited || b.RSS > 0), b.RSSInterval, err
}
func (d *DB) MarkRSSDispatched(ctx context.Context, id int64, now time.Time, interval time.Duration) error {
	_, err := d.W.ExecContext(ctx, `UPDATE indexers SET next_rss_at=? WHERE id=?`, now.Add(interval).UnixMilli(), id)
	return err
}

// RequestCapacity returns real rolling release times for discovery feasibility.
func (d *DB) RequestCapacity(ctx context.Context, id int64, now time.Time) (IndexerBudget, []time.Time, []time.Time, error) {
	var cap int
	if err := d.R.QueryRowContext(ctx, `SELECT daily_request_cap FROM indexers WHERE id=?`, id).Scan(&cap); err != nil {
		return IndexerBudget{}, nil, nil, err
	}
	rows, err := d.R.QueryContext(ctx, `SELECT requested_at,automatic,bucket FROM indexer_request_usage WHERE indexer_id=? AND bucket IN ('search','interactive','headroom') AND requested_at>? ORDER BY requested_at`, id, now.Add(-24*time.Hour).UnixMilli())
	if err != nil {
		return IndexerBudget{}, nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	var day, half, manual []time.Time
	for rows.Next() {
		var ts int64
		var a int
		var bucket string
		if err = rows.Scan(&ts, &a, &bucket); err != nil {
			return IndexerBudget{}, nil, nil, err
		}
		at := time.UnixMilli(ts)
		if bucket == "search" {
			day = append(day, at.Add(24*time.Hour))
		} else {
			manual = append(manual, at.Add(24*time.Hour))
		}
		if a != 0 && at.After(now.Add(-12*time.Hour)) {
			half = append(half, at.Add(12*time.Hour))
		}
	}
	b := RequestBudget(cap)
	if !b.Unlimited {
		// After lowering a cap or leaving unlimited mode, prior manual
		// calls may overflow the current reserve and headroom. Their oldest
		// releases free the shared search capacity first; keep the ledger
		// unchanged and include those release times in the forecast.
		overflow := max(0, len(manual)-b.Interactive-b.Headroom)
		day = append(day, manual[:overflow]...)
		slices.SortFunc(day, func(a, b time.Time) int { return a.Compare(b) })
	}
	return b, day, half, rows.Err()
}

// ManualRequestCapacity is RequestCapacity for a manual request, which draws
// on the interactive reserve, then headroom, then unspent search capacity and
// is not held to the twelve-hour share. The times are when each request made
// from that combined pool in the last day returns its token.
func (d *DB) ManualRequestCapacity(ctx context.Context, id int64, now time.Time) (IndexerBudget, []time.Time, error) {
	var cap int
	if err := d.R.QueryRowContext(ctx, `SELECT daily_request_cap FROM indexers WHERE id=?`, id).Scan(&cap); err != nil {
		return IndexerBudget{}, nil, err
	}
	rows, err := d.R.QueryContext(ctx, `SELECT requested_at FROM indexer_request_usage WHERE indexer_id=? AND bucket IN ('interactive','headroom','search') AND requested_at>? ORDER BY requested_at`, id, now.Add(-24*time.Hour).UnixMilli())
	if err != nil {
		return IndexerBudget{}, nil, err
	}
	defer func() { _ = rows.Close() }()
	var day []time.Time
	for rows.Next() {
		var ts int64
		if err = rows.Scan(&ts); err != nil {
			return IndexerBudget{}, nil, err
		}
		day = append(day, time.UnixMilli(ts).Add(24*time.Hour))
	}
	return RequestBudget(cap), day, rows.Err()
}
func (d *DB) SetDailyRequestCap(ctx context.Context, id int64, cap int) error {
	if cap < 0 {
		return fmt.Errorf("daily request cap cannot be negative")
	}
	_, err := d.W.ExecContext(ctx, `UPDATE indexers SET daily_request_cap=? WHERE id=?`, cap, id)
	return err
}
