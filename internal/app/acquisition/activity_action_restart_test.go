package acquisition

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

func TestActivityDeleteRestartsAtDurableCommitBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, trigger, durableState string
		bound, verified             bool
		ownerCalls                  int
	}{
		{
			name: "initial audit write", durableState: "pending",
			trigger: `BEFORE INSERT ON activity_action_events WHEN NEW.step='running'`,
		},
		{
			name: "canonical owner request", durableState: "blocked",
			trigger: `BEFORE UPDATE OF owner_payload ON download_dispositions WHEN NEW.owner_payload!=''`,
		},
		{
			name: "canonical request on member", durableState: "blocked", bound: true,
			trigger: `BEFORE UPDATE ON activity_operation_targets WHEN NEW.state='running' AND json_extract(NEW.payload,'$.ownerPayload')!=''`,
		},
		{
			name: "durable receipt of verified removal", durableState: "pending", bound: true, ownerCalls: 1,
			trigger: `BEFORE UPDATE OF verified_at ON download_dispositions WHEN NEW.verified_at>0`,
		},
		{
			name: "final audit write after verified removal", durableState: "running", bound: true, verified: true, ownerCalls: 1,
			trigger: `BEFORE INSERT ON activity_action_events WHEN NEW.step='succeeded'`,
		},
		{
			name: "owner refusal progress", durableState: "running", bound: true, ownerCalls: 1,
			trigger: `BEFORE UPDATE OF state ON download_dispositions WHEN NEW.state='blocked'`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setupV2(t, nil)
			dl := f.grab(1, epOne)
			owner := &activityEvidenceRunner{fakeRunner: f.runner, dispositionState: "verified"}
			if tc.name == "owner refusal progress" {
				owner.dispositionState = "blocked"
			}
			f.svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return owner }
			admission := ports.RequestAdmission{
				RequestToken: dl.Transfer, JobID: dl.Handle, RunnerInstanceID: "fixture-owner",
				Generation: "fixture-owner-generation", SourceArtifactID: "fixture-payload", SourceGeneration: "fixture-payload-generation",
			}
			f.runner.admissions[dl.Transfer] = admission
			const id = "delete-restart-boundary"
			const fault = "injected activity write failure"
			identity := sqlite.ActivityIdentity{ID: dl.ID, GenerationKey: dl.GenerationKey, Epoch: "0"}
			work := claimActivityAction(t, f.db, id, "delete", identity)
			if _, err := f.db.W.ExecContext(f.ctx, `CREATE TRIGGER block_fixture_activity_write `+tc.trigger+
				` BEGIN SELECT RAISE(ABORT,'injected activity write failure'); END`); err != nil {
				t.Fatal(err)
			}

			runErr := f.svc.handleActivityAction(f.ctx, work)
			failed, err := f.db.ActivityOperation(f.ctx, id)
			if err != nil || failed.Succeeded != 0 || len(failed.Targets) != 1 ||
				failed.Targets[0].State != tc.durableState || owner.dispositions != tc.ownerCalls {
				t.Fatalf("fault escaped its durable boundary: result=%+v owner calls=%d worker=%v err=%v", failed, owner.dispositions, runErr, err)
			}
			switch tc.durableState {
			case "blocked":
				if runErr != nil || !strings.Contains(failed.Targets[0].Message, fault) {
					t.Fatalf("owner binding fault was not reported as a blocked member: result=%+v err=%v", failed, runErr)
				}
			case "pending":
				if tc.ownerCalls > 0 && !errors.Is(runErr, errActivityPending) ||
					tc.ownerCalls == 0 && (runErr == nil || !strings.Contains(runErr.Error(), fault)) {
					t.Fatalf("pending continuation lost its error: %v", runErr)
				}
			default:
				if runErr == nil || !strings.Contains(runErr.Error(), fault) {
					t.Fatalf("worker acknowledged a failed audit/progress write: %v", runErr)
				}
			}
			if requested, verified, err := f.db.DownloadDeletionStatus(f.ctx, dl.ID, dl.GenerationKey); err != nil || !requested || verified != tc.verified {
				t.Fatalf("deletion authority changed after interrupted write: requested=%v verified=%v err=%v", requested, verified, err)
			}
			readOwnerBinding := func(db *sqlite.DB) string {
				t.Helper()
				var binding string
				if err := db.R.QueryRowContext(f.ctx, `SELECT owner_payload FROM download_dispositions
					WHERE download_id=? AND generation_key=?`, dl.ID, dl.GenerationKey).Scan(&binding); err != nil {
					t.Fatal(err)
				}
				return binding
			}
			canonical := readOwnerBinding(f.db)
			if (canonical != "") != tc.bound {
				t.Fatalf("binding did not survive the expected commit: %q", canonical)
			}
			if _, err = f.db.W.ExecContext(f.ctx, `DROP TRIGGER block_fixture_activity_write`); err != nil {
				t.Fatal(err)
			}

			var filename string
			if err = f.db.R.QueryRowContext(f.ctx, `SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&filename); err != nil {
				t.Fatal(err)
			}
			reopened, err := sqlite.Open(filepath.Dir(filename))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reopened.Close() })
			fresh := New(reopened, f.svc.bus, nil, f.svc.newIndexer, f.svc.newClient)
			owner.dispositionState = "verified"
			retryID := id
			if tc.durableState == "blocked" {
				if err = reopened.CompleteJob(f.ctx, work.ID); err != nil {
					t.Fatal(err)
				}
				retryID = "operator-retries-interrupted-deletion"
				if _, err := reopened.CreateActivityOperation(f.ctx, retryID, "retry", identity, []sqlite.ActivityTarget{{ActivityIdentity: identity}}); err != nil {
					t.Fatal(err)
				}
			} else if err = reopened.RetryJob(f.ctx, work.ID, fault, time.Now().Add(-time.Second)); err != nil {
				t.Fatal(err)
			}
			replay, err := reopened.ClaimJob(f.ctx, "after-activity-restart", []string{sqlite.ActivityActionJobKind}, time.Minute)
			if err != nil || replay.DedupeKey != "activity:"+retryID {
				t.Fatalf("unexpected recovered lease: %+v err=%v", replay, err)
			}
			if err := fresh.handleActivityAction(f.ctx, replay); err != nil {
				t.Fatal(err)
			}
			completed, err := reopened.ActivityOperation(f.ctx, retryID)
			if err != nil || completed.Succeeded != 1 || completed.State != "completed" || owner.dispositions != tc.ownerCalls+1 {
				t.Fatalf("restart did not finish the exact operation: %+v owner calls=%d err=%v", completed, owner.dispositions, err)
			}
			after := readOwnerBinding(reopened)
			if after == "" || (canonical != "" && after != canonical) {
				t.Fatalf("restart lost or replaced the exact owner request: before=%q after=%q", canonical, after)
			}
			if requested, verified, err := reopened.DownloadDeletionStatus(f.ctx, dl.ID, dl.GenerationKey); err != nil || !requested || !verified {
				t.Fatalf("deleted job was not durably verified: requested=%v verified=%v err=%v", requested, verified, err)
			}
		})
	}
}

func TestActivityDeleteSafeguardsPublicationBeforeUnlink(t *testing.T) {
	for _, tc := range []struct{ fault, trigger, message string }{
		{"manifest write", `BEFORE UPDATE OF manifest_json ON publications WHEN NEW.manifest_source='file'`, "persistence unavailable"},
		{"per-file outcome write", `BEFORE UPDATE ON publication_files WHEN NEW.outcome='discarded'`, "persistence unavailable"},
		{"historical cleanup approval", `BEFORE UPDATE OF cleanup_approved_at ON publications WHEN NEW.cleanup_approved_at>0`, "persistence unavailable"},
		{"paused cleanup remains paused", `BEFORE UPDATE OF cleanup_paused_at ON publications WHEN OLD.cleanup_paused_at>0 AND NEW.cleanup_paused_at=0`, "persistence unavailable"},
		{"live manifest changed after it was recorded", "", "changed from its recorded seal"},
	} {
		t.Run(tc.fault, func(t *testing.T) {
			f := setupV2(t, nil)
			dl, err := f.db.InsertDownloadRecord(f.ctx, sqlite.Download{
				MediaItemID: f.item, Protocol: "manual", State: "failed", ReleaseTitle: "Exact published payload",
			})
			if err != nil {
				t.Fatal(err)
			}
			const name, contents = epOne + ".mkv", "original recorded payload"
			pub := f.publish("exact-published-payload", "exact-publication", map[string]string{name: contents})
			publication := sqlite.PublicationWrite{
				DownloadID: dl.ID, Kind: "completed", Generation: pub.Generation, Path: pub.Path, Cohort: "historical",
			}
			manifestPath := filepath.Join(pub.Path, ports.ManifestFileName)
			recorded, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			if tc.trigger == "" {
				publication.Source = "file"
				publication.ManifestJSON = string(recorded)
			}
			id, err := f.db.InsertPublication(f.ctx, publication)
			if err != nil {
				t.Fatal(err)
			}
			if tc.fault == "paused cleanup remains paused" {
				if err := f.db.SetPublicationCleanupPaused(f.ctx, id, true); err != nil {
					t.Fatal(err)
				}
			}
			if tc.trigger != "" {
				if _, err := f.db.W.ExecContext(f.ctx, `CREATE TRIGGER block_fixture_publication `+tc.trigger+
					` BEGIN SELECT RAISE(ABORT,'persistence unavailable'); END`); err != nil {
					t.Fatal(err)
				}
			} else {
				foreign := f.publish("different-payload", pub.Generation, map[string]string{"different.mkv": "unrelated payload"})
				changed, err := os.ReadFile(filepath.Join(foreign.Path, ports.ManifestFileName))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(manifestPath, changed, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			// An entry absent from the sealed manifest never belongs to this job.
			const extra = "independent bytes inside the publication"
			unrelated := writeFile(t, filepath.Join(pub.Path, "independent.txt"), extra)
			identity := sqlite.ActivityIdentity{ID: dl.ID, GenerationKey: dl.GenerationKey, Epoch: "0"}
			const op = "delete-before-publication-proof"
			work := claimActivityAction(t, f.db, op, "delete", identity)
			if err := f.svc.handleActivityAction(f.ctx, work); err != nil {
				t.Fatal(err)
			}
			result, err := f.db.ActivityOperation(f.ctx, op)
			if err != nil || result.Blocked != 1 || len(result.Targets) != 1 || !strings.Contains(result.Targets[0].Message, tc.message) {
				t.Fatalf("publication cleanup did not surface the failed prerequisite: %+v err=%v", result, err)
			}
			path := filepath.Join(pub.Path, name)
			if body, err := os.ReadFile(path); err != nil || string(body) != contents {
				t.Fatalf("a failed persistence boundary changed owned payload: %q err=%v", body, err)
			}
			if requested, verified, err := f.db.DownloadDeletionStatus(f.ctx, dl.ID, dl.GenerationKey); err != nil || !requested || verified {
				t.Fatalf("incomplete cleanup was verified: requested=%v verified=%v err=%v", requested, verified, err)
			}
			if tc.trigger != "" {
				if _, err := f.db.W.ExecContext(f.ctx, `DROP TRIGGER block_fixture_publication`); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(manifestPath, recorded, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := f.db.CompleteJob(f.ctx, work.ID); err != nil {
				t.Fatal(err)
			}
			const retryID = "retry-after-publication-proof"
			fresh := New(f.db, f.svc.bus, nil, f.svc.newIndexer, f.svc.newClient)
			replay := claimActivityAction(t, f.db, retryID, "retry", identity)
			if err := fresh.handleActivityAction(f.ctx, replay); err != nil {
				t.Fatal(err)
			}
			settled, err := f.db.ActivityOperation(f.ctx, retryID)
			if err != nil || settled.Succeeded != 1 {
				t.Fatalf("proven owned cleanup was not verified after restart: %+v err=%v", settled, err)
			}
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the proven manifest file was not removed: %v", err)
			}
			if body, err := os.ReadFile(unrelated); err != nil || string(body) != extra {
				t.Fatalf("deletion removed unowned bytes alongside the manifest: %q err=%v", body, err)
			}
		})
	}
}

func TestActivityDeleteRetiresCompletedPublicationAfterLibraryOffload(t *testing.T) {
	t.Run("settled publication after offload", func(t *testing.T) {
		checkActivityDeleteAfterLibraryChange(t, false, true)
	})
	t.Run("partially selected publication after offload", func(t *testing.T) {
		checkActivityDeleteAfterLibraryChange(t, true, true)
	})
	t.Run("missing Library bytes without an offload", func(t *testing.T) {
		checkActivityDeleteAfterLibraryChange(t, false, false)
	})
}

func checkActivityDeleteAfterLibraryChange(t *testing.T, partial, offloaded bool) {
	t.Helper()
	f := setupV2(t, nil)
	ctx, cancel := context.WithCancel(f.ctx)
	f.svc.rematchMu.Lock()
	f.svc.rematchCtx = ctx
	f.svc.rematchMu.Unlock()
	t.Cleanup(func() { cancel(); f.svc.importWG.Wait() })
	item, err := f.db.GetMediaItemFull(ctx, f.item)
	if err != nil {
		t.Fatal(err)
	}
	root, err := f.db.AddRootFolder(ctx, filepath.Dir(item.Path), domain.KindMixed)
	if err != nil {
		t.Fatal(err)
	}
	item.RootFolderID = root.ID
	if err := f.db.UpdateMediaItemPlacement(ctx, item); err != nil {
		t.Fatal(err)
	}
	notifier, err := f.db.AddNotifier(ctx, ports.NotifierConfig{
		Type: "plurx", Name: "Cinema", Enabled: true, OnImport: true, Settings: map[string]string{"url": "http://cinema"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetNotifierFeatures(ctx, notifier, []string{ports.FeatureOffload, ports.FeatureRemovedPaths}); err != nil {
		t.Fatal(err)
	}

	const filename, unselected = epOne + ".mkv", epTwo + ".mkv"
	media := map[string]string{filename: "imported episode"}
	if partial {
		cfg := f.cfg
		cfg.ManualApproval = true
		if err := f.db.UpdateDownloadClient(ctx, cfg); err != nil {
			t.Fatal(err)
		}
		f.cfg = cfg
		media[unselected] = "episode left out of the approval"
	}
	f.holdCleanup()
	var initial sqlite.Download
	if partial {
		initial = f.grab(0, seasonPack)
	} else {
		initial = f.grab(1, epOne)
	}
	publication := f.publish("published-episode", "publication-generation", media)
	f.complete(initial, publication)
	if partial {
		if row := f.row(initial.ID); row.State != "awaiting_import" {
			t.Fatalf("publication did not wait for its partial approval: %+v", row)
		}
		record, _ := f.publicationFiles(publication.Path)
		if err := f.svc.ResolvePublicationFile(ctx, initial.ID, record.ID, filename, "place"); err != nil {
			t.Fatal(err)
		}
	}
	record, files := f.publicationFiles(publication.Path)
	if partial && files[unselected].Outcome != "unresolved" {
		t.Fatalf("partial approval did not leave the other file unresolved: %+v", files[unselected])
	}
	libraryFile := files[filename].LibraryPath
	if record.State != "imported" || files[filename].Outcome != "placed" || !exists(libraryFile) {
		t.Fatalf("fixture did not complete publication import: publication=%+v file=%+v", record, files[filename])
	}
	if err := f.svc.PausePublicationCleanup(ctx, record.ID, true); err != nil {
		t.Fatal(err)
	}
	f.drainCleanup()
	dl := f.row(record.DownloadID)
	outbox, err := f.db.ListDeliveries(ctx, notifier, 10)
	if err != nil || len(outbox) != 1 || outbox[0].DownloadID != dl.ID || outbox[0].Event != "import" {
		t.Fatalf("fixture did not publish the original Cinema import: %+v err=%v", outbox, err)
	}
	originalDeliveries := map[string]sqlite.Delivery{"import": outbox[0]}
	unrelated := writeFile(t, filepath.Join(item.Path, "Unrelated library trailer.mkv"), "independent Library media")

	if offloaded {
		offload := offloadFixture{rematchFixture{svc: f.svc, db: f.db, show: f.item, root: root}}
		req := OffloadRequest{ItemID: item.ID, Scope: "episode", EpisodeID: item.Seasons[0].Episodes[0].ID}
		preview := offload.preview(t, req)
		if preview.Blocked != nil || len(preview.Files) != 1 || preview.Files[0].Path != libraryFile {
			t.Fatalf("Offload did not review the imported episode: %+v", preview)
		}
		offloadOp := offload.start(t, req, preview)
		if result := offload.wait(t, offloadOp.ID); result.State != "complete" {
			t.Fatalf("Library Offload did not finish: %+v", result)
		}
		offloadOutbox, err := f.db.OperationDeliveries(ctx, offloadOp.ID)
		if err != nil || len(offloadOutbox) != 1 || offloadOutbox[0].Event != "offload" {
			t.Fatalf("Offload did not record its independent Cinema update: %+v err=%v", offloadOutbox, err)
		}
		originalDeliveries["offload"] = offloadOutbox[0]
	} else if err := os.Remove(libraryFile); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(publication.Path, filename)
	libraryRows := f.count(`SELECT COUNT(*) FROM media_files WHERE path=?`, libraryFile)
	if exists(libraryFile) || !exists(source) || (offloaded && libraryRows != 0) || (!offloaded && libraryRows != 1) {
		t.Fatalf("fixture did not preserve the paused download and expected Library metadata: offloaded=%v rows=%d", offloaded, libraryRows)
	}
	owner := &activityEvidenceRunner{fakeRunner: f.runner, dispositionState: "verified"}
	f.svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return owner }
	f.runner.admissions[dl.Transfer] = ports.RequestAdmission{
		RequestToken: dl.Transfer, JobID: dl.Handle, RunnerInstanceID: "offload-test-owner",
		Generation: "offload-test-admission", Outcome: ports.OutcomeCompleted,
	}
	attempt, err := f.db.GetImportAttempt(ctx, dl.ID, dl.GenerationKey)
	if err != nil {
		t.Fatal(err)
	}
	const id = "delete-after-legitimate-offload"
	work := claimActivityAction(t, f.db, id, "delete", sqlite.ActivityIdentity{
		ID: dl.ID, GenerationKey: dl.GenerationKey, Epoch: strconv.FormatInt(attempt.Epoch, 10),
	})
	if err := f.svc.handleActivityAction(ctx, work); err != nil {
		t.Fatal(err)
	}
	result, err := f.db.ActivityOperation(ctx, id)
	if err != nil || len(result.Targets) != 1 {
		t.Fatalf("reading Activity result: %+v err=%v", result, err)
	}
	pubAfter, filesAfter := f.publicationFiles(publication.Path)
	if offloaded {
		if result.Succeeded != 1 || exists(source) || pubAfter.State != "removed" || filesAfter[filename].RemovedAt.IsZero() {
			t.Fatalf("Activity could not retire the exact download after valid Library Offload: result=%+v file=%+v", result, filesAfter[filename])
		}
		if partial {
			removed := filesAfter[unselected]
			if removed.Outcome != "discarded" || removed.RemovedAt.IsZero() || exists(filepath.Join(publication.Path, unselected)) {
				t.Fatalf("unselected file was not safely retired: %+v", removed)
			}
		}
	} else {
		if result.Blocked != 1 || !strings.Contains(result.Targets[0].Message, "missing or differs") ||
			pubAfter.State == "removed" || !filesAfter[filename].RemovedAt.IsZero() {
			t.Fatalf("Activity did not protect the only remaining copy: result=%+v file=%+v", result, filesAfter[filename])
		}
		if body, err := os.ReadFile(source); err != nil || string(body) != media[filename] {
			t.Fatalf("Activity changed the sole remaining source: %q err=%v", body, err)
		}
	}
	if requested, verified, err := f.db.DownloadDeletionStatus(ctx, dl.ID, dl.GenerationKey); err != nil || !requested || verified != offloaded {
		t.Fatalf("Activity recorded incorrect disposal evidence: requested=%v verified=%v err=%v", requested, verified, err)
	}
	if exists(libraryFile) {
		t.Fatal("Activity restored the previously removed Library file")
	}
	if body, err := os.ReadFile(unrelated); err != nil || string(body) != "independent Library media" {
		t.Fatalf("Activity changed unrelated Library bytes: %q err=%v", body, err)
	}
	outbox, err = f.db.ListDeliveries(ctx, notifier, 10)
	if err != nil || len(outbox) != len(originalDeliveries) {
		t.Fatalf("Activity replayed or dropped a Cinema notification: %+v err=%v", outbox, err)
	}
	for _, row := range outbox {
		before, found := originalDeliveries[row.Event]
		if !found || row.ID != before.ID || row.Attempt != before.Attempt || row.Payload != before.Payload {
			t.Fatalf("Activity rewrote the existing Cinema %s record: %+v", row.Event, row)
		}
		delete(originalDeliveries, row.Event)
	}
}

func TestActivityAndSweepDoNotInheritDeletionAuthorityFromAReplacementManifest(t *testing.T) {
	for _, tc := range []struct {
		name, state, reason string
		sealed              bool
	}{
		{"finalized row with recorded whole-manifest seal", "import_failed", "changed from its recorded seal", true},
		{"finalized legacy row with no recorded sealed digest", "import_failed", "no recorded digest or object identity", false},
		{"unfinished legacy row with no recorded sealed digest", "discovered", "no recorded digest or object identity", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setupV2(t, nil)
			dl, err := f.db.InsertDownloadRecord(f.ctx, sqlite.Download{
				MediaItemID: f.item, State: "failed", Protocol: "manual", ReleaseTitle: "Legacy manifest authority",
			})
			if err != nil {
				t.Fatal(err)
			}
			const name = "episode.mkv"
			const original, replacement = "original bytes", "replaced bytes"
			pub := f.publish("legacy", "legacy-generation", map[string]string{name: original})
			marker := filepath.Join(pub.Path, ports.ManifestFileName)
			record := sqlite.PublicationWrite{
				DownloadID: dl.ID, Kind: "completed", Path: pub.Path, Generation: pub.Generation, Cohort: "live",
			}
			if tc.sealed {
				data, err := os.ReadFile(marker)
				if err != nil {
					t.Fatal(err)
				}
				record.Source, record.ManifestJSON = "file", string(data)
			}
			id, err := f.db.InsertPublication(f.ctx, record)
			if err != nil {
				t.Fatal(err)
			}
			file := sqlite.PublicationFile{PublicationID: id, Path: name, Bytes: int64(len(original)), Outcome: "discarded", DiscardReason: "rejected"}
			if err := f.db.SeedPublicationFiles(f.ctx, id, []sqlite.PublicationFile{file}); err != nil {
				t.Fatal(err)
			}
			if err := f.db.SetPublicationFileOutcome(f.ctx, file); err != nil {
				t.Fatal(err)
			}
			if err := f.db.SetPublicationState(f.ctx, id, tc.state, ""); err != nil {
				t.Fatal(err)
			}

			baseline, _ := f.publicationFiles(pub.Path)
			foreign := f.publish("foreign", pub.Generation, map[string]string{name: replacement})
			source := filepath.Join(pub.Path, name)
			if err := os.Rename(filepath.Join(foreign.Path, name), source); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(filepath.Join(foreign.Path, ports.ManifestFileName), marker); err != nil {
				t.Fatal(err)
			}
			const operation = "delete-rewritten-legacy-manifest"
			work := claimActivityAction(t, f.db, operation, "delete", sqlite.ActivityIdentity{ID: dl.ID, GenerationKey: dl.GenerationKey, Epoch: "0"})
			if err := f.svc.handleActivityAction(f.ctx, work); err != nil {
				t.Fatal(err)
			}
			result, err := f.db.ActivityOperation(f.ctx, operation)
			if err != nil || result.Blocked != 1 || len(result.Targets) != 1 || !strings.Contains(result.Targets[0].Message, tc.reason) {
				t.Fatalf("the rewritten marker granted deletion authority: %+v err=%v", result, err)
			}
			assertSource := func() {
				t.Helper()
				if body, err := os.ReadFile(source); err != nil || string(body) != replacement {
					t.Fatalf("the unrelated replacement did not survive: %q err=%v", body, err)
				}
				publication, files := f.publicationFiles(pub.Path)
				if publication.State == "removed" || (publication.ManifestJSON != baseline.ManifestJSON || publication.ManifestSource != baseline.ManifestSource) ||
					files[name].SHA256 != "" || !files[name].RemovedAt.IsZero() {
					t.Fatalf("replacement identity was adopted or recorded removed: publication=%+v file=%+v", publication, files[name])
				}
			}
			assertSource()
			if tc.state == "import_failed" {
				if err := f.db.ClearPublicationCleanupError(f.ctx, id); err != nil {
					t.Fatal(err)
				}
				if err := f.svc.sweepPublications(f.ctx); err != nil {
					t.Fatal(err)
				}
				publication, _ := f.publicationFiles(pub.Path)
				if !strings.Contains(publication.Error, tc.reason) {
					t.Fatalf("the ordinary sweep did not apply the same evidence guard: %q", publication.Error)
				}
				assertSource()
			}
		})
	}
}

func TestActivityDeletionMergesPendingCinemaFilesAcrossInterruptedPlacements(t *testing.T) {
	f := setupV2(t, nil)
	notifier, err := f.db.AddNotifier(f.ctx, ports.NotifierConfig{
		Type: "plurx", Name: "Cinema", Enabled: true, OnImport: true, Settings: map[string]string{"url": "http://cinema"},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := f.cfg
	cfg.ManualApproval = true
	if err := f.db.UpdateDownloadClient(f.ctx, cfg); err != nil {
		t.Fatal(err)
	}
	f.cfg = cfg
	f.holdCleanup()
	dl := f.grab(0, seasonPack)
	const firstName, secondName = epOne + ".mkv", epTwo + ".mkv"
	const firstBytes, secondBytes = "first installed episode", "second installed episode"
	pub := f.publish("two-installations-one-delivery", "same-directory-generation", map[string]string{
		firstName: firstBytes, secondName: secondBytes,
	})
	f.complete(dl, pub)
	p, _ := f.publicationFiles(pub.Path)
	if err := f.svc.ResolvePublicationFile(f.ctx, dl.ID, p.ID, firstName, "place"); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.PausePublicationCleanup(f.ctx, p.ID, true); err != nil {
		t.Fatal(err)
	}
	f.drainCleanup()

	_, beforeFiles := f.publicationFiles(pub.Path)
	first, second := beforeFiles[firstName], beforeFiles[secondName]
	if first.Outcome != "placed" || second.Outcome != "unresolved" || !exists(first.LibraryPath) {
		t.Fatalf("fixture did not commit only the first selected file: %+v", beforeFiles)
	}
	before, err := f.db.ListDeliveries(f.ctx, notifier, 10)
	if err != nil || len(before) != 1 || before[0].Status != "pending" || len(before[0].ExpectedFiles) != 1 ||
		before[0].ExpectedFiles[0].Path != first.LibraryPath {
		t.Fatalf("first expected file was not pending at Cinema: %+v err=%v", before, err)
	}

	// The second writer published its file and committed its Library record,
	// then stopped before committing the file outcome and updated notification.
	item, err := f.db.GetMediaItemFull(f.ctx, f.item)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := f.db.BeginImportRetry(f.ctx, dl.ID, dl.GenerationKey)
	if err != nil {
		t.Fatal(err)
	}
	secondSource := filepath.Join(pub.Path, secondName)
	if err := f.db.SealImportAttempt(f.ctx, attempt.ImportAttemptKey, []sqlite.ImportAttemptFile{{Source: secondSource, Kind: "media"}}, false); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetPublicationState(f.ctx, p.ID, "importing", ""); err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateDownloadState(f.ctx, dl.ID, "importing", 1, ""); err != nil {
		t.Fatal(err)
	}
	secondTarget := filepath.Join(filepath.Dir(first.LibraryPath), "Test Show - S01E02.mkv")
	publishCtx := withAcceptedPublicationGeneration(withImportAttempt(f.ctx, attempt.ImportAttemptKey), pub.Generation)
	fileID, err := f.svc.commitPlacement(publishCtx, item, importScope{
		Dest: item.Path, DownloadID: dl.ID, SourceGeneration: dl.GenerationKey, Release: dl.ReleaseTitle,
	}, secondSource, secondTarget, quality.FromString("WEBDL-1080p"), []int64{item.Seasons[0].Episodes[1].ID}, false)
	if err != nil || fileID == 0 {
		t.Fatalf("failed to commit the second file before publication settlement: id=%d err=%v", fileID, err)
	}

	fresh := f.row(dl.ID)
	owner := &activityEvidenceRunner{fakeRunner: f.runner, dispositionState: "verified"}
	f.svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return owner }
	f.runner.admissions[dl.Transfer] = ports.RequestAdmission{
		RequestToken: dl.Transfer, JobID: fresh.Handle, RunnerInstanceID: "cinema-test-owner",
		Generation: "cinema-test-admission", Outcome: ports.OutcomeCompleted,
	}
	const operation = "delete-after-second-file-placement"
	work := claimActivityAction(t, f.db, operation, "delete", sqlite.ActivityIdentity{
		ID: dl.ID, GenerationKey: dl.GenerationKey, Epoch: strconv.FormatInt(attempt.Epoch, 10),
	})
	if err := f.svc.handleActivityAction(f.ctx, work); err != nil {
		t.Fatal(err)
	}
	result, err := f.db.ActivityOperation(f.ctx, operation)
	if err != nil || result.Succeeded != 1 {
		t.Fatalf("deletion did not finish after recovering both placements: %+v err=%v", result, err)
	}
	stored, afterFiles := f.publicationFiles(pub.Path)
	if stored.State != "removed" || afterFiles[firstName].Outcome != "placed" || afterFiles[secondName].Outcome != "placed" || exists(pub.Path) {
		t.Fatalf("publication lost exact placement outcomes: publication=%+v files=%+v", stored, afterFiles)
	}
	for path, want := range map[string]string{first.LibraryPath: firstBytes, secondTarget: secondBytes} {
		if body, err := os.ReadFile(path); err != nil || string(body) != want {
			t.Fatalf("Activity changed a committed Library file %s: %q err=%v", path, body, err)
		}
	}
	after, err := f.db.ListDeliveries(f.ctx, notifier, 10)
	if err != nil || len(after) != 1 || after[0].ID != before[0].ID ||
		after[0].IdempotencyKey != before[0].IdempotencyKey || after[0].Status != "pending" || after[0].Attempt <= before[0].Attempt {
		t.Fatalf("recovery failed to refresh the same Cinema sub-delivery: %+v err=%v", after, err)
	}
	got := make([]string, 0, len(after[0].ExpectedFiles))
	for _, file := range after[0].ExpectedFiles {
		got = append(got, file.Path)
	}
	want := []string{first.LibraryPath, secondTarget}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("refresh replaced the pending first file instead of merging distinct expectations: got %v want %v", got, want)
	}
}
