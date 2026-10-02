package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/ports"
)

func TestRequestBudgetArithmetic(t *testing.T) {
	for _, tt := range []struct{ cap, i, r, s, minutes int }{{0, 20, 96, 100, 15}, {200, 20, 90, 90, 16}, {100, 10, 45, 45, 32}, {50, 5, 22, 23, 66}} {
		b := RequestBudget(tt.cap)
		if b.Interactive != tt.i || b.RSS != tt.r || b.Search != tt.s || int(b.RSSInterval/time.Minute) != tt.minutes {
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
