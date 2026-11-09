package acquisition

import (
	"testing"

	"github.com/pjunod/monarr/internal/infra/sqlite"
)

func TestImportActivityClassifiesOwnershipBeforePersistedState(t *testing.T) {
	for _, tc := range []struct {
		name, worker, want string
		entry              sqlite.ImportActivityEntry
	}{
		{"running retry of a failure", "running", "active", sqlite.ImportActivityEntry{State: "failed"}},
		{"queued retry of a failure", "queued", "queued", sqlite.ImportActivityEntry{State: "failed"}},
		{"stale importing row", "", "attention", sqlite.ImportActivityEntry{State: "importing"}},
		{"unresolved completed row", "", "attention", sqlite.ImportActivityEntry{State: "imported", Unresolved: true}},
		{"retained stage", "", "attention", sqlite.ImportActivityEntry{State: "imported", Review: true}},
		{"cancelled payload", "", "attention", sqlite.ImportActivityEntry{State: "downloaded", Cancelled: true}},
		{"pending publication", "", "queued", sqlite.ImportActivityEntry{State: "downloaded", PendingPublication: true}},
		{"approval required", "", "attention", sqlite.ImportActivityEntry{State: "awaiting_import", PendingPublication: true}},
		{"new publication on completed download", "", "queued", sqlite.ImportActivityEntry{State: "imported", PendingPublication: true}},
		{"salvage on failed download", "", "imported", sqlite.ImportActivityEntry{State: "failed", ImportedPublication: true}},
		{"salvage on ongoing download", "", "imported", sqlite.ImportActivityEntry{State: "downloading", ImportedPublication: true}},
		{"ordinary history", "", "imported", sqlite.ImportActivityEntry{State: "imported"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := importActivityCategory(tc.entry, tc.worker); got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}
