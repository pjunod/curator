package acquisition

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

func claimActivityAction(t *testing.T, db *sqlite.DB, requestID, kind string, target sqlite.ActivityIdentity) domain.Job {
	t.Helper()
	ctx := context.Background()
	if _, err := db.CreateActivityOperation(ctx, requestID, kind, target, []sqlite.ActivityTarget{{ActivityIdentity: target}}); err != nil {
		t.Fatal(err)
	}
	job, err := db.ClaimJob(ctx, "activity-fixture", nil, time.Minute)
	if err != nil || job.Kind != sqlite.ActivityActionJobKind || job.DedupeKey != "activity:"+requestID {
		t.Fatalf("claim activity job: %+v err=%v", job, err)
	}
	return job
}

func TestActivityActionsReconcileInterruptedScratch(t *testing.T) {
	for _, tc := range []struct {
		name, action   string
		legacy, backup bool
	}{
		{name: "retry an older stage", action: "retry", legacy: true},
		{name: "delete an owned stage", action: "delete"},
		{name: "retain a prior Library backup", action: "delete", backup: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, db, itemID := setup(t, nil, &fakeClient{})
			ctx := context.Background()
			const sourceContents = "downloaded content that was interrupted"
			const libraryContents = "installed Library media"
			const backupContents = "prior Library media"
			source := writeFile(t, filepath.Join(t.TempDir(), "original.mkv"), sourceContents)
			parent := t.TempDir()
			target := writeFile(t, filepath.Join(parent, "library.mkv"), libraryContents)
			const placementID = "activity-stage"
			temporary := writeFile(t, filepath.Join(parent, ".monarr-stage-"+placementID), sourceContents[:12])
			backup := filepath.Join(parent, ".monarr-prior-"+placementID)
			priorDigest := ""
			if tc.backup {
				writeFile(t, backup, backupContents)
				var err error
				priorDigest, _, err = fileDigest(ctx, backup)
				if err != nil {
					t.Fatal(err)
				}
			}

			dl, err := db.CommitManualImportAdmission(ctx, sqlite.Download{
				MediaItemID: itemID, Protocol: "manual", State: "failed", ReleaseTitle: "Interrupted manual import", ImportPath: source,
			})
			if err != nil {
				t.Fatal(err)
			}
			attempt, err := db.EnsureImportAttempt(ctx, dl.ID, dl.GenerationKey)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.SealImportAttempt(ctx, attempt.ImportAttemptKey, []sqlite.ImportAttemptFile{{Source: source, Kind: "media"}}, false); err != nil {
				t.Fatal(err)
			}
			info, err := os.Lstat(temporary)
			if err != nil {
				t.Fatal(err)
			}
			storage, parentID, err := placementParentIdentity(target)
			if err != nil {
				t.Fatal(err)
			}
			digest, size, sourceGeneration, err := fileDigestSnapshot(ctx, source)
			if err != nil {
				t.Fatal(err)
			}
			placement := sqlite.Placement{
				ID: placementID, DownloadID: dl.ID, ImportEpoch: &attempt.Epoch, ItemID: itemID, SourceGeneration: dl.GenerationKey,
				Source: source, SourceFileGeneration: sourceGeneration, Temporary: temporary, Target: target, Backup: backup,
				Size: size, SHA256: digest, PreviousSHA256: priorDigest, State: "prepared",
				StorageRoot: parent, StorageIdentity: storage, StorageRootIdentity: parentID, TargetParentIdentity: parentID,
			}
			if !tc.legacy {
				placement.TemporaryObjectIdentity = deliveryFileStableIdentity(info)
				placement.TemporaryFileGeneration = deliveryFileGeneration(info)
			}
			if err := db.PreparePlacement(ctx, placement); err != nil {
				t.Fatal(err)
			}
			cancelled, err := svc.CancelImportAttempt(ctx, dl.ID, &attempt.ImportAttemptKey)
			if err != nil || cancelled.Status != "cancelled" {
				t.Fatalf("settle prior cancellation: %+v err=%v", cancelled, err)
			}

			// Keep the subsequent import queued so it cannot alter the existing
			// Library file before the action's cleanup has been inspected.
			svc.importCh = make(chan importJob, 1)
			identity := sqlite.ActivityIdentity{ID: dl.ID, GenerationKey: dl.GenerationKey, Epoch: strconv.FormatInt(attempt.Epoch, 10)}
			const operationID = "scratch-disposition"
			job := claimActivityAction(t, db, operationID, tc.action, identity)
			if err := svc.handleActivityAction(ctx, job); err != nil {
				t.Fatal(err)
			}
			operation, err := db.ActivityOperation(ctx, operationID)
			if err != nil || len(operation.Targets) != 1 {
				t.Fatalf("read action result: %+v err=%v", operation, err)
			}
			assertContents := func(path, expected string) {
				t.Helper()
				body, err := os.ReadFile(path)
				if err != nil || string(body) != expected {
					t.Fatalf("%q has %q, want %q: %v", path, body, expected, err)
				}
			}
			assertContents(target, libraryContents)
			assertContents(source, sourceContents)

			if tc.backup {
				assertContents(backup, backupContents)
				assertContents(temporary, sourceContents[:12])
				if operation.Blocked != 1 || operation.Succeeded != 0 {
					t.Fatalf("deletion succeeded with unresolved prior-file recovery: %+v", operation)
				}
				if requested, verified, err := db.DownloadDeletionStatus(ctx, dl.ID, dl.GenerationKey); err != nil || !requested || verified {
					t.Fatalf("the backup lost its protection: requested=%v verified=%v err=%v", requested, verified, err)
				}
				return
			}

			if operation.Succeeded != 1 || operation.Blocked != 0 {
				t.Fatalf("owned scratch did not reconcile: %+v", operation)
			}
			if _, err := os.Lstat(temporary); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("owned scratch was not removed: %v", err)
			}
			stored, err := db.GetPlacement(ctx, placementID)
			if err != nil || stored.State != "rolled_back" {
				t.Fatalf("the placement still owns scratch: %+v err=%v", stored, err)
			}
			if tc.action == "retry" {
				select {
				case queued := <-svc.importCh:
					defer queued.executionCancel()
					if queued.ticket == nil || queued.ticket.attempt.Epoch != attempt.Epoch+1 || queued.ticket.attempt.GenerationKey != dl.GenerationKey {
						t.Fatalf("retry did not queue a new epoch of the same download: %+v", queued.ticket)
					}
				default:
					t.Fatal("a successful retry did not queue an importer")
				}
			} else if requested, verified, err := db.DownloadDeletionStatus(ctx, dl.ID, dl.GenerationKey); err != nil || !requested || !verified {
				t.Fatalf("owned deletion was not independently verified: requested=%v verified=%v err=%v", requested, verified, err)
			}
		})
	}
}

type activityDispositionCall struct {
	jobID   string
	request ports.RunnerDispositionRequest
}

type activityDispositionRunner struct {
	*fakeRunner
	calls []activityDispositionCall
}

func (f *activityDispositionRunner) DisposeJob(_ context.Context, jobID string, req ports.RunnerDispositionRequest) (ports.RunnerDisposition, error) {
	f.calls = append(f.calls, activityDispositionCall{jobID: jobID, request: req})
	state := "pending"
	if len(f.calls) > 1 {
		state = "verified"
	}
	return ports.RunnerDisposition{
		RequestID: req.RequestID, JobID: jobID, Transfer: req.Transfer, RunnerInstanceID: req.RunnerInstanceID,
		Generation: req.Generation, State: state,
	}, nil
}

func TestActivityDeleteReplaysExactRunnerIdentityAndPreservesTerminal(t *testing.T) {
	for _, tc := range []struct{ terminal, state string }{
		{terminal: ports.OutcomeFailed, state: "failed"},
		{terminal: ports.OutcomeCompleted, state: "imported"},
	} {
		t.Run(tc.terminal, func(t *testing.T) {
			f := setupV2(t, nil)
			dl := f.grab(1, epOne)
			owner := &activityDispositionRunner{fakeRunner: f.runner}
			f.svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return owner }
			recorded, err := f.db.RecordRunnerTerminal(f.ctx, sqlite.RunnerTerminalWrite{
				DownloadID: dl.ID, Transfer: dl.Transfer, Outcome: tc.terminal, State: tc.state, Salvage: "none",
			})
			if err != nil || !recorded {
				t.Fatalf("preexisting terminal: recorded=%v err=%v", recorded, err)
			}
			// Some Runner receipts report the job as deleted after artifact
			// removal. That disposition must leave this observed terminal intact.
			admission := ports.RequestAdmission{
				RequestToken: dl.Transfer, JobID: dl.Handle, RunnerInstanceID: "runner-instance", Generation: "runner-generation",
				SourceArtifactID: "source-artifact", SourceGeneration: "source-generation", Outcome: "deleted",
			}
			f.runner.admissions[dl.Transfer] = admission
			const operationID = "delete-runner-with-terminal"
			job := claimActivityAction(t, f.db, operationID, "delete", sqlite.ActivityIdentity{ID: dl.ID, GenerationKey: dl.GenerationKey, Epoch: "0"})
			if err := f.svc.handleActivityAction(f.ctx, job); !errors.Is(err, errActivityPending) {
				t.Fatalf("owner has yet to verify removal: %v", err)
			}
			pending, err := f.db.ActivityOperation(f.ctx, operationID)
			if err != nil || len(pending.Targets) != 1 || pending.Targets[0].OwnerPayload == "" || pending.State != "running" {
				t.Fatalf("owner request was not saved for replay: %+v err=%v", pending, err)
			}
			if _, verified, err := f.db.DownloadDeletionStatus(f.ctx, dl.ID, dl.GenerationKey); err != nil || verified {
				t.Fatalf("pending owner was recorded as verified: verified=%v err=%v", verified, err)
			}
			if err := f.db.RetryJob(f.ctx, job.ID, errActivityPending.Error(), time.Now().Add(-time.Second)); err != nil {
				t.Fatal(err)
			}
			fresh := New(f.db, f.svc.bus, nil, f.svc.newIndexer, f.svc.newClient)
			replay, err := f.db.ClaimJob(f.ctx, "restarted-worker", nil, time.Minute)
			if err != nil || replay.ID != job.ID || replay.LeaseOwner != "restarted-worker" {
				t.Fatalf("operation was lost across the worker restart: %+v err=%v", replay, err)
			}
			if err := fresh.handleActivityAction(f.ctx, replay); err != nil {
				t.Fatal(err)
			}
			want := activityDispositionCall{jobID: dl.Handle, request: ports.RunnerDispositionRequest{
				RequestID: operationID + ":" + strconv.FormatInt(dl.ID, 10), Action: "delete", Transfer: dl.Transfer,
				RunnerInstanceID: admission.RunnerInstanceID, Generation: admission.Generation,
				SourceArtifactID: admission.SourceArtifactID, SourceGeneration: admission.SourceGeneration,
			}}
			if len(owner.calls) != 2 || owner.calls[0] != want || owner.calls[1] != want {
				t.Fatalf("nonidentical or unbound owner commands: %+v", owner.calls)
			}
			stored := f.row(dl.ID)
			if stored.RunnerOutcome != tc.terminal || stored.State != tc.state {
				t.Fatalf("delete changed the Runner terminal: got %s/%s, want %s/%s", stored.RunnerOutcome, stored.State, tc.terminal, tc.state)
			}
			completed, err := f.db.ActivityOperation(f.ctx, operationID)
			if err != nil || completed.State != "completed" || completed.Succeeded != 1 {
				t.Fatalf("verified owner did not complete deletion: %+v err=%v", completed, err)
			}
		})
	}
}

func TestActivityLatePublicationRequeuesAsWorkerCompletes(t *testing.T) {
	svc, db, itemID := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	dl, err := db.InsertDownloadRecord(ctx, sqlite.Download{
		MediaItemID: itemID, Protocol: "manual", State: "imported", ReleaseTitle: "Already-imported download",
	})
	if err != nil {
		t.Fatal(err)
	}
	const operationID = "late-publication-replay"
	active := claimActivityAction(t, db, operationID, "delete", sqlite.ActivityIdentity{ID: dl.ID, GenerationKey: dl.GenerationKey, Epoch: "0"})
	if err := svc.handleActivityAction(ctx, active); err != nil {
		t.Fatal(err)
	}
	beforeArrival, err := db.ActivityOperation(ctx, operationID)
	if err != nil || beforeArrival.Succeeded != 1 || beforeArrival.State != "completed" {
		t.Fatalf("worker had not made its final save: %+v err=%v", beforeArrival, err)
	}

	// The worker finished its final save, but the runtime still owns its
	// lease. The late publication cannot enqueue another active dedupe key.
	if _, err := db.InsertPublication(ctx, sqlite.PublicationWrite{
		DownloadID: dl.ID, Generation: "late-files", Kind: "salvage",
		Path: filepath.Join(t.TempDir(), "late"), Source: "none", Cohort: "live",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimJob(ctx, "other-worker", nil, time.Minute); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("a second worker claimed the pending operation while the previous lease remained: %v", err)
	}
	if err := db.CompleteJob(ctx, active.ID); err != nil {
		t.Fatal(err)
	}

	replay, err := db.ClaimJob(ctx, "other-worker", nil, time.Minute)
	if err != nil || replay.ID == active.ID || replay.DedupeKey != active.DedupeKey || replay.Payload != active.Payload {
		t.Fatalf("completion lost the pending replay: previous=%+v replay=%+v err=%v", active, replay, err)
	}
	afterArrival, err := db.ActivityOperation(ctx, operationID)
	if err != nil || len(afterArrival.Targets) != 1 || afterArrival.Targets[0].State != "pending" || afterArrival.State != "running" {
		t.Fatalf("late publication did not reopen the pending target: %+v err=%v", afterArrival, err)
	}
}
