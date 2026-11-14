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
		{"removed failed publication", "", "history", sqlite.ImportActivityEntry{State: "failed", FailedPublication: true, ImportedPublication: true}},
		{"new publication after removed failure", "", "queued", sqlite.ImportActivityEntry{State: "failed", FailedPublication: true, ImportedPublication: true, PendingPublication: true}},
		{"ordinary history", "", "imported", sqlite.ImportActivityEntry{State: "imported"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := importActivityCategory(tc.entry, tc.worker); got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}

func TestActivityCategorySeparatesAutomaticWorkFromOperatorDecisions(t *testing.T) {
	for _, tc := range []struct {
		name, worker, want string
		entry              sqlite.ImportActivityEntry
	}{
		{"fetching", "", "progress", sqlite.ImportActivityEntry{ID: 1, State: "downloading"}},
		{"Runner environmental hold", "", "progress", sqlite.ImportActivityEntry{ID: 1, State: "held"}},
		{"interrupted import recovers automatically", "", "progress", sqlite.ImportActivityEntry{ID: 1, State: "importing"}},
		{"approval is explicit", "", "attention", sqlite.ImportActivityEntry{ID: 1, State: "awaiting_import"}},
		{"writer keeps custody", "running", "progress", sqlite.ImportActivityEntry{ID: 1, State: "failed", Review: true}},
		{"cancellation waits for writer", "", "progress", sqlite.ImportActivityEntry{ID: 1, CancelOutcome: "pending"}},
		{"cancellation requires exact disposition", "", "attention", sqlite.ImportActivityEntry{ID: 1, CancelOutcome: "pending", Review: true}},
		{"cancelled without obligations", "", "history", sqlite.ImportActivityEntry{ID: 1, CancelOutcome: "cancelled"}},
		{"uncertain submission", "", "attention", sqlite.ImportActivityEntry{ID: 1, ReservationActionable: true, SubmissionPhase: "uncertain"}},
		{"local import failure", "", "attention", sqlite.ImportActivityEntry{ID: 1, State: "failed", LocalImportFailure: true}},
		{"local environment retries", "", "progress", sqlite.ImportActivityEntry{ID: 1, State: "failed", LocalImportFailure: true, AutomaticImportRetry: true}},
		{"publication failure is replaced", "", "history", sqlite.ImportActivityEntry{ID: 1, State: "failed", LocalImportFailure: true, FailedPublication: true}},
		{"released uncertain reservation", "", "history", sqlite.ImportActivityEntry{ID: 1, SubmissionPhase: "uncertain", Superseded: true}},
		{"automatic unresolved failure remains terminal", "", "history", sqlite.ImportActivityEntry{ID: 1, State: "failed", Unresolved: true}},
		{"manual unresolved files", "", "attention", sqlite.ImportActivityEntry{ID: 1, Protocol: "manual", Unresolved: true, ActionableFiles: true}},
		{"failed attempt with later salvage", "", "history", sqlite.ImportActivityEntry{ID: 1, State: "failed", ImportedPublication: true}},
		{"pending publication on completed download", "", "progress", sqlite.ImportActivityEntry{ID: 1, State: "imported", PendingPublication: true}},
		{"historical records are read only", "", "history", sqlite.ImportActivityEntry{ID: -1, State: "import_failed", Unresolved: true, Review: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := activityCategory(tc.entry, tc.worker); got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}

func TestCancelledImportHistoryRequiresNoCustody(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*sqlite.ImportActivityEntry)
	}{
		{"held stage or placement", func(e *sqlite.ImportActivityEntry) { e.Review = true }},
		{"unresolved files", func(e *sqlite.ImportActivityEntry) { e.Unresolved = true }},
		{"pending publication", func(e *sqlite.ImportActivityEntry) { e.PendingPublication = true }},
		{"failed publication", func(e *sqlite.ImportActivityEntry) { e.FailedPublication = true; e.OpenFailedPublication = true }},
		{"reservation", func(e *sqlite.ImportActivityEntry) { e.ReservationActionable = true }},
		{"actionable files", func(e *sqlite.ImportActivityEntry) { e.ActionableFiles = true }},
		{"approval", func(e *sqlite.ImportActivityEntry) { e.State = "awaiting_import" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := sqlite.ImportActivityEntry{ID: 1, State: "failed", Cancelled: true, CancelOutcome: "cancelled", ImportedPublication: true, FailedPublication: true}
			for _, classify := range []func(sqlite.ImportActivityEntry, string) string{importActivityCategory, activityCategory} {
				if got := classify(e, ""); got != "history" {
					t.Fatalf("resolved cancellation = %s", got)
				}
			}
			tc.change(&e)
			for _, classify := range []func(sqlite.ImportActivityEntry, string) string{importActivityCategory, activityCategory} {
				if got := classify(e, ""); got != "attention" {
					t.Fatalf("custody hidden as %s", got)
				}
			}
			if got := importActivityCategory(e, "running"); got != "active" {
				t.Fatal(got)
			}
			if got := importActivityCategory(e, "queued"); got != "queued" {
				t.Fatal(got)
			}
			if got := activityCategory(e, "running"); got != "progress" {
				t.Fatal(got)
			}
		})
	}
}
