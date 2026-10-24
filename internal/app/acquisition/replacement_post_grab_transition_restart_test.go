package acquisition

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// A Runner admission can be durable before the replacement operation's state
// transition is written. If that transition transaction fails, a restart must
// discover the exact active attempt and avoid submitting a second release.
func TestReplacementWorkerRecoversGrabbedAttemptAfterReplacingStepWriteFailure(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setup(t, []ports.Release{rel("Test.Show.S01E01.1080p.WEB-DL.x264-STEP-RETRY", 25)}, client)
	ctx := context.Background()
	clients, err := db.ListDownloadClients(ctx)
	if err != nil || len(clients) == 0 {
		t.Fatalf("download clients=%+v err=%v", clients, err)
	}
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil || len(item.Seasons) == 0 || len(item.Seasons[0].Episodes) == 0 {
		t.Fatalf("series fixture item=%+v err=%v", item, err)
	}
	op, _, err := db.CreateReplacementOperation(ctx, sqlite.ReplacementOperation{
		ID: "worker-replacing-step-retry", RequestID: "worker-replacing-step-retry", Fingerprint: "worker-replacing-step-retry-fingerprint",
		State: "searching", Intent: "replace_bad", Scope: "episodes", MediaItemID: itemID, EpisodeIDs: []int64{item.Seasons[0].Episodes[0].ID},
		ReasonCode: "bad_media", Reason: "recover admission when the operation-step write fails", CreatedAt: time.Now(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER fail_replacement_replacing_step BEFORE INSERT ON replacement_operation_steps WHEN NEW.operation_id='worker-replacing-step-retry' AND NEW.step='replacing' BEGIN SELECT RAISE(ABORT,'simulated replacing step write failure'); END`); err != nil {
		t.Fatal(err)
	}

	err = svc.processReplacementOperation(ctx, op)
	if err == nil || !strings.Contains(err.Error(), "simulated replacing step write failure") {
		t.Fatalf("worker did not surface the failed operation-step transaction: %v", err)
	}
	interrupted, err := db.GetReplacementOperation(ctx, op.ID)
	if err != nil || interrupted.State != "searching" {
		t.Fatalf("failed state transition was not rolled back atomically: operation=%+v err=%v", interrupted, err)
	}
	rows, err := db.ListReplacementDownloads(ctx, op.ID)
	if err != nil || len(rows) != 1 || rows[0].State != "grabbed" || rows[0].Transfer == "" || rows[0].GenerationKey == "" {
		t.Fatalf("successful admission did not remain as the durable exact attempt: downloads=%+v err=%v", rows, err)
	}
	admittedID, admittedGeneration := rows[0].ID, rows[0].GenerationKey
	admittedTransfer, admittedHandle := rows[0].Transfer, rows[0].Handle
	if len(client.added) != 1 {
		t.Fatalf("initial worker pass made unexpected admission calls: %v", client.added)
	}
	var replacingSteps int
	if err := db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM replacement_operation_steps WHERE operation_id=? AND step='replacing'`, op.ID).Scan(&replacingSteps); err != nil || replacingSteps != 0 {
		t.Fatalf("failed transition left a partial step row: count=%d err=%v", replacingSteps, err)
	}

	reopened := reopenLifecycleDB(t, db)
	if _, err := reopened.W.ExecContext(ctx, `DROP TRIGGER fail_replacement_replacing_step`); err != nil {
		t.Fatal(err)
	}
	restarted := New(reopened, svc.bus, svc.log, svc.newIndexer, svc.newClient)
	restarted.processReplacementOperations(ctx)

	resumed, err := reopened.GetReplacementOperation(ctx, op.ID)
	if err != nil || resumed.State != "replacing" || !strings.Contains(resumed.LastError, "replacement download is still active") {
		t.Fatalf("restart sweep did not reconcile the existing active attempt: operation=%+v err=%v", resumed, err)
	}
	rows, err = reopened.ListReplacementDownloads(ctx, op.ID)
	if err != nil || len(rows) != 1 || rows[0].ID != admittedID || rows[0].GenerationKey != admittedGeneration || rows[0].Transfer != admittedTransfer || rows[0].Handle != admittedHandle {
		t.Fatalf("restart sweep changed the exact admitted attempt: downloads=%+v err=%v", rows, err)
	}
	if len(client.added) != 1 {
		t.Fatalf("restart sweep duplicated Runner admission: %v", client.added)
	}
	var stepState, stepError string
	if err := reopened.R.QueryRowContext(ctx, `SELECT state,error FROM replacement_operation_steps WHERE operation_id=? AND step='replacing'`, op.ID).Scan(&stepState, &stepError); err != nil {
		t.Fatalf("successful replay did not persist a replacing step: state=%q err=%v", stepState, err)
	}
	if stepState != "blocked" || !strings.Contains(stepError, "replacement download is still active") {
		t.Fatalf("replay step did not explain the durable active attempt: state=%q error=%q", stepState, stepError)
	}
}
