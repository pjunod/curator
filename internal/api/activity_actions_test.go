package api

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/app/acquisition"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

func insertActivityApprovalJob(t *testing.T, db *sqlite.DB, item int64, title string) sqlite.Download {
	t.Helper()
	job, err := db.InsertDownloadRecord(context.Background(), sqlite.Download{
		MediaItemID: item, State: "awaiting_import", Protocol: "manual", ReleaseTitle: title,
	})
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func TestActivitySelectionFreezesEveryMatchingPage(t *testing.T) {
	e := newAPIEnv(t)
	ctx := context.Background()
	item := e.addMovie(t)

	const matchingJobs = 52
	wantTargets := make(map[int64]string, matchingJobs)
	var excluded sqlite.Download
	for i := range matchingJobs {
		job := insertActivityApprovalJob(t, e.db, item, fmt.Sprintf("selection-fixture-%02d", i))
		wantTargets[job.ID] = job.GenerationKey
		// The oldest job lies on page two with the default 50-row page size.
		if i == 0 {
			excluded = job
		}
	}
	unrelated := insertActivityApprovalJob(t, e.db, item, "different release")
	var selection struct {
		ID        string                    `json:"id"`
		Total     int                       `json:"total"`
		Retryable int                       `json:"retryable"`
		Targets   []sqlite.ActivityIdentity `json:"targets"`
	}
	e.post(t, "/api/v1/activity/selections", `{"view":"attention","scope":"imports","q":"selection-fixture-"}`).
		expect(t, http.StatusOK).into(t, &selection)
	if selection.ID == "" || selection.Total != matchingJobs || selection.Retryable != 0 || len(selection.Targets) != matchingJobs {
		t.Fatalf("missing frozen jobs or approval counted as retry: %+v", selection)
	}
	frozen := make(map[int64]string, matchingJobs)
	for _, target := range selection.Targets {
		if target.Epoch != "0" {
			t.Fatalf("wrong frozen attempt epoch: %+v", target)
		}
		frozen[target.ID] = target.GenerationKey
	}
	if !maps.Equal(frozen, wantTargets) {
		t.Fatalf("incorrect frozen identities: %v", frozen)
	}

	arrival := insertActivityApprovalJob(t, e.db, item, "selection-fixture-arrived-later")
	body := fmt.Sprintf(`{"requestId":"frozen-selection-request","action":"delete","selection":{"id":%q,"excludeIds":[%d]}}`, selection.ID, excluded.ID)
	var operation sqlite.ActivityOperation
	e.post(t, "/api/v1/activity/actions", body).expect(t, http.StatusAccepted).into(t, &operation)
	if operation.Total != matchingJobs-1 || operation.State != "running" || operation.Completed != 0 || len(operation.Targets) != matchingJobs-1 {
		t.Fatalf("unexpected frozen operation: %+v", operation)
	}
	delete(wantTargets, excluded.ID)
	submitted := make(map[int64]string, matchingJobs-1)
	for _, target := range operation.Targets {
		if target.State != "pending" {
			t.Fatalf("unexpected operation target state: %+v", target)
		}
		submitted[target.ID] = target.GenerationKey
	}
	if !maps.Equal(submitted, wantTargets) {
		t.Fatalf("incorrect submitted identities: %v", submitted)
	}
	for _, job := range []sqlite.Download{excluded, arrival, unrelated} {
		deleting, err := e.db.DownloadDeletionRequested(ctx, job.ID, job.GenerationKey)
		if err != nil || deleting {
			t.Fatalf("deletion escaped the selected jobs for %q: deleting=%v, err=%v", job.ReleaseTitle, deleting, err)
		}
	}
	// Expiry removes the authority to start a new operation, while a client
	// recovering a lost response can still replay the original command.
	if _, err := e.db.W.ExecContext(ctx, `UPDATE activity_selections SET created_at=? WHERE id=?`, time.Now().Add(-48*time.Hour).UnixMilli(), selection.ID); err != nil {
		t.Fatal(err)
	}
	var replay sqlite.ActivityOperation
	e.post(t, "/api/v1/activity/actions", body).expect(t, http.StatusAccepted).into(t, &replay)
	if replay.ID != operation.ID || replay.Total != operation.Total {
		t.Fatalf("expired snapshot replay returned a different operation: %+v", replay)
	}
	expired := strings.Replace(body, "frozen-selection-request", "new-expired-selection-request", 1)
	e.post(t, "/api/v1/activity/actions", expired).expect(t, http.StatusConflict)

}

func TestActivityActionReplayAndPrivateOwnerReceiptSurviveReopen(t *testing.T) {
	e := newAPIEnv(t)
	ctx := context.Background()
	item := e.addMovie(t)
	current := insertActivityApprovalJob(t, e.db, item, "Current attempt")
	changed := insertActivityApprovalJob(t, e.db, item, "Replacement generation")
	const requestID = "deletion-action-persisted"
	body := fmt.Sprintf(`{"requestId":%q,"action":"delete","targets":[{"id":%d,"generationKey":%q,"epoch":"0"},{"id":%d,"generationKey":"previous-generation","epoch":"0"}]}`, requestID, current.ID, current.GenerationKey, changed.ID)

	var first sqlite.ActivityOperation
	e.post(t, "/api/v1/activity/actions", body).expect(t, http.StatusAccepted).into(t, &first)
	if first.ID != requestID || first.Total != 2 || first.Completed != 1 || first.Blocked != 1 || len(first.Targets) != 2 || first.Targets[0].State != "pending" || first.Targets[1].State != "blocked" {
		t.Fatalf("a changed attempt should block only that target: %+v", first)
	}
	deleting, err := e.db.DownloadDeletionRequested(ctx, changed.ID, changed.GenerationKey)
	if err != nil || deleting {
		t.Fatalf("changed target acquired a deletion intent: deleting=%v err=%v", deleting, err)
	}

	stored, err := e.db.ActivityOperation(ctx, requestID)
	if err != nil {
		t.Fatal(err)
	}
	const privateReceipt = "worker-only disposition request"
	stored.Targets[0].OwnerPayload = privateReceipt
	stored.Targets[0].State = "blocked"
	stored.Targets[0].Message = "Reconnect the download drive."
	if err := e.db.SaveActivityOperation(ctx, &stored); err != nil {
		t.Fatal(err)
	}

	var databasePath string
	if err := e.db.R.QueryRowContext(ctx, `SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&databasePath); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.Open(filepath.Dir(databasePath))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	deps := e.srv.deps
	deps.Store = reopened
	deps.Settings = reopened
	deps.Acquisition = acquisition.New(reopened, e.bus, nil, deps.IndexerFactory, deps.ClientFactory)
	srv := New(deps)
	t.Cleanup(func() { srv.BeginShutdown(); srv.CancelRequests(); srv.Wait() })
	handler := srv.Handler()
	persisted, err := reopened.ActivityOperation(ctx, requestID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Targets[0].OwnerPayload != privateReceipt {
		t.Fatal("the new store lost the writer's disposition receipt")
	}

	checkResult := func(response *httptestRecorder, status int) {
		t.Helper()
		response.expect(t, status)
		if text := response.Body.String(); strings.Contains(text, "ownerPayload") || strings.Contains(text, privateReceipt) {
			t.Fatalf("public operation leaked the writer's receipt: %s", text)
		}
		var result sqlite.ActivityOperation
		response.into(t, &result)
		if result.ID != requestID || result.State != "completed" || result.Blocked != 2 || len(result.Targets) != 2 || result.Targets[0].Message != "Reconnect the download drive." {
			t.Fatalf("reconstructed server lost action results: %+v", result)
		}
	}
	checkResult(&httptestRecorder{do(t, handler, http.MethodGet, "/api/v1/activity/operations/"+requestID, "")}, http.StatusOK)
	checkResult(&httptestRecorder{do(t, handler, http.MethodPost, "/api/v1/activity/actions", body)}, http.StatusAccepted)
	conflict := strings.Replace(body, `"delete"`, `"retry"`, 1)
	(&httptestRecorder{do(t, handler, http.MethodPost, "/api/v1/activity/actions", conflict)}).expect(t, http.StatusConflict)

	var workers int
	if err := reopened.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE kind=? AND dedupe_key=?`, sqlite.ActivityActionJobKind, "activity:"+requestID).Scan(&workers); err != nil || workers != 1 {
		t.Fatalf("idempotent replay queued extra work: workers=%d err=%v", workers, err)
	}
}
