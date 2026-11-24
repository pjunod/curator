package acquisition

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

type activityEvidenceClient struct {
	*fakeClient
	statusErr error
}

func (c *activityEvidenceClient) Statuses(ctx context.Context) ([]ports.DownloadStatus, error) {
	if c.statusErr != nil {
		return nil, c.statusErr
	}
	return c.fakeClient.Statuses(ctx)
}

func TestActivityExternalDeleteRequiresOwnerAndMountedFileAbsence(t *testing.T) {
	for _, tc := range []struct {
		name, observation, want string
	}{
		{"exact transfer under a different handle", "same-token", "delete this exact job"},
		{"same handle with a conflicting transfer", "same-handle", "delete this exact job"},
		{"owner temporarily unreachable", "owner-unavailable", "503"},
		{"legacy transfer identity missing", "no-transfer", "no exact client transfer identity"},
		{"mapped payload remains", "present", "remove this job's files"},
		{"payload parent is missing", "parent-absent", "remove this job's files"},
		{"no mapped payload path", "unmapped", "configure a verified completed-folder"},
		{"completed mount unavailable", "missing-storage", "configure a verified completed-folder"},
		{"owner and files are absent", "verified", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setupV2(t, nil)
			client := &activityEvidenceClient{fakeClient: &fakeClient{}}
			f.svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return client }
			const transfer, handle = "exact-external-transfer", "external-handle"
			path := filepath.Join(f.root, "job-payload.mkv")
			download := sqlite.Download{
				MediaItemID: f.item, Protocol: "torrent", ClientID: f.cfg.ID, State: "imported",
				ReleaseTitle: "External transfer", Transfer: transfer, Handle: handle, SavePath: path,
			}
			switch tc.observation {
			case "same-token":
				client.statuses = []ports.DownloadStatus{{Transfer: transfer, Handle: "different-handle"}}
			case "same-handle":
				client.statuses = []ports.DownloadStatus{{Transfer: "different-transfer", Handle: handle}}
			case "owner-unavailable":
				client.statusErr = manualStatusError(503)
			case "no-transfer":
				download.Transfer = ""
			case "present":
				writeFile(t, path, "unrelated bytes left at the recorded name")
			case "parent-absent":
				download.SavePath = filepath.Join(f.root, "missing-parent", "job-payload.mkv")
			case "unmapped":
				download.SavePath = ""
			case "missing-storage":
				if err := os.Rename(f.root, f.root+"-original"); err != nil {
					t.Fatal(err)
				}
			case "verified":
				client.statuses = []ports.DownloadStatus{{Transfer: "unrelated-transfer", Handle: "unrelated-handle"}}
			}
			job, err := f.db.InsertDownloadRecord(f.ctx, download)
			if err != nil {
				t.Fatal(err)
			}
			job.SavePath = download.SavePath
			if err := f.db.UpdateDownloadHandoff(f.ctx, job); err != nil {
				t.Fatal(err)
			}
			const opID = "delete-external-evidence"
			work := claimActivityAction(t, f.db, opID, "delete", sqlite.ActivityIdentity{
				ID: job.ID, GenerationKey: job.GenerationKey, Epoch: "0",
			})
			runErr := f.svc.handleActivityAction(f.ctx, work)
			if (tc.observation == "owner-unavailable") != errors.Is(runErr, errActivityPending) ||
				(runErr != nil && !errors.Is(runErr, errActivityPending)) {
				t.Fatalf("worker result does not match retry evidence: %v", runErr)
			}
			op, err := f.db.ActivityOperation(f.ctx, opID)
			if err != nil || len(op.Targets) != 1 {
				t.Fatalf("operation=%+v err=%v", op, err)
			}
			expected := "blocked"
			if tc.observation == "owner-unavailable" {
				expected = "pending"
			} else if tc.want == "" {
				expected = "succeeded"
			}
			if got := op.Targets[0]; got.State != expected || !strings.Contains(got.Message, tc.want) {
				t.Fatalf("target=%+v, want state %s and message %q", got, expected, tc.want)
			}
			requested, verified, err := f.db.DownloadDeletionStatus(f.ctx, job.ID, job.GenerationKey)
			if err != nil || !requested || verified != (expected == "succeeded") {
				t.Fatalf("owner absence not reflected in durable receipt: requested=%v verified=%v err=%v", requested, verified, err)
			}
			if len(client.removed) != 0 {
				t.Fatalf("Curator asked an unverified third-party owner to delete data: %+v", client.removed)
			}
			if tc.observation == "present" {
				if got, err := os.ReadFile(path); err != nil || string(got) != "unrelated bytes left at the recorded name" {
					t.Fatalf("Curator changed unverified external bytes: %q err=%v", got, err)
				}
			}
		})
	}
}

func TestActivityParentDeletionWaitsForItsPendingReplacement(t *testing.T) {
	f := setupV2(t, nil)
	client := &activityEvidenceClient{fakeClient: &fakeClient{}, statusErr: manualStatusError(503)}
	f.svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return client }
	parent, err := f.db.InsertDownloadRecord(f.ctx, sqlite.Download{
		MediaItemID: f.item, Protocol: "manual", State: "failed", ReleaseTitle: "Original attempt",
	})
	if err != nil {
		t.Fatal(err)
	}
	child, err := f.db.InsertReplacementDownload(f.ctx, sqlite.Download{
		MediaItemID: f.item, Protocol: "torrent", State: "submitting", SubmissionPhase: "submitting",
		Transfer: "pending-child-transfer", Handle: "pending-child-handle", ClientID: f.cfg.ID,
		ReleaseTitle: "Replacement awaiting admission", ExecutionPayload: "{}",
	}, parent.ID, parent.GenerationKey)
	if err != nil {
		t.Fatal(err)
	}
	child.SavePath = filepath.Join(f.root, "pending-child.mkv")
	if err := f.db.UpdateDownloadHandoff(f.ctx, child); err != nil {
		t.Fatal(err)
	}
	identity := sqlite.ActivityIdentity{ID: parent.ID, GenerationKey: parent.GenerationKey, Epoch: "0"}
	const operationID = "delete-parent-with-live-child"
	work := claimActivityAction(t, f.db, operationID, "delete", identity)
	if err := f.svc.handleActivityAction(f.ctx, work); !errors.Is(err, errActivityPending) {
		t.Fatalf("a temporarily unreachable replacement must keep the parent pending: %v", err)
	}
	for _, job := range []sqlite.Download{parent, child} {
		requested, verified, err := f.db.DownloadDeletionStatus(f.ctx, job.ID, job.GenerationKey)
		if err != nil || !requested || verified {
			t.Fatalf("pending child incorrectly verified disposition %d: requested=%v verified=%v err=%v", job.ID, requested, verified, err)
		}
	}
	if err := f.db.RetryJob(f.ctx, work.ID, "owner unavailable", time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	client.statusErr = nil
	client.statuses = []ports.DownloadStatus{{Transfer: child.Transfer, Handle: ports.Handle(child.Handle)}}
	replay, err := f.db.ClaimJob(f.ctx, "next-activity-worker", nil, time.Minute)
	if err != nil || replay.ID != work.ID {
		t.Fatalf("worker did not reclaim the original parent operation: %+v err=%v", replay, err)
	}
	if err := f.svc.handleActivityAction(f.ctx, replay); err != nil {
		t.Fatal(err)
	}
	blocked, err := f.db.ActivityOperation(f.ctx, operationID)
	if err != nil || blocked.Blocked != 1 || blocked.Total != 1 || len(blocked.Targets) != 1 ||
		blocked.Targets[0].ID != parent.ID || !strings.Contains(blocked.Targets[0].Message, "delete this exact job") {
		t.Fatalf("child refusal should block its original parent target: %+v err=%v", blocked, err)
	}
	if err := f.db.CompleteJob(f.ctx, replay.ID); err != nil {
		t.Fatal(err)
	}

	client.statuses = nil
	const retryID = "retry-parent-child-disposition"
	retry := claimActivityAction(t, f.db, retryID, "retry", identity)
	if err := f.svc.handleActivityAction(f.ctx, retry); err != nil {
		t.Fatal(err)
	}
	result, err := f.db.ActivityOperation(f.ctx, retryID)
	if err != nil || result.Succeeded != 1 || len(result.Targets) != 1 || result.Targets[0].ID != parent.ID {
		t.Fatalf("retry did not complete the original deletion intent: %+v err=%v", result, err)
	}
	for _, job := range []sqlite.Download{parent, child} {
		requested, verified, err := f.db.DownloadDeletionStatus(f.ctx, job.ID, job.GenerationKey)
		if err != nil || !requested || !verified {
			t.Fatalf("verified child did not settle disposition %d: requested=%v verified=%v err=%v", job.ID, requested, verified, err)
		}
	}
}

type activityEvidenceRunner struct {
	*fakeRunner
	dispositionState string
	dispositionErr   error
	dispositions     int
}

func (r *activityEvidenceRunner) DisposeJob(_ context.Context, jobID string, request ports.RunnerDispositionRequest) (ports.RunnerDisposition, error) {
	r.dispositions++
	return ports.RunnerDisposition{
		RequestID: request.RequestID, JobID: jobID, Transfer: request.Transfer,
		RunnerInstanceID: request.RunnerInstanceID, Generation: request.Generation,
		State: r.dispositionState, Reason: "Runner still owns processing data",
	}, r.dispositionErr
}

func TestActivityRunnerDeleteRequiresExactAdmissionAndVerifiedArtifacts(t *testing.T) {
	for _, tc := range []struct {
		name, scenario, operationState, reason string
		ownerCalls                             int
	}{
		{"older Runner has no disposal contract", "unsupported", "blocked", "update Runner", 0},
		{"admission store is unavailable", "unreachable", "pending", "503", 0},
		{"unsettled admission has no receipt yet", "not-admitted", "pending", "settle this exact submission", 0},
		{"accepted admission receipt is missing", "lost-receipt", "blocked", "no admission receipt", 0},
		{"receipt points to a different job", "another-job", "blocked", "job identity changed", 0},
		{"owner reports conflicting disposition", "conflicting", "blocked", "409", 1},
		{"owner refused due to retained processing files", "retained", "blocked", "Runner still owns processing data", 1},
		{"owner verified but Curator publication is uncatalogued", "uncatalogued", "blocked", "no verified publication manifest", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setupV2(t, nil)
			owner := &activityEvidenceRunner{fakeRunner: f.runner, dispositionState: "verified"}
			f.svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return owner }
			download := sqlite.Download{
				MediaItemID: f.item, Protocol: "torrent", ClientID: f.cfg.ID, State: "downloading",
				Transfer: "activity-owner-evidence", Handle: "101", ReleaseTitle: "Runner transfer",
				SubmissionPhase: "submitted", DeliveryExpected: true,
			}
			admission := ports.RequestAdmission{
				RequestToken: download.Transfer, JobID: download.Handle, RunnerInstanceID: "owner-instance", Generation: "owner-generation",
			}
			switch tc.scenario {
			case "unsupported":
				f.svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return f.runner }
			case "unreachable":
				f.runner.admitErr = manualStatusError(503)
			case "not-admitted":
				download.SubmissionPhase = "uncertain"
			case "another-job":
				admission.JobID = "102"
			case "conflicting":
				owner.dispositionErr = manualStatusError(409)
			case "retained":
				owner.dispositionState = "blocked"
			}
			if tc.scenario != "not-admitted" && tc.scenario != "lost-receipt" {
				f.runner.admissions[download.Transfer] = admission
			}
			dl, err := f.db.InsertDownloadRecord(f.ctx, download)
			if err != nil {
				t.Fatal(err)
			}
			if tc.scenario == "uncatalogued" {
				path := writeFile(t, filepath.Join(f.root, "recorded-but-uncatalogued.mkv"), "published bytes")
				recorded, err := f.db.RecordRunnerTerminal(f.ctx, sqlite.RunnerTerminalWrite{
					DownloadID: dl.ID, Transfer: dl.Transfer, Outcome: ports.OutcomeCompleted,
					State: "imported", SavePath: path, ImportPath: path, Salvage: "none",
				})
				if err != nil || !recorded {
					t.Fatalf("record terminal: won=%v err=%v", recorded, err)
				}
			}

			const id = "runner-delete-evidence"
			work := claimActivityAction(t, f.db, id, "delete", sqlite.ActivityIdentity{ID: dl.ID, GenerationKey: dl.GenerationKey, Epoch: "0"})
			runErr := f.svc.handleActivityAction(f.ctx, work)
			if (tc.operationState == "pending") != errors.Is(runErr, errActivityPending) ||
				(runErr != nil && !errors.Is(runErr, errActivityPending)) {
				t.Fatalf("worker retry classification is wrong: %v", runErr)
			}
			result, err := f.db.ActivityOperation(f.ctx, id)
			if err != nil || len(result.Targets) != 1 || result.Targets[0].State != tc.operationState ||
				!strings.Contains(result.Targets[0].Message, tc.reason) || owner.dispositions != tc.ownerCalls {
				t.Fatalf("wrong owner or operation result: operation=%+v ownerCalls=%d err=%v", result, owner.dispositions, err)
			}
			requested, verified, err := f.db.DownloadDeletionStatus(f.ctx, dl.ID, dl.GenerationKey)
			if err != nil || !requested || verified {
				t.Fatalf("unproven disposition was verified: requested=%v verified=%v err=%v", requested, verified, err)
			}
			if tc.scenario == "uncatalogued" {
				row := f.row(dl.ID)
				body, err := os.ReadFile(row.ImportPath)
				if err != nil || string(body) != "published bytes" || row.RunnerOutcome != ports.OutcomeCompleted {
					t.Fatalf("manifestless deletion changed publication bytes or its terminal: bytes=%q terminal=%s err=%v", body, row.RunnerOutcome, err)
				}
			}
		})
	}
}

func TestActivityRetryRevalidatesQueuedJobAndResumesSameRunnerHandle(t *testing.T) {
	for _, tc := range []struct {
		name, change, message string
	}{
		{"no intervening change", "", ""},
		{"new import epoch while queued", "epoch", "attempt changed"},
		{"operator paused Runner while queued", "paused", "action is no longer available"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &resumableClient{}
			service, db, item := setup(t, nil, &client.fakeClient)
			service.newClient = func(ports.ClientConfig) ports.DownloadClient { return client }
			ctx := t.Context()
			job, err := db.InsertDownloadRecord(ctx, sqlite.Download{
				MediaItemID: item, ClientID: 1, Protocol: "torrent", State: "downloading",
				ReleaseTitle: "Held Runner download", Transfer: "original-request-token", Handle: "original-handle",
			})
			if err != nil {
				t.Fatal(err)
			}
			job.RunnerControl = `{"version":1,"revision":"initial","lifecycle":"held","cause":"capacity","retry_policy":"resume_same_job"}`
			if err := db.UpdateDownloadControl(ctx, job); err != nil {
				t.Fatal(err)
			}
			const id = "retry-held-job-evidence"
			work := claimActivityAction(t, db, id, "retry", sqlite.ActivityIdentity{
				ID: job.ID, GenerationKey: job.GenerationKey, Epoch: "0",
			})
			switch tc.change {
			case "epoch":
				if _, err := db.BeginImportRetry(ctx, job.ID, job.GenerationKey); err != nil {
					t.Fatal(err)
				}
			case "paused":
				job.RunnerControl = `{"version":1,"revision":"paused","lifecycle":"held","cause":"capacity","retry_policy":"resume_same_job","manual_pause":true}`
				if err := db.UpdateDownloadControl(ctx, job); err != nil {
					t.Fatal(err)
				}
			}
			beforeRetry, err := db.GetDownload(ctx, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.change == "epoch" && (beforeRetry.State != "downloaded" || beforeRetry.RunnerControl != "") {
				t.Fatalf("new import epoch retained a transfer hold: %+v", beforeRetry)
			}
			if err := service.handleActivityAction(ctx, work); err != nil {
				t.Fatal(err)
			}
			result, err := db.ActivityOperation(ctx, id)
			if err != nil || len(result.Targets) != 1 {
				t.Fatalf("read operation result: %+v err=%v", result, err)
			}
			if tc.message == "" {
				if result.Succeeded != 1 || client.resumed != ports.Handle(job.Handle) {
					t.Fatalf("Retry did not resume exactly the same Runner handle: result=%+v handle=%q", result, client.resumed)
				}
			} else if result.Blocked != 1 || !strings.Contains(result.Targets[0].Message, tc.message) || client.resumed != "" {
				t.Fatalf("Retry bypassed changed eligibility: result=%+v handle=%q", result, client.resumed)
			}
			current, err := db.GetDownload(ctx, job.ID)
			if err != nil || current.RunnerControl != beforeRetry.RunnerControl || current.Transfer != job.Transfer ||
				len(client.added) != 0 || len(client.removed) != 0 {
				t.Fatalf("Retry rewrote the hold or replaced the original Runner job: %+v err=%v", current, err)
			}
		})
	}
}

func TestActivityDeleteReleasesAbsentStageOnlyOnPinnedLibraryParent(t *testing.T) {
	for _, tc := range []struct{ scenario, refusal string }{
		{"no journal", "no recorded Library storage identity"},
		{"legacy bytes remain", "lacks a recorded object identity"},
		{"parent moved", "reconnect the original Library drive"},
		{"matching parent and absent stage", ""},
	} {
		t.Run(tc.scenario, func(t *testing.T) {
			f := setupV2(t, nil)
			original := writeFile(t, filepath.Join(t.TempDir(), "original.mkv"), "manual import original")
			parent := filepath.Join(t.TempDir(), "library-parent")
			target := writeFile(t, filepath.Join(parent, "installed.mkv"), "installed before cancellation")
			stage := filepath.Join(parent, ".monarr-stage-legacy")
			dl, err := f.db.CommitManualImportAdmission(f.ctx, sqlite.Download{
				MediaItemID: f.item, State: "importing", Protocol: "manual", ReleaseTitle: "Legacy local stage", ImportPath: original,
			})
			if err != nil {
				t.Fatal(err)
			}
			attempt, err := f.db.EnsureImportAttempt(f.ctx, dl.ID, dl.GenerationKey)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.db.RecordImportStage(f.ctx, attempt.ImportAttemptKey, original, target, stage); err != nil {
				t.Fatal(err)
			}
			if tc.scenario != "no journal" {
				storage, identity, err := placementParentIdentity(target)
				if err != nil {
					t.Fatal(err)
				}
				// The placement finished rollback before its separate stage receipt,
				// leaving only the journal as evidence of the original Library parent.
				if err := f.db.PreparePlacement(f.ctx, sqlite.Placement{
					ID: "legacy", State: "rolled_back", ItemID: f.item, DownloadID: dl.ID, ImportEpoch: &attempt.Epoch,
					SourceGeneration: dl.GenerationKey, Source: original, Temporary: stage, Target: target,
					StorageIdentity: storage, TargetParentIdentity: identity,
				}); err != nil {
					t.Fatal(err)
				}
			}
			switch tc.scenario {
			case "legacy bytes remain":
				writeFile(t, stage, "unproven bytes at legacy path")
			case "parent moved":
				if err := os.Rename(parent, parent+"-original"); err != nil {
					t.Fatal(err)
				}
				writeFile(t, target, "independent newly installed media")
			}
			const id = "delete-legacy-stage"
			work := claimActivityAction(t, f.db, id, "delete", sqlite.ActivityIdentity{
				ID: dl.ID, GenerationKey: dl.GenerationKey, Epoch: strconv.FormatInt(attempt.Epoch, 10),
			})
			if err := f.svc.handleActivityAction(f.ctx, work); err != nil {
				t.Fatal(err)
			}
			result, err := f.db.ActivityOperation(f.ctx, id)
			if err != nil || len(result.Targets) != 1 {
				t.Fatalf("operation result: %+v err=%v", result, err)
			}
			custody, err := f.db.ImportStageCustody(f.ctx, dl.ID, dl.GenerationKey)
			if err != nil {
				t.Fatal(err)
			}
			if tc.refusal == "" {
				if result.Succeeded != 1 || len(custody) != 0 {
					t.Fatalf("verified absence did not settle stage custody: result=%+v custody=%+v", result, custody)
				}
			} else if result.Blocked != 1 || !strings.Contains(result.Targets[0].Message, tc.refusal) || len(custody) != 1 {
				t.Fatalf("unproven absence or object retired its stage: result=%+v custody=%+v", result, custody)
			}
			wantTarget := "installed before cancellation"
			if tc.scenario == "parent moved" {
				wantTarget = "independent newly installed media"
			}
			if body, err := os.ReadFile(target); err != nil || string(body) != wantTarget {
				t.Fatalf("cleanup changed Library content: %q err=%v", body, err)
			}
			if body, err := os.ReadFile(original); err != nil || string(body) != "manual import original" {
				t.Fatalf("cleanup changed the manual source: %q err=%v", body, err)
			}
			if tc.scenario == "legacy bytes remain" {
				if body, err := os.ReadFile(stage); err != nil || string(body) != "unproven bytes at legacy path" {
					t.Fatalf("cleanup changed unproven stage bytes: %q err=%v", body, err)
				}
			}
		})
	}
}

func TestActivityDeleteStopsUnsubmittedWorkWithoutContactingOwner(t *testing.T) {
	client := &fakeClient{}
	s, db, item := setup(t, nil, client)
	ctx := t.Context()
	dl, err := db.InsertDownloadRecord(ctx, sqlite.Download{
		MediaItemID: item, ClientID: 1, Protocol: "torrent", State: "planned", SubmissionPhase: "pending",
		ReleaseTitle: "Unsubmitted download", Transfer: "unsubmitted-request", ExecutionPayload: "{}",
	})
	if err != nil {
		t.Fatal(err)
	}
	const id = "delete-unsubmitted-attempt"
	work := claimActivityAction(t, db, id, "delete", sqlite.ActivityIdentity{ID: dl.ID, GenerationKey: dl.GenerationKey, Epoch: "0"})
	if err := s.handleActivityAction(ctx, work); err != nil {
		t.Fatal(err)
	}
	row, err := db.GetDownload(ctx, dl.ID)
	if err != nil || row.SubmissionPhase != "rejected" || len(client.added) != 0 || client.polls != 0 || len(client.removed) != 0 {
		t.Fatalf("unsubmitted attempt reached its client: row=%+v err=%v", row, err)
	}
	result, err := db.ActivityOperation(ctx, id)
	if err != nil || len(result.Targets) != 1 || result.Targets[0].State != "succeeded" {
		t.Fatalf("unexpected pending-delete outcome: %+v err=%v", result, err)
	}
	requested, verified, err := db.DownloadDeletionStatus(ctx, row.ID, row.GenerationKey)
	if err != nil || !requested || !verified {
		t.Fatalf("cancellation of an unsubmitted attempt was not verified: requested=%v verified=%v err=%v", requested, verified, err)
	}
}

func TestActivityApprovalQueuesOneExactPublication(t *testing.T) {
	f := setupV2(t, nil)
	downloaded := f.publish("approval-publication", "approval-generation", map[string]string{epOne + ".mkv": "episode awaiting approval"})
	dl, err := f.db.InsertDownloadRecord(f.ctx, sqlite.Download{
		MediaItemID: f.item, State: "awaiting_import", ClientID: f.cfg.ID, Protocol: "torrent",
		ReleaseTitle: "Ready for approval",
	})
	if err != nil {
		t.Fatal(err)
	}
	dl.ImportPath = downloaded.Path
	if err := f.db.UpdateDownloadHandoff(f.ctx, dl); err != nil {
		t.Fatal(err)
	}
	pub, err := f.db.InsertPublication(f.ctx, sqlite.PublicationWrite{
		DownloadID: dl.ID, Generation: downloaded.Generation, Kind: "completed", Path: downloaded.Path, Cohort: "live",
	})
	if err != nil {
		t.Fatal(err)
	}
	f.svc.importCh = make(chan importJob, 1)
	const id = "approve-exact-job"
	work := claimActivityAction(t, f.db, id, "approve", sqlite.ActivityIdentity{ID: dl.ID, GenerationKey: dl.GenerationKey, Epoch: "0"})
	if err := f.svc.handleActivityAction(f.ctx, work); err != nil {
		t.Fatal(err)
	}
	result, err := f.db.ActivityOperation(f.ctx, id)
	if err != nil || result.Succeeded != 1 || result.Blocked != 0 {
		t.Fatalf("approval did not queue successfully: %+v err=%v", result, err)
	}
	select {
	case queued := <-f.svc.importCh:
		t.Cleanup(queued.executionCancel)
		if !queued.approved || queued.publicationID != pub || queued.dl.ID != dl.ID || queued.dl.GenerationKey != dl.GenerationKey ||
			queued.ticket == nil || queued.ticket.attempt.Epoch <= 0 {
			t.Fatalf("approval queued a different job or lost its explicit scope: %+v", queued)
		}
	default:
		t.Fatal("accepted approval left no import work")
	}
	fresh := f.row(dl.ID)
	actions, err := f.svc.ActivityActions(f.ctx, fresh)
	if err != nil || actions.Reason != "Import is running." || len(actions.Actions) != 1 || actions.Actions[0].Kind != "delete" {
		t.Fatalf("running import offered repeat approval or retry: %+v err=%v", actions, err)
	}
	if err := f.svc.handleActivityAction(f.ctx, work); err != nil || len(f.svc.importCh) != 0 {
		t.Fatalf("replaying successful approval queued additional import work: %v", err)
	}
}
