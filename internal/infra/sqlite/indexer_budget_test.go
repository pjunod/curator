package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/ports"
)

func TestRequestBudgetArithmetic(t *testing.T) {
	if b := RequestBudget(0); !b.Unlimited || b.RSSInterval != 15*time.Minute {
		t.Fatalf("zero cap = %+v", b)
	}
	for _, tt := range []struct{ cap, i, r, s, minutes int }{{250, 20, 96, 100, 15}, {200, 20, 90, 90, 16}, {100, 10, 45, 45, 32}, {50, 5, 22, 23, 66}} {
		b := RequestBudget(tt.cap)
		if b.Unlimited || b.Total != tt.cap || b.Interactive != tt.i || b.RSS != tt.r || b.Search != tt.s || int(b.RSSInterval/time.Minute) != tt.minutes {
			t.Fatalf("%d: %+v", tt.cap, b)
		}
	}
}
func TestRollingBudgetAndInteractiveBorrowing(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id, err := db.AddIndexer(ctx, ports.IndexerConfig{Name: "budget", URL: "http://example.invalid", Protocol: "torrent", Enabled: true, DailyRequestCap: 50})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for n := 0; n < 28; n++ {
		if err = db.ReserveIndexerRequest(ctx, id, "interactive", false, now); err != nil {
			t.Fatalf("token %d: %v", n, err)
		}
	}
	var deferred *BudgetDeferred
	if err = db.ReserveIndexerRequest(ctx, id, "interactive", false, now); !errors.As(err, &deferred) {
		t.Fatal("interactive borrowed RSS")
	}
	for n := 0; n < 22; n++ {
		if err = db.ReserveIndexerRequest(ctx, id, "rss", false, now); err != nil {
			t.Fatal(err)
		}
	}
	if err = db.ReserveIndexerRequest(ctx, id, "rss", false, now); !errors.As(err, &deferred) {
		t.Fatal("total cap exceeded")
	}
	if err = db.ReserveIndexerRequest(ctx, id, "rss", false, now.Add(24*time.Hour+time.Second)); err != nil {
		t.Fatal(err)
	}
}

func TestProviderRetryAtIsMonotoneAndDefersAllBuckets(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id, err := db.AddIndexer(ctx, ports.IndexerConfig{Name: "retry", URL: "http://example.invalid", Protocol: "torrent", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	at := now.Add(time.Hour)
	if err = db.SetIndexerRetryAt(ctx, id, at); err != nil {
		t.Fatal(err)
	}
	_ = db.SetIndexerRetryAt(ctx, id, now.Add(time.Minute))
	for _, bucket := range []string{"rss", "interactive", "search"} {
		err = db.ReserveIndexerRequest(ctx, id, bucket, false, now)
		var deferred *BudgetDeferred
		if !errors.As(err, &deferred) || deferred.DeferredUntil().Before(at.Add(-time.Millisecond)) || deferred.Error() == "" {
			t.Fatalf("%s did not retain RetryAt: %v", bucket, err)
		}
	}
}

func TestZeroCapIsUnlimitedAndRetainsUsage(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id, err := db.AddIndexer(ctx, ports.IndexerConfig{Name: "unlimited", URL: "http://example.invalid", Protocol: "torrent", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, bucket := range []string{"interactive", "search", "rss"} {
		for n := 0; n < 300; n++ {
			if err := db.ReserveIndexerRequest(ctx, id, bucket, bucket != "interactive", now); err != nil {
				t.Fatalf("%s request %d: %v", bucket, n, err)
			}
		}
	}
	var total int
	if err := db.R.QueryRowContext(ctx, `SELECT sum(units) FROM indexer_request_usage WHERE indexer_id=?`, id).Scan(&total); err != nil || total != 900 {
		t.Fatalf("usage = %d, err = %v", total, err)
	}
	automatic, day, half, err := db.RequestCapacity(ctx, id, now)
	if err != nil || !automatic.Unlimited || len(day) != 300 || len(half) != 300 {
		t.Fatalf("automatic capacity = %+v, day=%d half=%d err=%v", automatic, len(day), len(half), err)
	}
	manual, day, err := db.ManualRequestCapacity(ctx, id, now)
	if err != nil || !manual.Unlimited || len(day) != 600 {
		t.Fatalf("manual capacity = %+v, day=%d err=%v", manual, len(day), err)
	}
	// An explicit cap must count calls made while unlimited, and zero must
	// immediately remove the local block without erasing that history.
	if err := db.SetDailyRequestCap(ctx, id, 250); err != nil {
		t.Fatal(err)
	}
	var deferred *BudgetDeferred
	if err := db.ReserveIndexerRequest(ctx, id, "interactive", false, now); !errors.As(err, &deferred) {
		t.Fatalf("finite cap ignored previous usage: %v", err)
	}
	if err := db.SetDailyRequestCap(ctx, id, 0); err != nil {
		t.Fatal(err)
	}
	if err := db.ReserveIndexerRequest(ctx, id, "interactive", false, now); err != nil {
		t.Fatal(err)
	}
	if err := db.SetDailyRequestCap(ctx, id, 250); err != nil {
		t.Fatal(err)
	}
	if err := db.ReserveIndexerRequest(ctx, id, "search", true, now); !errors.As(err, &deferred) {
		t.Fatalf("switching caps erased usage: %v", err)
	}
	if err := db.ReserveIndexerRequest(ctx, id, "search", true, now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
}

func TestUnlimitedRSSRetainsPacingAndProviderRetry(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id, err := db.AddIndexer(ctx, ports.IndexerConfig{Name: "unlimited-rss", URL: "http://example.invalid", Protocol: "torrent", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Millisecond)
	due, interval, err := db.RSSDue(ctx, id, now)
	if err != nil || !due || interval != 15*time.Minute {
		t.Fatalf("due=%v interval=%v err=%v", due, interval, err)
	}
	if err := db.MarkRSSDispatched(ctx, id, now, interval); err != nil {
		t.Fatal(err)
	}
	due, _, err = db.RSSDue(ctx, id, now.Add(time.Minute))
	if err != nil || due {
		t.Fatalf("RSS polled before its interval: due=%v err=%v", due, err)
	}
	due, _, err = db.RSSDue(ctx, id, now.Add(interval))
	if err != nil || !due {
		t.Fatalf("RSS not due after its interval: due=%v err=%v", due, err)
	}
	retry := now.Add(time.Hour)
	if err := db.SetIndexerRetryAt(ctx, id, retry); err != nil {
		t.Fatal(err)
	}
	due, _, err = db.RSSDue(ctx, id, now.Add(interval))
	if err != nil || due {
		t.Fatalf("RSS ignored retry delay: due=%v err=%v", due, err)
	}
	for _, bucket := range []string{"interactive", "search", "rss"} {
		var deferred *BudgetDeferred
		if err := db.ReserveIndexerRequest(ctx, id, bucket, bucket != "interactive", now); !errors.As(err, &deferred) || !deferred.At.Equal(retry) {
			t.Fatalf("%s ignored provider delay: %v", bucket, err)
		}
		if err := db.ReserveIndexerRequest(ctx, id, bucket, bucket != "interactive", retry); err != nil {
			t.Fatal(err)
		}
	}
	due, _, err = db.RSSDue(ctx, id, retry)
	if err != nil || !due {
		t.Fatalf("RSS remained blocked after retry: due=%v err=%v", due, err)
	}
}

func TestRestoringCapPreservesRSSReserveAfterUnlimitedSearches(t *testing.T) {
	for _, bucket := range []string{"interactive", "search"} {
		t.Run(bucket, func(t *testing.T) {
			db := openTestDB(t)
			ctx := context.Background()
			id, err := db.AddIndexer(ctx, ports.IndexerConfig{Name: "transition", URL: "http://example.invalid", Protocol: "torrent", Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			for n := 0; n < 100; n++ {
				if err := db.ReserveIndexerRequest(ctx, id, "interactive", false, now); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.SetDailyRequestCap(ctx, id, 250); err != nil {
				t.Fatal(err)
			}
			budget, day, half, err := db.RequestCapacity(ctx, id, now)
			if err != nil || budget.Unlimited || len(day) != 46 || len(half) != 0 {
				t.Fatalf("forecast after restoring cap: budget=%+v day=%d half=%d err=%v", budget, len(day), len(half), err)
			}
			// 154 combined search calls minus the 100 already recorded. Automatic
			// calls retain their separate 50-call half-day allowance as well.
			available := 54
			if bucket == "search" {
				available = 50
			}
			for n := 0; n < available; n++ {
				if err := db.ReserveIndexerRequest(ctx, id, bucket, bucket == "search", now); err != nil {
					t.Fatalf("request %d: %v", n, err)
				}
			}
			var deferred *BudgetDeferred
			if err := db.ReserveIndexerRequest(ctx, id, bucket, bucket == "search", now); !errors.As(err, &deferred) {
				t.Fatalf("restored cap allowed excess %s: %v", bucket, err)
			}
			// Once the half-day share resets, only four search calls remain.
			if bucket == "search" {
				for n := 0; n < 4; n++ {
					if err := db.ReserveIndexerRequest(ctx, id, bucket, true, now.Add(12*time.Hour)); err != nil {
						t.Fatal(err)
					}
				}
				if err := db.ReserveIndexerRequest(ctx, id, bucket, true, now.Add(12*time.Hour)); !errors.As(err, &deferred) {
					t.Fatalf("search borrowed RSS: %v", err)
				}
			}
			for n := 0; n < 96; n++ {
				if err := db.ReserveIndexerRequest(ctx, id, "rss", true, now.Add(12*time.Hour)); err != nil {
					t.Fatalf("RSS request %d: %v", n, err)
				}
			}
		})
	}
}
