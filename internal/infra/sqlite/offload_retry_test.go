package sqlite

import (
	"context"
	"testing"
)

// Retrying failed deliveries must not revive one that Re-acquire or Stop
// cancelled (its files are on disk again, or never left): only a delivery
// that genuinely failed is requeued.
func TestRetryFailedDeliveriesSkipsCancelledOffloadRequests(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	rows := []OutboxWrite{
		{NotifierID: 1, Directory: "/m/a", IdempotencyKey: "op/of-1/a", Event: "offload", Payload: `{}`, OperationID: "of-1"},
		{NotifierID: 1, Directory: "/m/b", IdempotencyKey: "op/of-1/b", Event: "offload", Payload: `{}`, OperationID: "of-1"},
	}
	if err := d.EnqueueDeliveries(ctx, rows); err != nil {
		t.Fatal(err)
	}
	if _, err := d.W.ExecContext(ctx, `UPDATE notifier_deliveries SET status='failed',last_error='cancelled: re-acquired before cinema confirmed it' WHERE directory='/m/a'`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.W.ExecContext(ctx, `UPDATE notifier_deliveries SET status='failed',last_error='cinema refused (401)' WHERE directory='/m/b'`); err != nil {
		t.Fatal(err)
	}
	n, err := d.RetryFailedDeliveries(ctx, 1)
	if err != nil || n != 1 {
		t.Fatalf("requeued %d (%v), want only the genuinely failed one", n, err)
	}
	got, _ := d.ListDeliveries(ctx, 1, 10)
	for _, x := range got {
		if x.Directory == "/m/a" && x.Status != "failed" {
			t.Fatalf("a cancelled delivery was revived: %+v", x)
		}
		if x.Directory == "/m/b" && x.Status != "pending" {
			t.Fatalf("a failed delivery was not requeued: %+v", x)
		}
	}
}
