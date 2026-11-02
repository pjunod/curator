package sqlite

import (
	"context"
	"testing"
)

func TestLateReplacementFailureCannotReopenTerminalOperation(t *testing.T) {
	for _, terminal := range []string{"complete", "cancelled", "failed"} {
		t.Run(terminal, func(t *testing.T) {
			ctx := context.Background()
			db, _ := openLifecycleCoverageDB(t)
			defer db.Close()
			op, _, err := db.CreateReplacementOperation(ctx, ReplacementOperation{ID: "settled", RequestID: "settled", Fingerprint: "settled", State: "searching", Intent: "replace_quality", Scope: "download", MediaItemID: 1, ReasonCode: "quality_preference", Reason: "terminal transition"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.TransitionReplacementOperation(ctx, op.ID, terminal, ""); err != nil {
				t.Fatal(err)
			}
			var initialSteps int
			if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM replacement_operation_steps WHERE operation_id=?`, op.ID).Scan(&initialSteps); err != nil {
				t.Fatal(err)
			}
			for _, late := range []string{"waiting_for_release", "cleanup_blocked", "replacing"} {
				if err := db.TransitionReplacementOperation(ctx, op.ID, late, "late callback"); err != nil {
					t.Fatal(err)
				}
			}
			current, err := db.GetReplacementOperation(ctx, op.ID)
			if err != nil || current.State != terminal || current.LastError != "" || current.RetryAttempts != 0 {
				t.Fatalf("late callback reopened settled work: %+v %v", current, err)
			}
			var steps int
			if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM replacement_operation_steps WHERE operation_id=?`, op.ID).Scan(&steps); err != nil || steps != initialSteps {
				t.Fatalf("late callback appended false progress: steps=%d err=%v", steps, err)
			}
		})
	}
}
