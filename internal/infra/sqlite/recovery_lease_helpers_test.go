package sqlite

import (
	"context"
	"encoding/json"
	"testing"
)

// leasedRecoveryData is a recovery import's JSON holding the current lease
// for its title, as QueueRecovery records it.
func leasedRecoveryData(t *testing.T, db *DB, itemID int64) string {
	t.Helper()
	lease, err := db.RecoveryLease(context.Background(), itemID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"request": map[string]any{"media_item_id": itemID}, "accepted_lease": lease})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// acceptCurrentRecoveryLease re-accepts the title's current lease, as a
// fresh preview would.
func acceptCurrentRecoveryLease(t *testing.T, db *DB, id string) {
	t.Helper()
	ctx := context.Background()
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = advanceRecoveryRevision(ctx, tx, id); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// touchTitle changes a title the way a library edit does.
func touchTitle(t *testing.T, db *DB, itemID int64) {
	t.Helper()
	if _, err := db.W.ExecContext(context.Background(), `UPDATE media_items SET title=title||'' WHERE id=?`, itemID); err != nil {
		t.Fatal(err)
	}
}
