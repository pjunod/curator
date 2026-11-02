package sqlite

import (
	"context"
	"encoding/json"
	"github.com/pjunod/monarr/internal/ports"
	"testing"
)

func TestDiscoveryRejectionIsAuditableAndIdempotentWithoutAcceptance(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	v := ports.RunnerDeliveryRejection{Sequence: 7, DeliveryID: "malformed", Reason: "unsupported manifest"}
	for i := 0; i < 2; i++ {
		if e := db.RecordDeliveryDiscoveryRejection(ctx, 1, v); e != nil {
			t.Fatal(e)
		}
	}
	var count int
	var raw string
	if e := db.R.QueryRowContext(ctx, `SELECT count(*),data FROM history_events WHERE type='delivery_discovery_rejected'`).Scan(&count, &raw); e != nil {
		t.Fatal(e)
	}
	var evidence map[string]any
	if e := json.Unmarshal([]byte(raw), &evidence); e != nil {
		t.Fatal(e)
	}
	if count != 1 || evidence["publicationNotAccepted"] != true || evidence["deliveryId"] != "malformed" {
		t.Fatalf("wrong rejection audit: %d %s", count, raw)
	}
	for _, bad := range []ports.RunnerDeliveryRejection{{Reason: "missing sequence"}, {Sequence: 8}} {
		if e := db.RecordDeliveryDiscoveryRejection(ctx, 1, bad); e == nil {
			t.Fatal("unidentified rejection persisted")
		}
	}
}
