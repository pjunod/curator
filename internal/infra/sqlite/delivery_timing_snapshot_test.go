package sqlite

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/ports"
	"github.com/pressly/goose/v3"
)

func timingNumber(value int64) *int64 { return &value }
func TestDeliveryTimingImmutableAndPerCopyCleanupMutableAcrossRestart(t *testing.T) {
	ctx := context.Background()
	db, r, _ := atomicDeliveryFixture(t)
	v := r.Delivery
	v.DeliveryID = "timed-delivery"
	v.Sequence++
	v.RunnerStages = &ports.RunnerStages{NodeID: "worker", ClockDomain: "worker:post", Spans: []ports.RunnerStageSpan{{Stage: "unpack", StartedAtUnix: 1700000000, Milliseconds: timingNumber(20)}}}
	v.LifecycleTiming = &ports.RunnerLifecycleTiming{Version: 1, Intervals: []ports.RunnerLifecycleInterval{{Stage: "post_processing", NodeID: "worker", ClockDomain: "worker:boot-1", StartUnixMS: 1700000000000, StartMonotonicMS: timingNumber(10), EndMonotonicMS: timingNumber(50), DurationMS: timingNumber(40), State: "completed"}}}
	v.LifecycleTiming.OmittedIntervals = map[string]uint32{"queue": 2}
	v.SourceCopies = []ports.RunnerSourceCopy{{ID: "worker-copy", OwnerNode: "worker", Status: "pending"}}
	saved, err := db.SaveDiscoveredDelivery(ctx, r.ClientID, v, r.MappedPath)
	if err != nil {
		t.Fatal(err)
	}
	v.SourceCopies[0].Status = "removed"
	if _, err := db.SaveDiscoveredDelivery(ctx, r.ClientID, v, r.MappedPath); err != nil {
		t.Fatal(err)
	}
	if err := db.RefreshDeliverySourceCleanup(ctx, saved.ID, json.RawMessage(`{"status":"succeeded"}`), nil, time.Hour, v.SourceCopies); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(db.Path)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	actual, err := reopened.DeliveryReceiptByID(ctx, saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if actual.State == "conflict" || !ports.SameRunnerStages(actual.Delivery.RunnerStages, v.RunnerStages) || !ports.SameRunnerLifecycleTiming(actual.Delivery.LifecycleTiming, v.LifecycleTiming) || len(actual.Delivery.SourceCopies) != 1 || actual.Delivery.SourceCopies[0].Status != "removed" {
		t.Fatalf("timing/copy refresh lost evidence: %+v", actual.Delivery)
	}
	if err := goose.DownToContext(ctx, reopened.W, "migrations", 66); err == nil {
		t.Fatal("downgrade discarded durable stage/copy evidence")
	}
	// A refused multi-step downgrade may have safely removed newer empty
	// schemas before the timing-evidence guard. Restore the current schema
	// before invoking the current query layer again.
	if err := reopened.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	*v.LifecycleTiming.Intervals[0].DurationMS = 41
	conflict, err := reopened.SaveDiscoveredDelivery(ctx, r.ClientID, v, r.MappedPath)
	if err != nil || conflict.State != "conflict" {
		t.Fatalf("changed immutable timing accepted: %+v %v", conflict, err)
	}
}
