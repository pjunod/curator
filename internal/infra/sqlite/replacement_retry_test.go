package sqlite

import (
	"context"
	"testing"
	"time"
)

func TestCleanupBlockedReplacementUsesDurableBackoff(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	op, _, err := db.CreateReplacementOperation(ctx, ReplacementOperation{
		ID: "cleanup-backoff", RequestID: "cleanup-backoff-request", Fingerprint: "fingerprint",
		State: "accepted", Intent: "replace_bad", Scope: "download", MediaItemID: 77,
		ReasonCode: "bad_media", Reason: "bad bytes",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.TransitionReplacementOperation(ctx, op.ID, "cleanup_blocked", "mount unavailable"); err != nil {
		t.Fatal(err)
	}
	first, err := db.GetReplacementOperation(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.RetryAttempts != 1 || first.BlockedSince.IsZero() || time.Until(first.NextAttemptAt) < 20*time.Second {
		t.Fatalf("first blocked retry was not scheduled with durable warning state: %+v", first)
	}
	if err := db.TransitionReplacementOperation(ctx, op.ID, "cleanup_blocked", "mount still unavailable"); err != nil {
		t.Fatal(err)
	}
	second, err := db.GetReplacementOperation(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second.RetryAttempts != 2 || second.BlockedSince.IsZero() || time.Until(second.NextAttemptAt) < 50*time.Second {
		t.Fatalf("second blocked retry did not back off: %+v", second)
	}
	if err := db.TransitionReplacementOperation(ctx, op.ID, "searching", ""); err != nil {
		t.Fatal(err)
	}
	recovered, err := db.GetReplacementOperation(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.RetryAttempts != 0 || !recovered.BlockedSince.IsZero() {
		t.Fatalf("recovered operation retained blocked warning state: %+v", recovered)
	}
}
