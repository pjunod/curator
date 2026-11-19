package acquisition

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/quality"
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
		name, action, prior   string
		legacy, missingParent bool
	}{
		{name: "retry an owned stage", action: "retry"},
		{name: "retry after the destination was moved", action: "retry", missingParent: true},
		{name: "delete an owned stage", action: "delete"},
		{name: "preserve an older stage without object identity", action: "retry", legacy: true},
		{name: "retain a prior Library backup", action: "delete", prior: "different-target"},
		{name: "restore a prior Library backup", action: "delete", prior: "missing-target"},
		{name: "resume the prior file link after interruption", action: "delete", prior: "linked-target"},
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
			if tc.prior != "" {
				writeFile(t, backup, backupContents)
				var err error
				priorDigest, _, err = fileDigest(ctx, backup)
				if err != nil {
					t.Fatal(err)
				}
			}
			switch tc.prior {
			case "missing-target", "linked-target":
				if err := os.Remove(target); err != nil {
					t.Fatal(err)
				}
			}
			if tc.prior == "linked-target" {
				if err := os.Link(backup, target); err != nil {
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
			if tc.missingParent {
				moved := filepath.Join(t.TempDir(), "former season")
				if err := os.Rename(parent, moved); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					body, err := os.ReadFile(filepath.Join(moved, filepath.Base(temporary)))
					if err != nil || string(body) != sourceContents[:12] {
						t.Errorf("moved scratch changed: %s %v", body, err)
					}
				})
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
			if tc.prior == "missing-target" || tc.prior == "linked-target" {
				assertContents(target, backupContents)
				if _, err := os.Lstat(backup); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("settled backup name remained: %v", err)
				}
			} else if !tc.missingParent {
				assertContents(target, libraryContents)
			}
			assertContents(source, sourceContents)

			if tc.legacy {
				assertContents(temporary, sourceContents[:12])
				if operation.Blocked != 1 || operation.Succeeded != 0 || len(svc.importCh) != 0 {
					t.Fatalf("file contents alone granted custody of the legacy stage: %+v", operation)
				}
				return
			}

			if tc.prior == "different-target" {
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
	if len(f.calls) > 2 {
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
			if err := fresh.handleActivityAction(f.ctx, replay); !errors.Is(err, errActivityPending) {
				t.Fatalf("owner still has work after restart: %v", err)
			}

			// A second user action has a different operation ID. The remote
			// writer must still receive the exact same disposition command.
			const secondID = "second-user-delete"
			second := claimActivityAction(t, f.db, secondID, "delete", sqlite.ActivityIdentity{ID: dl.ID, GenerationKey: dl.GenerationKey, Epoch: "0"})
			if err := fresh.handleActivityAction(f.ctx, second); err != nil {
				t.Fatal(err)
			}
			want := activityDispositionCall{jobID: dl.Handle, request: ports.RunnerDispositionRequest{
				RequestID: "curator-delete-" + dl.GenerationKey, Action: "delete", Transfer: dl.Transfer,
				RunnerInstanceID: admission.RunnerInstanceID, Generation: admission.Generation,
				SourceArtifactID: admission.SourceArtifactID, SourceGeneration: admission.SourceGeneration,
			}}
			if len(owner.calls) != 3 || owner.calls[0] != want || owner.calls[1] != want || owner.calls[2] != want {
				t.Fatalf("nonidentical or unbound owner commands: %+v", owner.calls)
			}
			stored := f.row(dl.ID)
			if stored.RunnerOutcome != tc.terminal || stored.State != tc.state {
				t.Fatalf("delete changed the Runner terminal: got %s/%s, want %s/%s", stored.RunnerOutcome, stored.State, tc.terminal, tc.state)
			}
			completed, err := f.db.ActivityOperation(f.ctx, secondID)
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

func TestActivityRetryKeepsWriterContentionPending(t *testing.T) {
	svc, db, itemID := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	source := writeFile(t, filepath.Join(t.TempDir(), "waiting.mkv"), "waiting source")
	dl, err := db.CommitManualImportAdmission(ctx, sqlite.Download{
		MediaItemID: itemID, Protocol: "manual", State: "failed", ReleaseTitle: "Busy import", ImportPath: source,
	})
	if err != nil {
		t.Fatal(err)
	}
	const operationID = "retry-with-live-writer"
	job := claimActivityAction(t, db, operationID, "retry", sqlite.ActivityIdentity{ID: dl.ID, GenerationKey: dl.GenerationKey, Epoch: "0"})
	unlock := svc.lockDownload(dl.ID)
	err = svc.handleActivityAction(ctx, job)
	unlock()
	if !errors.Is(err, errActivityPending) {
		t.Fatalf("busy writer should leave a retry scheduled: %v", err)
	}
	pending, err := db.ActivityOperation(ctx, operationID)
	if err != nil || pending.State != "running" || pending.Blocked != 0 || len(pending.Targets) != 1 || pending.Targets[0].State != "pending" {
		t.Fatalf("writer contention became an operator blocker: %+v err=%v", pending, err)
	}
	if err := db.RetryJob(ctx, job.ID, errActivityPending.Error(), time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	replay, err := db.ClaimJob(ctx, "retry-worker", nil, time.Minute)
	if err != nil || replay.ID != job.ID {
		t.Fatalf("pending operation did not remain claimable: %+v err=%v", replay, err)
	}
	svc.importCh = make(chan importJob, 1)
	if err := svc.handleActivityAction(ctx, replay); err != nil {
		t.Fatal(err)
	}
	done, err := db.ActivityOperation(ctx, operationID)
	if err != nil || done.State != "completed" || done.Succeeded != 1 {
		t.Fatalf("the pending retry did not proceed after writer exit: %+v err=%v", done, err)
	}
	select {
	case queued := <-svc.importCh:
		queued.executionCancel()
	default:
		t.Fatal("completed retry did not queue an importer")
	}
}

func TestActivityDeleteSeedsUnseenManifestAndReplaysMountedAbsence(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		name := "previously unseen manifest"
		if interrupted {
			name = "removed directory awaiting final ledger write"
		}
		t.Run(name, func(t *testing.T) {
			f := setupV2(t, nil)
			dl, err := f.db.InsertDownloadRecord(f.ctx, sqlite.Download{
				MediaItemID: f.item, State: "failed", Protocol: "manual", ReleaseTitle: "Unimported local publication",
			})
			if err != nil {
				t.Fatal(err)
			}
			pub := f.publish("unseen-publication", "unseen", map[string]string{epOne + ".mkv": "episode one", epTwo + ".mkv": "episode two"})
			id, err := f.db.InsertPublication(f.ctx, sqlite.PublicationWrite{
				DownloadID: dl.ID, Kind: "completed", Generation: pub.Generation, Path: pub.Path, Cohort: "live",
			})
			if err != nil {
				t.Fatal(err)
			}
			if n := f.count(`SELECT COUNT(*) FROM publication_files WHERE publication_id=?`, id); n != 0 {
				t.Fatalf("fixture seeded %d file rows", n)
			}
			outside := writeFile(t, filepath.Join(f.root, "neighbor.mkv"), "unrelated bytes")
			if interrupted {
				if _, err := f.db.W.ExecContext(f.ctx, `CREATE TRIGGER fail_final_publication_write BEFORE UPDATE OF state ON publications
					WHEN NEW.state='removed' BEGIN SELECT RAISE(ABORT,'injected final ledger failure'); END`); err != nil {
					t.Fatal(err)
				}
			}

			identity := sqlite.ActivityIdentity{ID: dl.ID, GenerationKey: dl.GenerationKey, Epoch: "0"}
			operationID := "delete-unseen-publication"
			job := claimActivityAction(t, f.db, operationID, "delete", identity)
			if err := f.svc.handleActivityAction(f.ctx, job); err != nil {
				t.Fatal(err)
			}
			if interrupted {
				state, err := f.db.ActivityOperation(f.ctx, operationID)
				if err != nil || state.Blocked != 1 || exists(pub.Path) {
					t.Fatalf("fixture missed the window after physical removal: %+v pathExists=%v err=%v", state, exists(pub.Path), err)
				}
				if requested, verified, err := f.db.DownloadDeletionStatus(f.ctx, dl.ID, dl.GenerationKey); err != nil || !requested || verified {
					t.Fatalf("failed final write was reported as verified: requested=%v verified=%v err=%v", requested, verified, err)
				}
				if _, err := f.db.W.ExecContext(f.ctx, `DROP TRIGGER fail_final_publication_write`); err != nil {
					t.Fatal(err)
				}
				if err := f.db.CompleteJob(f.ctx, job.ID); err != nil {
					t.Fatal(err)
				}
				operationID = "retry-absent-publication"
				retry := claimActivityAction(t, f.db, operationID, "retry", identity)
				if err := f.svc.handleActivityAction(f.ctx, retry); err != nil {
					t.Fatal(err)
				}
			}
			done, err := f.db.ActivityOperation(f.ctx, operationID)
			if err != nil || done.Succeeded != 1 || done.State != "completed" {
				t.Fatalf("publication cleanup never verified: %+v err=%v", done, err)
			}
			stored, files := f.publicationFiles(pub.Path)
			if stored.State != "removed" || len(files) != 2 || exists(pub.Path) {
				t.Fatalf("manifest files were not retired exactly once: publication=%+v files=%+v", stored, files)
			}
			for _, file := range files {
				if file.Outcome != "discarded" || file.RemovedAt.IsZero() {
					t.Fatalf("a manifest file has no verified disposal: %+v", file)
				}
			}
			if body, err := os.ReadFile(outside); err != nil || string(body) != "unrelated bytes" {
				t.Fatalf("deletion crossed its publication scope: %q err=%v", body, err)
			}
		})
	}
}

func TestActivityDeleteRecoversPartialPackAndCinemaOutboxTogether(t *testing.T) {
	t.Run("publication still present", func(t *testing.T) {
		checkActivityDeleteRecoversPartialPackAndCinemaOutbox(t, false)
	})
	t.Run("historical publication already absent", func(t *testing.T) {
		checkActivityDeleteRecoversPartialPackAndCinemaOutbox(t, true)
	})
}

func checkActivityDeleteRecoversPartialPackAndCinemaOutbox(t *testing.T, alreadyAbsent bool) {
	t.Helper()
	f := setupV2(t, nil)
	notifierID, err := f.db.AddNotifier(f.ctx, ports.NotifierConfig{
		Type: "plurx", Name: "Cinema", Enabled: true, OnImport: true, Settings: map[string]string{"url": "http://cinema"},
	})
	if err != nil {
		t.Fatal(err)
	}
	firstName, secondName := epOne+".mkv", epTwo+".mkv"
	pub := f.publish("partly-placed-pack", "partial-pack", map[string]string{firstName: "episode that placed", secondName: "episode still downloading"})
	dl, err := f.db.CommitManualImportAdmission(f.ctx, sqlite.Download{
		MediaItemID: f.item, Protocol: "manual", State: "importing", ReleaseTitle: "Partly imported season", ImportPath: pub.Path,
	})
	if err != nil {
		t.Fatal(err)
	}
	publication := sqlite.PublicationWrite{
		DownloadID: dl.ID, Kind: "completed", Generation: pub.Generation, Path: pub.Path, Cohort: "live",
	}
	if alreadyAbsent {
		manifest, err := os.ReadFile(filepath.Join(pub.Path, ports.ManifestFileName))
		if err != nil {
			t.Fatal(err)
		}
		publication.Cohort = "historical"
		publication.Source = "file"
		publication.ManifestJSON = string(manifest)
	}
	publicationID, err := f.db.InsertPublication(f.ctx, publication)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetPublicationState(f.ctx, publicationID, "importing", ""); err != nil {
		t.Fatal(err)
	}
	item, err := f.db.GetMediaItemFull(f.ctx, f.item)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := f.db.EnsureImportAttempt(f.ctx, dl.ID, dl.GenerationKey)
	if err != nil {
		t.Fatal(err)
	}
	first, second := filepath.Join(pub.Path, firstName), filepath.Join(pub.Path, secondName)
	if err := f.db.SealImportAttempt(f.ctx, attempt.ImportAttemptKey,
		[]sqlite.ImportAttemptFile{{Source: first, Kind: "media"}, {Source: second, Kind: "media"}}, false); err != nil {
		t.Fatal(err)
	}

	// The importer committed its first file but stopped before the publication
	// transaction recorded that outcome and its Cinema notification.
	publishCtx := withAcceptedPublicationGeneration(withImportAttempt(f.ctx, attempt.ImportAttemptKey), pub.Generation)
	destination := filepath.Join(item.Path, "Season 01", "Test Show - S01E01.mkv")
	fileID, err := f.svc.commitPlacement(publishCtx, item, importScope{
		Dest: item.Path, DownloadID: dl.ID, SourceGeneration: dl.GenerationKey, Release: dl.ReleaseTitle,
	}, first, destination, quality.FromString("WEBDL-1080p"), []int64{item.Seasons[0].Episodes[0].ID}, false)
	if err != nil || fileID == 0 {
		t.Fatalf("stage committed episode: id=%d err=%v", fileID, err)
	}
	if alreadyAbsent {
		if err := os.RemoveAll(pub.Path); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.count(`SELECT COUNT(*) FROM notifier_deliveries WHERE download_id=?`, dl.ID); n != 0 {
		t.Fatalf("before aggregate recovery, got %d Cinema notifications", n)
	}
	if _, err := f.db.W.ExecContext(f.ctx, `CREATE TRIGGER fail_disposition_outbox BEFORE INSERT ON notifier_deliveries
		WHEN NEW.event='import' BEGIN SELECT RAISE(ABORT,'injected Cinema outbox failure'); END`); err != nil {
		t.Fatal(err)
	}
	identity := sqlite.ActivityIdentity{ID: dl.ID, GenerationKey: dl.GenerationKey, Epoch: strconv.FormatInt(attempt.Epoch, 10)}
	const operationID = "delete-committed-partial-pack"
	job := claimActivityAction(t, f.db, operationID, "delete", identity)
	if err := f.svc.handleActivityAction(f.ctx, job); err != nil {
		t.Fatal(err)
	}
	blocked, err := f.db.ActivityOperation(f.ctx, operationID)
	if err != nil || blocked.Blocked != 1 {
		t.Fatalf("fixture failed to interrupt aggregate recovery: %+v err=%v", blocked, err)
	}
	_, before := f.publicationFiles(pub.Path)
	if before[firstName].Outcome == "placed" || before[secondName].Outcome == "discarded" ||
		exists(first) == alreadyAbsent || exists(second) == alreadyAbsent {
		t.Fatalf("failed outbox write advanced file outcomes or deletion: %+v", before)
	}
	if _, err := f.db.W.ExecContext(f.ctx, `DROP TRIGGER fail_disposition_outbox`); err != nil {
		t.Fatal(err)
	}
	if err := f.db.CompleteJob(f.ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	const retryID = "retry-partial-pack-deletion"
	retry := claimActivityAction(t, f.db, retryID, "retry", identity)
	if err := f.svc.handleActivityAction(f.ctx, retry); err != nil {
		t.Fatal(err)
	}
	done, err := f.db.ActivityOperation(f.ctx, retryID)
	if err != nil || done.State != "completed" || done.Succeeded != 1 {
		t.Fatalf("partial pack cleanup did not finish: %+v err=%v", done, err)
	}

	stored, files := f.publicationFiles(pub.Path)
	if stored.State != "removed" || files[firstName].Outcome != "placed" || files[firstName].LibraryPath != destination ||
		files[secondName].Outcome != "discarded" || exists(pub.Path) {
		t.Fatalf("partial placement or remaining file resolution was lost: publication=%+v files=%+v", stored, files)
	}
	if alreadyAbsent && stored.CleanupApprovedAt.IsZero() {
		t.Fatal("historical generation was retired without its cleanup approval")
	}
	if got, err := os.ReadFile(destination); err != nil || string(got) != "episode that placed" {
		t.Fatalf("deletion changed the installed episode: %q err=%v", got, err)
	}
	if n := f.count(`SELECT COUNT(*) FROM media_files WHERE id=? AND path=?`, fileID, destination); n != 1 {
		t.Fatal("the original Library file identity was lost")
	}
	if n := f.count(`SELECT COUNT(*) FROM notifier_deliveries WHERE download_id=? AND notifier_id=? AND publication_id=? AND event='import'`,
		dl.ID, notifierID, publicationID); n != 1 {
		t.Fatalf("expected one correlated Cinema notification for the committed episode, got %d", n)
	}
}

func TestActivityDeleteAdoptsFirstInstallAfterInterruptedMetadata(t *testing.T) {
	testActivityDeleteAdoptsFirstInstall(t, false)
}

func TestActivityRetainedFirstInstallWaitsForDurableObservedMetadata(t *testing.T) {
	testActivityDeleteAdoptsFirstInstall(t, true)
}

func testActivityDeleteAdoptsFirstInstall(t *testing.T, retained bool) {
	t.Helper()
	svc, db, itemID := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	const installed = "the physically published episode"
	source := writeFile(t, filepath.Join(t.TempDir(), "episode.mkv"), installed)
	destination := filepath.Join(item.Path, "First install.mkv")
	dl, err := db.CommitManualImportAdmission(ctx, sqlite.Download{
		MediaItemID: itemID, Protocol: "manual", State: "importing", ReleaseTitle: "First install", ImportPath: source,
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
	if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER fail_first_install_metadata BEFORE INSERT ON media_files
		BEGIN SELECT RAISE(ABORT,'injected placement metadata fault'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.commitPlacement(withImportAttempt(ctx, attempt.ImportAttemptKey), item, importScope{
		DownloadID: dl.ID, SourceGeneration: dl.GenerationKey, Dest: item.Path, Release: dl.ReleaseTitle,
	}, source, destination, quality.FromString("WEBDL-1080p"), []int64{item.Seasons[0].Episodes[0].ID}, false); err == nil {
		t.Fatal("fixture failed to interrupt the first install after its rename")
	}
	if body, err := os.ReadFile(destination); err != nil || string(body) != installed {
		t.Fatalf("fixture never published the verified Library file: %q err=%v", body, err)
	}
	prepared, err := db.UnresolvedPlacementReferences(ctx)
	if err != nil || len(prepared) != 1 || prepared[0].State != "prepared" || prepared[0].PublishedFileGeneration == "" {
		t.Fatalf("fixture lost the pre-commit journal: %+v err=%v", prepared, err)
	}
	if _, err := db.W.ExecContext(ctx, `DROP TRIGGER fail_first_install_metadata`); err != nil {
		t.Fatal(err)
	}
	if retained {
		// An earlier owner retained the stage after its rename but before
		// recording the final target identity. Re-observation must never
		// reinterpret a failed journal write as a rollback of installed bytes.
		if _, err := db.W.ExecContext(ctx, `UPDATE import_placements SET state='retained',data=json_remove(json_set(data,'$.state','retained'),'$.published_file_generation','$.storage_identity','$.storage_root','$.storage_root_identity','$.target_parent_identity') WHERE id=?`, prepared[0].ID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER fail_observed_placement BEFORE UPDATE ON import_placements WHEN COALESCE(json_extract(NEW.data,'$.published_file_generation'),'')!='' BEGIN SELECT RAISE(ABORT,'observed journal not durable'); END`); err != nil {
			t.Fatal(err)
		}
	}
	const operationID = "delete-after-first-install"
	job := claimActivityAction(t, db, operationID, "delete", sqlite.ActivityIdentity{
		ID: dl.ID, GenerationKey: dl.GenerationKey, Epoch: strconv.FormatInt(attempt.Epoch, 10),
	})
	if retained {
		if err := svc.handleActivityAction(ctx, job); !errors.Is(err, errActivityPending) {
			t.Fatalf("failed observation did not stay pending: %v", err)
		}
		p, err := db.GetPlacement(ctx, prepared[0].ID)
		if err != nil || p.State != "retained" || p.PublishedFileGeneration != "" {
			t.Fatalf("failed observation invented rollback/commit: %+v, %v", p, err)
		}
		if body, err := os.ReadFile(destination); err != nil || string(body) != installed {
			t.Fatalf("pending metadata changed installed bytes: %q, %v", body, err)
		}
		if _, err := db.W.ExecContext(ctx, `DROP TRIGGER fail_observed_placement`); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.handleActivityAction(ctx, job); err != nil {
		t.Fatal(err)
	}
	done, err := db.ActivityOperation(ctx, operationID)
	if err != nil || done.State != "completed" || done.Succeeded != 1 {
		t.Fatalf("delete did not settle the previously installed file: %+v err=%v", done, err)
	}
	files, err := db.ListFilesForItem(ctx, itemID)
	if err != nil || len(files) != 1 || files[0].Path != destination {
		t.Fatalf("Library lost its committed metadata: %+v err=%v", files, err)
	}
	if body, err := os.ReadFile(destination); err != nil || string(body) != installed {
		t.Fatalf("deletion changed an installed Library file: %q err=%v", body, err)
	}
}

func TestActivityDeleteKeepsDigestlessReplacementDuringOrdinarySweep(t *testing.T) {
	f := setupV2(t, nil)
	dl, err := f.db.InsertDownloadRecord(f.ctx, sqlite.Download{
		MediaItemID: f.item, State: "failed", Protocol: "manual", ReleaseTitle: "Legacy publication",
	})
	if err != nil {
		t.Fatal(err)
	}
	const generation = "legacy-digestless"
	const original, replacement = "original bytes", "replaced bytes"
	dir := filepath.Join(f.root, generation)
	name := epOne + ".mkv"
	source := writeFile(t, filepath.Join(dir, name), original)
	unrelated := writeFile(t, filepath.Join(f.root, "unrelated.mkv"), replacement)
	if err := os.Rename(unrelated, source); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(ports.Manifest{
		Generation: generation, Files: []ports.ManifestFile{{Path: name, Bytes: int64(len(original))}},
	})
	if err != nil {
		t.Fatal(err)
	}
	id, err := f.db.InsertPublication(f.ctx, sqlite.PublicationWrite{
		DownloadID: dl.ID, Kind: "completed", Generation: generation, Path: dir,
		Cohort: "historical", Source: "v1_record", ManifestJSON: string(raw),
	})
	if err != nil {
		t.Fatal(err)
	}

	identity := sqlite.ActivityIdentity{ID: dl.ID, GenerationKey: dl.GenerationKey, Epoch: "0"}
	const operationID = "delete-digestless-replacement"
	job := claimActivityAction(t, f.db, operationID, "delete", identity)
	if err := f.svc.handleActivityAction(f.ctx, job); err != nil {
		t.Fatal(err)
	}
	operation, err := f.db.ActivityOperation(f.ctx, operationID)
	if err != nil || operation.Blocked != 1 || len(operation.Targets) != 1 ||
		!strings.Contains(operation.Targets[0].Message, "no recorded digest or object identity") {
		t.Fatalf("explicit Delete did not refuse digestless ownership: %+v err=%v", operation, err)
	}
	if got, err := os.ReadFile(source); err != nil || string(got) != replacement {
		t.Fatalf("explicit Delete changed the unrelated file: %q err=%v", got, err)
	}
	if err := f.db.CompleteJob(f.ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.db.ClearPublicationCleanupError(f.ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.sweepPublications(f.ctx); err != nil {
		t.Fatal(err)
	}

	stored, files := f.publicationFiles(dir)
	if stored.State == "removed" || stored.CleanupApprovedAt.IsZero() ||
		!strings.Contains(stored.Error, "no recorded digest or object identity") ||
		files[name].Outcome != "discarded" || !files[name].RemovedAt.IsZero() {
		t.Fatalf("ordinary sweep did not recheck and keep the digestless file: publication=%+v files=%+v", stored, files)
	}
	if got, err := os.ReadFile(source); err != nil || string(got) != replacement {
		t.Fatalf("ordinary sweep changed the unrelated file: %q err=%v", got, err)
	}
}

func TestMissingLibraryDestinationDistinguishesSourceFromTarget(t *testing.T) {
	item := domain.MediaItem{Path: "/library/Show"}
	for _, tc := range []struct {
		path    string
		missing bool
	}{{item.Path, true}, {"/library/Show/Season 05", true}, {"/completed/Show.mkv", false}, {"/library/Showcase", false}} {
		e := &os.PathError{Op: "lstat", Path: tc.path, Err: os.ErrNotExist}
		if got := missingLibraryDestination(e, item, 0); got != tc.missing {
			t.Errorf("path %s missing destination=%v", tc.path, got)
		}
	}
}
