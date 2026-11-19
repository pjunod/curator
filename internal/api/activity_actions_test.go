package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	apigen "github.com/pjunod/monarr/internal/api/gen"
	"github.com/pjunod/monarr/internal/app/acquisition"
	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
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
	if _, err := e.db.W.ExecContext(ctx, `UPDATE download_dispositions SET owner_payload=? WHERE download_id=? AND generation_key=?`,
		privateReceipt, current.ID, current.GenerationKey); err != nil {
		t.Fatal(err)
	}
	current.Handoff = []sqlite.HandoffEntry{{At: 1000, Step: "copy_started", Detail: "Copy began."}}
	if err := e.db.UpdateDownloadHandoff(ctx, current); err != nil {
		t.Fatal(err)
	}
	stored.Targets[0].State = "running"
	stored.Targets[0].Message = "Waiting for the writer."
	if err := e.db.SaveActivityTarget(ctx, &stored, 0); err != nil {
		t.Fatal(err)
	}
	stored.Targets[0].OwnerPayload = privateReceipt
	stored.Targets[0].State = "blocked"
	stored.Targets[0].Message = "Reconnect the download drive."
	if err := e.db.SaveActivityTarget(ctx, &stored, 0); err != nil {
		t.Fatal(err)
	}
	// Adjacent source timestamps inside the same second must keep their order.
	if _, err := e.db.W.ExecContext(ctx, `UPDATE activity_action_events SET at=CASE step WHEN 'running' THEN 1100 ELSE 1200 END
		WHERE operation_id=? AND download_id=? AND step IN ('running','blocked')`, requestID, current.ID); err != nil {
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
	var canonical string
	if err := reopened.R.QueryRowContext(ctx, `SELECT owner_payload FROM download_dispositions WHERE download_id=? AND generation_key=?`,
		current.ID, current.GenerationKey).Scan(&canonical); err != nil || canonical != privateReceipt {
		t.Fatalf("canonical owner request did not survive reopen: %v", err)
	}
	if len(persisted.Targets) != 2 || persisted.Targets[0].OwnerPayload != privateReceipt {
		t.Fatal("the new store lost the writer's operation projection")
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

	var log struct {
		Events []struct{ Message string } `json:"events"`
	}
	jobLog := &httptestRecorder{do(t, handler, http.MethodGet, fmt.Sprintf("/api/v1/activity/jobs/%d/log", current.ID), "")}
	jobLog.expect(t, http.StatusOK).into(t, &log)
	var messages []string
	for _, event := range log.Events {
		switch event.Message {
		case "Copy began.", "Waiting for the writer.", "Reconnect the download drive.":
			messages = append(messages, event.Message)
		}
	}
	if !slices.Equal(messages, []string{"Copy began.", "Waiting for the writer.", "Reconnect the download drive."}) {
		t.Fatalf("job log lost an earlier transition or changed its causal order: %v", messages)
	}
	if strings.Contains(jobLog.Body.String(), privateReceipt) {
		t.Fatal("job log exposed the private owner request")
	}

	var workers int
	if err := reopened.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE kind=? AND dedupe_key=?`, sqlite.ActivityActionJobKind, "activity:"+requestID).Scan(&workers); err != nil || workers != 1 {
		t.Fatalf("idempotent replay queued extra work: workers=%d err=%v", workers, err)
	}
}

func TestActivitySelectionScopeAndStaleImportEpoch(t *testing.T) {
	e := newAPIEnv(t)
	ctx := t.Context()
	item := e.addMovie(t)
	approval := insertActivityApprovalJob(t, e.db, item, "Scoped episode awaiting approval")
	held := insertActivityRunnerHold(t, e, item, "Scoped episode waiting for Runner", ports.DownloadControl{
		Version: 1, Lifecycle: "held", Cause: "capacity", RetryPolicy: "resume_same_job",
	})

	type snapshot struct {
		ID        string
		Total     int
		Retryable int
		Targets   []sqlite.ActivityIdentity
	}
	selectScope := func(scope string) snapshot {
		t.Helper()
		var selection snapshot
		e.post(t, "/api/v1/activity/selections", fmt.Sprintf(`{"view":"attention","scope":%q,"q":"scoped episode"}`, scope)).
			expect(t, http.StatusOK).into(t, &selection)
		return selection
	}
	imports := selectScope("imports")
	frozenApproval := sqlite.ActivityIdentity{ID: approval.ID, GenerationKey: approval.GenerationKey, Epoch: "0"}
	if imports.Total != 1 || imports.Retryable != 0 || !slices.Equal(imports.Targets, []sqlite.ActivityIdentity{frozenApproval}) {
		t.Fatalf("imports selection includes a transfer or counts approval as retry: %+v", imports)
	}
	all := selectScope("all")
	heldIdentity := sqlite.ActivityIdentity{ID: held.ID, GenerationKey: held.GenerationKey, Epoch: "0"}
	if all.Total != 2 || all.Retryable != 1 || len(all.Targets) != 2 ||
		!slices.Contains(all.Targets, frozenApproval) || !slices.Contains(all.Targets, heldIdentity) {
		t.Fatalf("all selection did not capture both jobs and their retry eligibility: %+v", all)
	}
	if _, err := e.db.BeginImportRetry(ctx, approval.ID, approval.GenerationKey); err != nil {
		t.Fatal(err)
	}
	var result sqlite.ActivityOperation
	e.post(t, "/api/v1/activity/actions", fmt.Sprintf(`{"requestId":"selection-stale-epoch","action":"delete","selection":{"id":%q}}`, all.ID)).
		expect(t, http.StatusAccepted).into(t, &result)
	byID := map[int64]sqlite.ActivityTarget{}
	for _, target := range result.Targets {
		byID[target.ID] = target
	}
	if result.Total != 2 || result.Blocked != 1 ||
		byID[approval.ID].ActivityIdentity != frozenApproval || byID[approval.ID].State != "blocked" ||
		byID[held.ID].ActivityIdentity != heldIdentity || byID[held.ID].State != "pending" {
		t.Fatalf("stale epoch should block only that frozen target: %+v", result)
	}
	for _, job := range []sqlite.Download{approval, held} {
		requested, err := e.db.DownloadDeletionRequested(ctx, job.ID, job.GenerationKey)
		if err != nil || requested != (job.ID == held.ID) {
			t.Fatalf("unexpected deletion authority for %d: requested=%v err=%v", job.ID, requested, err)
		}
	}
	var progress apigen.ImportActivityPage
	e.get(t, "/api/v1/activity?view=progress&scope=all&q=waiting+for+Runner").expect(t, http.StatusOK).into(t, &progress)
	if len(progress.Items) != 1 || progress.Items[0].Operation == nil || progress.Items[0].Operation.Id != result.ID ||
		progress.Items[0].Operation.State != "pending" {
		t.Fatalf("running deletion lost its Activity operation reference: %+v", progress)
	}
}

func insertActivityRunnerHold(t *testing.T, e *apiEnv, item int64, release string, control ports.DownloadControl) sqlite.Download {
	t.Helper()
	job, err := e.db.InsertDownloadRecord(t.Context(), sqlite.Download{
		MediaItemID: item, State: "downloading", Protocol: "usenet", ReleaseTitle: release,
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(control)
	if err != nil {
		t.Fatal(err)
	}
	job.RunnerControl = string(raw)
	if err := e.db.UpdateDownloadControl(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	return job
}

func TestActivityRunnerHoldsExposePrerequisitesAndSafeActions(t *testing.T) {
	e := newAPIEnv(t)
	item := e.addMovie(t)
	cases := []struct {
		cause, policy, reason, prerequisite string
		version                             int
		pause, retry                        bool
	}{
		{"capacity", "resume_same_job", "could not allocate space", "Free space", 1, false, true},
		{"allocation", "scheduled_retry", "could not allocate space", "Free space", 1, false, true},
		{"quota", "resume_same_job", "storage quota", "Increase or free the quota", 1, false, true},
		{"mount_unavailable", "reobserve_mount", "storage is unavailable", "Reconnect its original drive", 1, false, true},
		{"storage_unavailable", "review", "storage is unavailable", "Reconnect its original drive", 1, false, false},
		{"password_required", "review", "needs a password", "Enter the password", 1, false, false},
		{"missing_tool", "review", "missing a required processing tool", "Install the required tool", 1, false, false},
		{"unknown_fixture", "review", "waiting for an operator action", "resolve its recorded hold", 1, false, false},
		{"quota", "resume_same_job", "Paused in Runner", "Resume the job in Runner", 1, true, false},
		{"capacity", "resume_same_job", "unsupported Runner control version", "Update Curator", 2, false, false},
	}
	for i, tc := range cases {
		title := fmt.Sprintf("typed-hold-%02d", i)
		t.Run(title, func(t *testing.T) {
			job := insertActivityRunnerHold(t, e, item, title, ports.DownloadControl{
				Version: tc.version, Lifecycle: "held", Cause: tc.cause, RetryPolicy: tc.policy, ManualPause: tc.pause,
				Message: "backend diagnostic that must not replace the reason",
			})
			var page apigen.ImportActivityPage
			e.get(t, "/api/v1/activity?view=attention&q="+title).expect(t, http.StatusOK).into(t, &page)
			if len(page.Items) != 1 {
				t.Fatalf("Runner hold missing from attention: %+v", page)
			}
			got := page.Items[0]
			if got.Identity == nil || got.Identity.Id != job.ID || got.Identity.GenerationKey != job.GenerationKey ||
				got.Reason == nil || !strings.Contains(*got.Reason, tc.reason) ||
				got.Prerequisite == nil || !strings.Contains(*got.Prerequisite, tc.prerequisite) ||
				got.Actions == nil {
				t.Fatalf("hold projection does not identify or explain the job: %+v", got)
			}
			var visible []string
			for _, action := range *got.Actions {
				if action.Enabled {
					visible = append(visible, action.Kind)
				}
			}
			want := []string{"delete"}
			if tc.retry {
				want = append(want, "retry")
			}
			if !slices.Equal(visible, want) {
				t.Fatalf("eligible actions = %v, want %v", visible, want)
			}
		})
	}
}

func TestActivityRejectsMalformedMissingAndEmptyActionTargets(t *testing.T) {
	e := newAPIEnv(t)
	job := insertActivityApprovalJob(t, e.db, e.addMovie(t), "Still needs approval")
	for _, body := range []string{
		`{"view":"progress","scope":"all"}`,
		`{"view":"attention","scope":"unknown"}`,
		`{"view":"attention","scope":"all","q":"` + strings.Repeat("x", 8192) + `"}`,
	} {
		e.post(t, "/api/v1/activity/selections", body).expect(t, http.StatusBadRequest)
	}
	identity := fmt.Sprintf(`{"id":%d,"generationKey":%q,"epoch":"0"}`, job.ID, job.GenerationKey)
	invalid := map[string]string{
		"malformed JSON":        `{"requestId":`,
		"both target sources":   `{"requestId":"ambiguous-targets","action":"delete","targets":[` + identity + `],"selection":{"id":"other"}}`,
		"duplicate job target":  `{"requestId":"duplicate-targets","action":"delete","targets":[` + identity + `,` + identity + `]}`,
		"archive only identity": `{"requestId":"history-identity","action":"delete","targets":[{"id":-1,"generationKey":"old","epoch":"0"}]}`,
		"missing epoch":         `{"requestId":"no-epoch-action","action":"delete","targets":[{"id":1,"generationKey":"some"}]}`,
	}
	for name, body := range invalid {
		t.Run(name, func(t *testing.T) {
			e.post(t, "/api/v1/activity/actions", body).expect(t, http.StatusBadRequest)
		})
	}
	var selection struct{ ID string }
	e.post(t, "/api/v1/activity/selections", `{"view":"attention","scope":"all","q":"Still needs approval"}`).
		expect(t, http.StatusOK).into(t, &selection)
	e.post(t, "/api/v1/activity/actions", fmt.Sprintf(`{"requestId":"selection-now-empty","action":"delete","selection":{"id":%q,"excludeIds":[%d]}}`, selection.ID, job.ID)).
		expect(t, http.StatusBadRequest)
	e.get(t, "/api/v1/activity/operations/selection-now-empty").expect(t, http.StatusNotFound)
	e.get(t, "/api/v1/activity/jobs/900001/log").expect(t, http.StatusNotFound)
	e.get(t, "/api/v1/activity/jobs/-900001/log").expect(t, http.StatusNotFound)
	requested, err := e.db.DownloadDeletionRequested(t.Context(), job.ID, job.GenerationKey)
	if err != nil || requested {
		t.Fatalf("invalid commands changed the approval job: requested=%v err=%v", requested, err)
	}
}

func TestActivityBulkRetryDoesNotApproveOrChooseFiles(t *testing.T) {
	e := newAPIEnv(t)
	ctx := t.Context()
	item := e.addMovie(t)
	approval := insertActivityApprovalJob(t, e.db, item, "Requires an approval decision")
	manual, err := e.db.InsertDownloadRecord(ctx, sqlite.Download{
		MediaItemID: item, State: "failed", Protocol: "manual", ReleaseTitle: "Requires a file decision", ImportPath: "/completed/file-choice",
	})
	if err != nil {
		t.Fatal(err)
	}
	publication, err := e.db.InsertPublication(ctx, sqlite.PublicationWrite{
		DownloadID: manual.ID, Kind: "completed", Generation: "file-choice", Path: manual.ImportPath, Cohort: "live",
	})
	if err != nil {
		t.Fatal(err)
	}
	file := sqlite.PublicationFile{
		PublicationID: publication, Path: "episode.mkv", Bytes: 10, Outcome: "unresolved",
		Reason: "not selected for import; place or discard it in Resolve",
	}
	if err := e.db.SeedPublicationFiles(ctx, publication, []sqlite.PublicationFile{file}); err != nil {
		t.Fatal(err)
	}
	if err := e.db.SetPublicationFileOutcome(ctx, file); err != nil {
		t.Fatal(err)
	}

	var selected apigen.ImportActivityPage
	e.get(t, "/api/v1/activity?view=attention&q=Requires+a+file").expect(t, http.StatusOK).into(t, &selected)
	if len(selected.Items) != 1 || selected.Items[0].Actions == nil {
		t.Fatalf("file decision disappeared from attention: %+v", selected)
	}
	var actions []string
	for _, action := range *selected.Items[0].Actions {
		if action.Enabled {
			actions = append(actions, action.Kind)
		}
	}
	if !slices.Equal(actions, []string{"delete", "choose_files"}) {
		t.Fatalf("file decision exposed an unexpected action: %v", actions)
	}
	targets := []sqlite.ActivityIdentity{
		{ID: approval.ID, GenerationKey: approval.GenerationKey, Epoch: "0"},
		{ID: manual.ID, GenerationKey: manual.GenerationKey, Epoch: "0"},
	}
	raw, err := json.Marshal(map[string]any{"requestId": "retry-with-user-decisions", "action": "retry", "targets": targets})
	if err != nil {
		t.Fatal(err)
	}
	e.post(t, "/api/v1/activity/actions", string(raw)).expect(t, http.StatusAccepted)
	work, err := e.db.ClaimJob(ctx, "api-activity-fixture", []string{sqlite.ActivityActionJobKind}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var run func(context.Context, domain.Job) error
	if err := acquisition.RegisterActivityActionJobs(e.acq, func(kind string, handler func(context.Context, domain.Job) error) error {
		if kind != work.Kind {
			return fmt.Errorf("registered unexpected job kind %q", kind)
		}
		run = handler
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := run(ctx, work); err != nil {
		t.Fatal(err)
	}
	var result sqlite.ActivityOperation
	e.get(t, "/api/v1/activity/operations/retry-with-user-decisions").expect(t, http.StatusOK).into(t, &result)
	if result.State != "completed" || result.Blocked != 2 || result.Succeeded != 0 || len(result.Targets) != 2 {
		t.Fatalf("bulk Retry bypassed an operator decision: %+v", result)
	}
	pending, err := e.db.GetDownload(ctx, approval.ID)
	if err != nil || pending.State != "awaiting_import" {
		t.Fatalf("bulk Retry approved the waiting job: state=%v err=%v", pending.State, err)
	}
	files, err := e.db.ListPublicationFiles(ctx, publication)
	if err != nil || len(files) != 1 || files[0].Outcome != "unresolved" {
		t.Fatalf("bulk Retry changed the file decision: %+v err=%v", files, err)
	}
}

func TestActivityLogBoundsRawErrorAndHistoricalIdentity(t *testing.T) {
	e := newAPIEnv(t)
	ctx := t.Context()
	job := insertActivityApprovalJob(t, e.db, e.addMovie(t), "Current import")
	const end = "raw error trailer"
	const recorded = "import diagnostic"
	message := recorded + strings.Repeat("x", 9000) + end
	if err := e.db.UpdateDownloadState(ctx, job.ID, "failed", 1, message); err != nil {
		t.Fatal(err)
	}
	associated, err := e.db.InsertPublication(ctx, sqlite.PublicationWrite{
		DownloadID: job.ID, Kind: "completed", Generation: "current-publication", Path: "/completed/current", Cohort: "live",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.db.SetPublicationState(ctx, associated, "import_failed", "recorded publication state"); err != nil {
		t.Fatal(err)
	}
	orphan, err := e.db.InsertPublication(ctx, sqlite.PublicationWrite{
		Kind: "completed", Generation: "historical-publication", Path: "/completed/archive/older-label", Cohort: "historical",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.db.SetPublicationState(ctx, orphan, "imported", ""); err != nil {
		t.Fatal(err)
	}
	type jobLog struct {
		Title, Summary string
		Events         []struct{ Step, Message string }
	}
	var current jobLog
	e.get(t, fmt.Sprintf("/api/v1/activity/jobs/%d/log", job.ID)).expect(t, http.StatusOK).into(t, &current)
	if current.Title != job.ReleaseTitle || strings.Contains(current.Summary, recorded) {
		t.Fatalf("raw error replaced the readable job summary: %+v", current)
	}
	var steps = map[string]string{}
	for _, event := range current.Events {
		steps[event.Step] = event.Message
	}
	bounded := steps["recorded job error"]
	if len(bounded) != 8192+len("…") || !strings.HasPrefix(bounded, recorded) || !strings.HasSuffix(bounded, "…") || strings.Contains(bounded, end) ||
		steps["publication observation"] != "import_failed: recorded publication state" {
		t.Fatalf("job log lost the bounded diagnostic or publication observation: steps=%d diagnosticBytes=%d", len(steps), len(bounded))
	}
	var historic jobLog
	e.get(t, fmt.Sprintf("/api/v1/activity/jobs/%d/log", -orphan)).expect(t, http.StatusOK).into(t, &historic)
	if historic.Title != "older-label" || !strings.Contains(historic.Summary, "transfer identity incomplete") ||
		len(historic.Events) != 2 || historic.Events[0].Step != "publication recorded" ||
		historic.Events[1].Step != "publication outcome" || historic.Events[1].Message != "imported" {
		t.Fatalf("historical log invented a transfer or lost its recorded outcome: %+v", historic)
	}
	var history apigen.ImportActivityPage
	e.get(t, "/api/v1/activity?view=history&q=older-label").expect(t, http.StatusOK).into(t, &history)
	if len(history.Items) != 1 || history.Items[0].Download.Id != -orphan || history.Items[0].Identity != nil || history.Items[0].Actions != nil {
		t.Fatalf("historical read-only publication exposed mutable job identity: %+v", history)
	}
}

func TestActivityAPIDoesNotAcknowledgeUncommittedSelectionOrDeletion(t *testing.T) {
	e := newAPIEnv(t)
	ctx := t.Context()
	job := insertActivityApprovalJob(t, e.db, e.addMovie(t), "Persistent selection")
	const selectionEndpoint = "/api/v1/activity/selections"
	const selectBody = `{"view":"attention","scope":"all","q":"Persistent selection"}`
	if _, err := e.db.W.ExecContext(ctx, `CREATE TRIGGER fail_fixture_selection BEFORE INSERT ON activity_selections
		BEGIN SELECT RAISE(ABORT,'cannot save frozen selection'); END`); err != nil {
		t.Fatal(err)
	}
	e.post(t, selectionEndpoint, selectBody).expect(t, http.StatusInternalServerError)
	if _, err := e.db.W.ExecContext(ctx, `DROP TRIGGER fail_fixture_selection`); err != nil {
		t.Fatal(err)
	}
	var selection struct{ ID string }
	e.post(t, selectionEndpoint, selectBody).expect(t, http.StatusOK).into(t, &selection)
	if _, err := e.db.W.ExecContext(ctx, `CREATE TRIGGER fail_fixture_activity_lease BEFORE INSERT ON jobs
		WHEN NEW.kind='activity.action' BEGIN SELECT RAISE(ABORT,'cannot schedule disposition'); END`); err != nil {
		t.Fatal(err)
	}
	const id = "activity-persist-failure"
	body := fmt.Sprintf(`{"requestId":%q,"action":"delete","selection":{"id":%q}}`, id, selection.ID)
	e.post(t, "/api/v1/activity/actions", body).expect(t, http.StatusConflict)
	e.get(t, "/api/v1/activity/operations/"+id).expect(t, http.StatusNotFound)
	if requested, err := e.db.DownloadDeletionRequested(ctx, job.ID, job.GenerationKey); err != nil || requested {
		t.Fatalf("failed scheduling left deletion authority: requested=%v err=%v", requested, err)
	}
	if _, err := e.db.W.ExecContext(ctx, `DROP TRIGGER fail_fixture_activity_lease`); err != nil {
		t.Fatal(err)
	}
	var operation sqlite.ActivityOperation
	e.post(t, "/api/v1/activity/actions", body).expect(t, http.StatusAccepted).into(t, &operation)
	if operation.ID != id || operation.Total != 1 || operation.State != "running" ||
		len(operation.Targets) != 1 || operation.Targets[0].ID != job.ID {
		t.Fatalf("retry after persistence failure did not submit the original frozen job: %+v", operation)
	}
}

func TestActivityAPIReportsUnavailableOrUnreadablePersistedEvidence(t *testing.T) {
	e := newAPIEnv(t)
	ctx := t.Context()
	job := insertActivityApprovalJob(t, e.db, e.addMovie(t), "Audit evidence")
	id := "activity-log-invalid-evidence"
	body := fmt.Sprintf(`{"requestId":%q,"action":"retry","targets":[{"id":%d,"generationKey":%q,"epoch":"0"}]}`, id, job.ID, job.GenerationKey)
	e.post(t, "/api/v1/activity/actions", body).expect(t, http.StatusAccepted)
	logPath := fmt.Sprintf("/api/v1/activity/jobs/%d/log", job.ID)
	e.get(t, logPath).expect(t, http.StatusOK)
	// A malformed persisted event cannot be silently omitted from a job log.
	if _, err := e.db.W.ExecContext(ctx, `INSERT INTO activity_action_events(operation_id,download_id,generation_key,at,step,message)
		VALUES(?,?,?,'invalid timestamp','invalid event','audit entry')`, id, job.ID, job.GenerationKey); err != nil {
		t.Fatal(err)
	}
	e.get(t, logPath).expect(t, http.StatusInternalServerError)
	if _, err := e.db.W.ExecContext(ctx, `DELETE FROM activity_action_events WHERE step='invalid event'`); err != nil {
		t.Fatal(err)
	}
	// The log and index depend on publication evidence even for an attempt
	// before a publication has appeared. An unavailable index is an error.
	if _, err := e.db.W.ExecContext(ctx, `ALTER TABLE publications RENAME TO unavailable_fixture_publications`); err != nil {
		t.Fatal(err)
	}
	e.get(t, logPath).expect(t, http.StatusInternalServerError)
	e.get(t, "/api/v1/activity?view=attention").expect(t, http.StatusInternalServerError)
	e.post(t, "/api/v1/activity/selections", `{"view":"attention","scope":"all"}`).expect(t, http.StatusInternalServerError)

	if _, err := e.db.W.ExecContext(ctx, `UPDATE activity_operation_targets SET payload='invalid JSON' WHERE operation_id=?`, id); err != nil {
		t.Fatal(err)
	}
	e.get(t, "/api/v1/activity/operations/"+id).expect(t, http.StatusInternalServerError)
}

func TestActivityJobLogCorrelatesItsCinemaDelivery(t *testing.T) {
	e := newAPIEnv(t)
	ctx := t.Context()
	item := e.addMovie(t)
	job := insertActivityApprovalJob(t, e.db, item, "Imported movie")
	unrelated := insertActivityApprovalJob(t, e.db, item, "Different acquisition for the same title")
	publicationID, err := e.db.InsertPublication(ctx, sqlite.PublicationWrite{
		DownloadID: job.ID, Kind: "completed", Generation: "movie-delivery", Path: "/completed/movie", Cohort: "live",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.db.SetPublicationState(ctx, publicationID, "imported", ""); err != nil {
		t.Fatal(err)
	}
	notifier, err := e.db.AddNotifier(ctx, ports.NotifierConfig{Type: "plurx", Name: "Cinema", Enabled: true, OnImport: true})
	if err != nil {
		t.Fatal(err)
	}
	const directory = "/library/movie"
	expected := []ports.ExpectedFile{{Path: filepath.Join(directory, "movie.mkv"), Bytes: 42, MtimeNs: 1234}}
	if err := e.db.EnqueueDeliveries(ctx, []sqlite.OutboxWrite{
		{NotifierID: notifier, DownloadID: job.ID, PublicationID: publicationID, MediaItemID: item,
			IdempotencyKey: "this-directory", Directory: directory, Event: "import", Payload: "{}", ExpectedFiles: expected},
		{NotifierID: notifier, DownloadID: unrelated.ID, MediaItemID: item,
			IdempotencyKey: "another-directory", Directory: "/library/other", Event: "import", Payload: "{}"},
	}); err != nil {
		t.Fatal(err)
	}
	deliveries, err := e.db.ListDeliveries(ctx, notifier, 10)
	if err != nil || len(deliveries) != 2 {
		t.Fatalf("read delivery fixture: %+v err=%v", deliveries, err)
	}
	for _, delivery := range deliveries {
		delivery.LastError = "unrelated Cinema message"
		if delivery.DownloadID == job.ID {
			delivery.LastError = "Waiting for the expected file"
			delivery.RemoteRequestID = "cinema-request"
			delivery.RemoteState = "scanning"
			delivery.WaitingOn = directory
			delivery.ItemIDs = []int64{17}
			// A pending directory may have registered an item but not yet
			// confirmed all expected files. The delivery API retains that evidence.
			if _, err := e.db.W.ExecContext(ctx, `UPDATE notifier_deliveries SET created_at=? WHERE id=?`,
				time.Now().Add(-time.Hour).UnixMilli(), delivery.ID); err != nil {
				t.Fatal(err)
			}
		}
		if err := e.db.SettleDelivery(ctx, delivery, sqlite.DeliveryTiming{}); err != nil {
			t.Fatal(err)
		}
	}
	var log struct {
		Events []struct{ Step, Message string }
	}
	e.get(t, fmt.Sprintf("/api/v1/activity/jobs/%d/log", job.ID)).expect(t, http.StatusOK).into(t, &log)
	var cinema []string
	for _, event := range log.Events {
		if event.Step == "Cinema update observation" {
			cinema = append(cinema, event.Message)
		}
	}
	if !slices.Equal(cinema, []string{"pending: Waiting for the expected file"}) {
		t.Fatalf("job log reported the wrong Cinema evidence: %v", cinema)
	}
	var public []apigen.Delivery
	e.get(t, fmt.Sprintf("/api/v1/notifiers/%d/deliveries", notifier)).expect(t, http.StatusOK).into(t, &public)
	for _, delivery := range public {
		if delivery.DownloadId == nil || *delivery.DownloadId != job.ID {
			continue
		}
		if delivery.PublicationId == nil || *delivery.PublicationId != publicationID ||
			delivery.Directory == nil || *delivery.Directory != directory ||
			delivery.ExpectedFiles == nil || len(*delivery.ExpectedFiles) != 1 || (*delivery.ExpectedFiles)[0].Path != expected[0].Path ||
			delivery.RemoteRequestId == nil || *delivery.RemoteRequestId != "cinema-request" ||
			delivery.ItemIds == nil || !slices.Equal(*delivery.ItemIds, []int64{17}) ||
			delivery.Warning == nil || !*delivery.Warning {
			t.Fatalf("Cinema delivery lost measurement or pending warning: %+v", delivery)
		}
		return
	}
	t.Fatal("the correlated Cinema delivery did not appear in delivery history")
}

func TestActivityChooseFilesValidatesAndCommitsOnlySelectedEpisode(t *testing.T) {
	e := newAPIEnv(t)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(func() { cancel(); e.acq.WaitImporters() })
	roots, err := e.db.ListRootFolders(ctx)
	if err != nil || len(roots) != 1 {
		t.Fatalf("read configured Library root: %+v err=%v", roots, err)
	}
	libraryPath := filepath.Join(e.root, "File Choice")
	item, err := e.db.CreateMediaItem(ctx, domain.MediaItem{
		Title: "File Choice", SortTitle: "file choice", Kind: domain.KindSeries,
		RootFolderID: roots[0].ID, Path: libraryPath,
		Seasons: []domain.Season{{Number: 1, Episodes: []domain.Episode{
			{SeasonNumber: 1, EpisodeNumber: 1, Title: "First"}, {SeasonNumber: 1, EpisodeNumber: 2, Title: "Second"},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	completed := t.TempDir()
	dir := filepath.Join(completed, "exact-publication")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := []ports.ManifestFile{}
	for _, name := range []string{"obfuscated.mkv", "left-out.mkv"} {
		body := []byte("media fixture " + name)
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
		files = append(files, ports.ManifestFile{Path: name, Bytes: int64(len(body)), SHA256: fmt.Sprintf("%x", sha256.Sum256(body))})
	}
	if err := e.db.SetMeta(ctx, acquisition.CompletedRootsSetting, completed); err != nil {
		t.Fatal(err)
	}
	if err := e.acq.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	digest, err := ports.ManifestDigest(files)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(ports.Manifest{Generation: "chosen-files", Digest: digest, Files: files})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ports.ManifestFileName), manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	job := insertActivityApprovalJob(t, e.db, item, "Choose files for this release")
	other := insertActivityApprovalJob(t, e.db, item, "Different release")
	pubID, err := e.db.InsertPublication(ctx, sqlite.PublicationWrite{
		DownloadID: job.ID, Kind: "completed", Generation: "chosen-files", Path: dir, Source: "file", ManifestJSON: string(manifest), Cohort: "live",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		file := sqlite.PublicationFile{PublicationID: pubID, Path: f.Path, Bytes: f.Bytes, SHA256: f.SHA256,
			Outcome: "unresolved", Reason: "not selected for import; place or discard it in Resolve"}
		if err := e.db.SeedPublicationFiles(ctx, pubID, []sqlite.PublicationFile{file}); err != nil {
			t.Fatal(err)
		}
		if err := e.db.SetPublicationFileOutcome(ctx, file); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.db.SetPublicationState(ctx, pubID, "import_failed", "unselected episode"); err != nil {
		t.Fatal(err)
	}
	e.acq.StartImporters(ctx)

	source := filepath.Join(dir, "obfuscated.mkv")
	e.post(t, "/api/v1/import/plan", fmt.Sprintf(`{"mediaItemId":%d,"removeOriginal":false,"files":[{"path":%q,"season":1,"episodes":[2]}]}`, item, source)).
		expect(t, http.StatusOK)
	const relative = "obfuscated.mkv"
	url := fmt.Sprintf("/api/v1/queue/%d/publications/%d/files/place", job.ID, pubID)
	mapped := fmt.Sprintf(`{"mediaItemId":%d,"files":[{"path":%q,"season":1,"episodes":[2]}]}`, item, relative)
	e.post(t, fmt.Sprintf("/api/v1/queue/%d/publications/%d/files/place", other.ID, pubID), mapped).expect(t, http.StatusNotFound)
	e.post(t, url, strings.Replace(mapped, `"episodes":[2]`, `"episodes":[99]`, 1)).expect(t, http.StatusBadRequest)
	e.post(t, url, `{"files":[{"path":"not-in-manifest.mkv","season":1,"episodes":[2]}]}`).expect(t, http.StatusBadRequest)
	e.post(t, url, mapped).expect(t, http.StatusNoContent)

	stored, err := e.db.GetDownload(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range stored.Handoff {
		if entry.Step == "manual_import_queued" && entry.Placement {
			target := entry.EpisodeTargets[source]
			if !slices.Equal(entry.Paths, []string{source}) || target.Season != 1 || !slices.Equal(target.Episodes, []int{2}) {
				t.Fatalf("selected file and episode changed on durable handoff: %+v", entry)
			}
			return
		}
	}
	t.Fatalf("selected publication was not admitted as a scoped placement: %+v", stored.Handoff)
}

func TestActivityAssignFolderRetriesOnlyFolderlessAttempt(t *testing.T) {
	e := newAPIEnv(t)
	ctx := t.Context()
	item := e.addMovie(t)
	original, err := e.db.GetMediaItemFull(ctx, item)
	if err != nil || original.RootFolderID == 0 || original.Path == "" {
		t.Fatalf("Library folder fixture: %+v err=%v", original, err)
	}
	itemPath := fmt.Sprintf("/api/v1/library/%d", item)
	e.patch(t, itemPath, `{"rootFolderId":0}`).expect(t, http.StatusOK)
	payload := []byte("the same download should import after folder assignment")
	source := filepath.Join(t.TempDir(), "Fight.Club.1999.1080p.WEB-DL.x264-LOCAL.mkv")
	if err := os.WriteFile(source, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	folderless := insertActivityApprovalJob(t, e.db, item, "Folderless import")
	folderless.State = "failed"
	folderless.Error = acquisition.ErrNoLibraryFolder.Error()
	folderless.ImportPath = source
	if err := e.db.UpdateDownloadHandoff(ctx, folderless); err != nil {
		t.Fatal(err)
	}
	other := insertActivityApprovalJob(t, e.db, item, "Unrelated import failure")
	if err := e.db.UpdateDownloadState(ctx, other.ID, "failed", 1, "unrelated failure"); err != nil {
		t.Fatal(err)
	}

	var page apigen.ImportActivityPage
	e.get(t, "/api/v1/activity?view=attention&q=Folderless").expect(t, http.StatusOK).into(t, &page)
	if len(page.Items) != 1 || page.Items[0].Reason == nil || !strings.Contains(*page.Items[0].Reason, "No library folder") ||
		page.Items[0].Prerequisite == nil || !strings.Contains(*page.Items[0].Prerequisite, "Assign a library folder") {
		t.Fatalf("folderless job did not surface the specific fix: %+v", page)
	}
	e.patch(t, itemPath, fmt.Sprintf(`{"rootFolderId":%d}`, original.RootFolderID)).expect(t, http.StatusOK)
	e.get(t, "/api/v1/activity?view=history&q=Folderless").expect(t, http.StatusOK).into(t, &page)
	if len(page.Items) != 1 || page.Items[0].Download.State != "imported" || page.Items[0].Download.Id != folderless.ID {
		t.Fatalf("folder assignment did not import the original attempt: %+v", page)
	}
	fresh, err := e.db.GetMediaItemFull(ctx, item)
	if err != nil || len(fresh.Files) != 1 {
		t.Fatalf("expected original payload in Library: %+v err=%v", fresh.Files, err)
	}
	if got, err := os.ReadFile(fresh.Files[0].Path); err != nil || !slices.Equal(got, payload) {
		t.Fatalf("wrong bytes installed after folder repair: %q err=%v", got, err)
	}
	unchanged, err := e.db.GetDownload(ctx, other.ID)
	if err != nil || unchanged.State != "failed" || unchanged.Error != "unrelated failure" {
		t.Fatalf("folder assignment retried an unrelated failure: %+v err=%v", unchanged, err)
	}
}

func TestActivityHistoryCanHideOldVerifiedDeletionWithoutDiscardingItsProof(t *testing.T) {
	e := newAPIEnv(t)
	ctx := t.Context()
	job := insertActivityApprovalJob(t, e.db, e.addMovie(t), "Historical deletion")
	const id = "historical-deletion-receipt"
	body := fmt.Sprintf(`{"requestId":%q,"action":"delete","targets":[{"id":%d,"generationKey":%q,"epoch":"0"}]}`, id, job.ID, job.GenerationKey)
	e.post(t, "/api/v1/activity/actions", body).expect(t, http.StatusAccepted)
	publication, err := e.db.InsertPublication(ctx, sqlite.PublicationWrite{
		DownloadID: job.ID, Kind: "salvage", Generation: "historical-cleanup", Path: "/completed/historical", Cohort: "historical",
	})
	if err != nil {
		t.Fatal(err)
	}
	file := sqlite.PublicationFile{PublicationID: publication, Path: "download.mkv", Bytes: 8, Outcome: "discarded", DiscardReason: "rejected"}
	if err := e.db.SeedPublicationFiles(ctx, publication, []sqlite.PublicationFile{file}); err != nil {
		t.Fatal(err)
	}
	if err := e.db.SetPublicationFileOutcome(ctx, file); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.ApprovePublicationCleanup(ctx, []int64{publication}); err != nil {
		t.Fatal(err)
	}
	if err := e.db.MarkPublicationFileRemoved(ctx, publication, file.Path, file.Outcome); err != nil {
		t.Fatal(err)
	}
	if err := e.db.SetPublicationState(ctx, publication, "removed", ""); err != nil {
		t.Fatal(err)
	}
	revision, err := e.db.DispositionRevision(ctx, job.ID, job.GenerationKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.db.VerifyDownloadDeletion(ctx, job.ID, job.GenerationKey, revision); err != nil {
		t.Fatal(err)
	}
	completed, err := e.db.ActivityOperation(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	completed.Targets[0].State = "succeeded"
	completed.Targets[0].Message = "Job files removed; Library files preserved."
	if err := e.db.SaveActivityTarget(ctx, &completed, 0); err != nil {
		t.Fatal(err)
	}
	const historyPath = "/api/v1/activity?view=history&q=Historical+deletion"
	checkVisible := func() {
		t.Helper()
		var page apigen.ImportActivityPage
		e.get(t, historyPath).expect(t, http.StatusOK).into(t, &page)
		if len(page.Items) != 1 || page.Items[0].Operation == nil || page.Items[0].Operation.Id != id ||
			page.Items[0].Operation.State != "succeeded" || page.Items[0].Reason == nil ||
			!strings.Contains(*page.Items[0].Reason, "Deleted") || page.Items[0].Actions == nil || len(*page.Items[0].Actions) != 0 {
			t.Fatalf("completed deletion should be a read-only history receipt: %+v", page)
		}
		pubs := page.Items[0].Download.Publications
		if pubs == nil || len(*pubs) != 1 || (*pubs)[0].CleanupApprovedAt == nil || (*pubs)[0].State != "removed" ||
			len((*pubs)[0].Files) != 1 || (*pubs)[0].Files[0].Removed == nil || !*(*pubs)[0].Files[0].Removed {
			t.Fatalf("history lost verified cleanup evidence: %+v", pubs)
		}
	}
	checkVisible()
	expired := time.Now().AddDate(0, 0, -acquisition.DefaultRetentionDays-2).UnixMilli()
	if _, err := e.db.W.ExecContext(ctx, `UPDATE downloads SET updated_at=? WHERE id=?`, expired, job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.W.ExecContext(ctx, `UPDATE publications SET updated_at=? WHERE id=?`, expired, publication); err != nil {
		t.Fatal(err)
	}
	var history apigen.ImportActivityPage
	e.get(t, historyPath).expect(t, http.StatusOK).into(t, &history)
	if history.Total != 0 || len(history.Items) != 0 {
		t.Fatalf("expired completed deletion remained in history: %+v", history)
	}
	e.get(t, "/api/v1/activity/operations/"+id).expect(t, http.StatusOK)
	if requested, verified, err := e.db.DownloadDeletionStatus(ctx, job.ID, job.GenerationKey); err != nil || !requested || !verified {
		t.Fatalf("history filtering discarded durable proof: requested=%v verified=%v err=%v", requested, verified, err)
	}
	if err := e.acq.SetRetentionDays(ctx, 0); err != nil {
		t.Fatal(err)
	}
	checkVisible()
}
