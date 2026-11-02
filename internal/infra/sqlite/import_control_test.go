package sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/pjunod/monarr/internal/domain"
)

func importControlFixture(t *testing.T) (*DB, Download, ImportAttempt) {
	t.Helper()
	db := openTestDB(t)
	ctx := context.Background()
	item, e := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Cancellation", Path: "/library"})
	if e != nil {
		t.Fatal(e)
	}
	id, e := db.InsertDownload(ctx, Download{MediaItemID: item, State: "downloaded", Protocol: "manual", ReleaseTitle: "payload", ImportPath: "/source"})
	if e != nil {
		t.Fatal(e)
	}
	dl, e := db.GetDownload(ctx, id)
	if e != nil {
		t.Fatal(e)
	}
	a, e := db.EnsureImportAttempt(ctx, id, dl.GenerationKey)
	if e != nil {
		t.Fatal(e)
	}
	return db, dl, a
}
func controlPlacement(dl Download, a ImportAttempt, id, source, target string) Placement {
	epoch := a.Epoch
	return Placement{ImportEpoch: &epoch, DownloadID: dl.ID, SourceGeneration: dl.GenerationKey, SourceTransfer: dl.Transfer, ID: id, Source: source, Target: target, Temporary: target + ".stage", SHA256: "digest", Size: 42, ItemID: dl.MediaItemID, State: "prepared", TemporaryObjectIdentity: "opened-object", PublishedFileGeneration: "physical-generation"}
}
func mustSeal(t *testing.T, db *DB, a ImportAttempt, sources ...string) {
	t.Helper()
	var files []ImportAttemptFile
	for _, source := range sources {
		files = append(files, ImportAttemptFile{Source: source, Kind: "media"})
	}
	if e := db.SealImportAttempt(context.Background(), a.ImportAttemptKey, files, false); e != nil {
		t.Fatal(e)
	}
}

func TestCancellationWinsBeforePublicationAndEpochFencesOldWorker(t *testing.T) {
	db, dl, a := importControlFixture(t)
	ctx := context.Background()
	mustSeal(t, db, a, "/source/video")
	p := controlPlacement(dl, a, "old", "/source/video", "/library/video")
	if e := db.PreparePlacement(ctx, p); e != nil {
		t.Fatal(e)
	}
	if _, e := db.RequestImportCancellation(ctx, a.ImportAttemptKey); e != nil {
		t.Fatal(e)
	}
	if e := db.AuthorizeImportPlacement(ctx, p); !errors.Is(e, ErrImportCancelled) {
		t.Fatalf("publication after intent: %v", e)
	}
	final, e := db.FinalizeImportCancellation(ctx, a.ImportAttemptKey)
	if e != nil || final.Outcome != "cancelled" {
		t.Fatalf("%+v %v", final, e)
	}
	// Ordinary updates cannot erase durable intent, and pruning cannot discard it.
	dl.State = "failed"
	dl.Handoff = nil
	if e = db.UpdateDownloadHandoff(ctx, dl); e != nil {
		t.Fatal(e)
	}
	if _, e = db.W.ExecContext(ctx, `DELETE FROM downloads WHERE id=?`, dl.ID); e == nil {
		t.Fatal("delete lost suppression/custody")
	}
	retry, e := db.BeginImportRetry(ctx, dl.ID, dl.GenerationKey)
	if e != nil {
		t.Fatal(e)
	}
	if retry.Epoch != a.Epoch+1 || retry.GenerationKey != a.GenerationKey {
		t.Fatal("retry changed acquisition identity or lost epoch")
	}
	if _, e = db.RequestImportCancellation(ctx, a.ImportAttemptKey); !errors.Is(e, ErrImportAttemptChanged) {
		t.Fatalf("stale cancel: %v", e)
	}
	if e = db.AuthorizeImportPlacement(ctx, p); !errors.Is(e, ErrImportAttemptChanged) {
		t.Fatalf("old authorization: %v", e)
	}
}

func TestWonPublicationCommitsAtomicallyAndFullPlanBeatsCancellation(t *testing.T) {
	db, dl, a := importControlFixture(t)
	ctx := context.Background()
	mustSeal(t, db, a, "/source/video")
	p := controlPlacement(dl, a, "winner", "/source/video", "/library/video")
	if e := db.PreparePlacement(ctx, p); e != nil {
		t.Fatal(e)
	}
	if e := db.AuthorizeImportPlacement(ctx, p); e != nil {
		t.Fatal(e)
	}
	if _, e := db.RequestImportCancellation(ctx, a.ImportAttemptKey); e != nil {
		t.Fatal(e)
	}
	if _, e := db.FinalizeImportCancellation(ctx, a.ImportAttemptKey); !errors.Is(e, ErrImportPublicationPending) {
		t.Fatalf("pending grant finalized: %v", e)
	}
	p.EpisodeIDs = []int64{99999}
	if _, e := db.CommitPlacement(ctx, p); e == nil {
		t.Fatal("invalid FK committed")
	}
	current, e := db.GetImportAttempt(ctx, dl.ID, dl.GenerationKey)
	if e != nil || current.PendingPublications != 1 {
		t.Fatalf("publication result escaped metadata rollback: %+v %v", current, e)
	}
	files, e := db.ImportAttemptFiles(ctx, a.ImportAttemptKey)
	if e != nil || files[0].Outcome != "pending" {
		t.Fatalf("file result escaped rollback: %+v %v", files, e)
	}
	p.EpisodeIDs = nil
	if _, e = db.CommitPlacement(ctx, p); e != nil {
		t.Fatal(e)
	}
	final, e := db.FinalizeImportCancellation(ctx, a.ImportAttemptKey)
	if e != nil || final.Outcome != "completed" {
		t.Fatalf("%+v %v", final, e)
	}
	fresh, e := db.GetDownload(ctx, dl.ID)
	if e != nil || fresh.State != "imported" {
		t.Fatalf("%+v %v", fresh, e)
	}
}

func TestPartialRetryReusesOnlyCurrentLogicalReceipt(t *testing.T) {
	db, dl, a := importControlFixture(t)
	ctx := context.Background()
	mustSeal(t, db, a, "/source/one", "/source/two")
	p := controlPlacement(dl, a, "first", "/source/one", "/library/one")
	if e := db.PreparePlacement(ctx, p); e != nil {
		t.Fatal(e)
	}
	if e := db.AuthorizeImportPlacement(ctx, p); e != nil {
		t.Fatal(e)
	}
	fid, e := db.CommitPlacement(ctx, p)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.RequestImportCancellation(ctx, a.ImportAttemptKey); e != nil {
		t.Fatal(e)
	}
	final, e := db.FinalizeImportCancellation(ctx, a.ImportAttemptKey)
	if e != nil || final.Outcome != "cancelled" {
		t.Fatalf("partial plan completed: %+v %v", final, e)
	}
	retry, e := db.BeginImportRetry(ctx, dl.ID, dl.GenerationKey)
	if e != nil {
		t.Fatal(e)
	}
	mustSeal(t, db, retry, p.Source)
	if e = db.SatisfyImportFile(ctx, retry.ImportAttemptKey, p); e != nil {
		t.Fatal(e)
	}
	// Same bytes at the same path with a different logical generation are a
	// different placement. Receipt replay must not claim that later owner.
	if _, e = db.W.ExecContext(ctx, `UPDATE media_files SET file_generation='replacement' WHERE id=?`, fid); e != nil {
		t.Fatal(e)
	}
	if e = db.SatisfyImportFile(ctx, retry.ImportAttemptKey, p); !errors.Is(e, ErrPlacementGenerationChanged) {
		t.Fatalf("old receipt rebound to replacement: %v", e)
	}
}

func TestExplicitReviewRevokesGrantWithoutDeletingBytesOrReactivatingIt(t *testing.T) {
	db, dl, a := importControlFixture(t)
	ctx := context.Background()
	mustSeal(t, db, a, "/source/video")
	p := controlPlacement(dl, a, "uncertain", "/source/video", "/library/video")
	if e := db.PreparePlacement(ctx, p); e != nil {
		t.Fatal(e)
	}
	if e := db.AuthorizeImportPlacement(ctx, p); e != nil {
		t.Fatal(e)
	}
	if _, e := db.RequestImportCancellation(ctx, a.ImportAttemptKey); e != nil {
		t.Fatal(e)
	}
	if _, e := db.RetainUncertainImport(ctx, a.ImportAttemptKey, []string{"wrong"}); !errors.Is(e, ErrImportAttemptChanged) {
		t.Fatalf("unreviewed grant revoked: %v", e)
	}
	final, e := db.RetainUncertainImport(ctx, a.ImportAttemptKey, []string{"placement:" + p.ID})
	if e != nil || final.Outcome != "cancelled" {
		t.Fatalf("%+v %v", final, e)
	}
	if _, e = db.CommitPlacement(ctx, p); !errors.Is(e, ErrImportAttemptChanged) {
		t.Fatalf("revoked grant committed: %v", e)
	}
	if e = db.AssertPublicationTargetAvailable(ctx, p.Target); !errors.Is(e, ErrImportReviewRequired) {
		t.Fatal("retained target reopened")
	}
	if _, e = db.BeginImportRetry(ctx, dl.ID, dl.GenerationKey); !errors.Is(e, ErrImportReviewRequired) {
		t.Fatal("retry forgot review")
	}
	if e = db.ResolveRetainedImportTargets(ctx, a.ImportAttemptKey, []string{"placement:" + p.ID}, "operator_adopted"); !errors.Is(e, ErrImportReviewRequired) {
		t.Fatal("unowned media adopted")
	}
	if e = db.ResolveRetainedImportTargets(ctx, a.ImportAttemptKey, []string{"placement:" + p.ID}, "operator_disposed"); e != nil {
		t.Fatal(e)
	}
	if e = db.AssertPublicationTargetAvailable(ctx, p.Target); e != nil {
		t.Fatal(e)
	}
	prior, e := db.GetPlacement(ctx, p.ID)
	if e != nil || prior.State != "abandoned" {
		t.Fatalf("%+v %v", prior, e)
	}
	if _, e = db.CommitPlacement(ctx, p); !errors.Is(e, ErrPlacementGenerationChanged) {
		t.Fatal("disposition reactivated old placement")
	}
}

func TestStageCustodySurvivesRetryAndRequiresExplicitDisposition(t *testing.T) {
	db, dl, a := importControlFixture(t)
	ctx := context.Background()
	if e := db.RecordImportStage(ctx, a.ImportAttemptKey, "/source/caption", "/library/caption", "/library/.stage"); e != nil {
		t.Fatal(e)
	}
	if _, e := db.RequestImportCancellation(ctx, a.ImportAttemptKey); e != nil {
		t.Fatal(e)
	}
	if _, e := db.FinalizeImportCancellation(ctx, a.ImportAttemptKey); e != nil {
		t.Fatal(e)
	}
	retry, e := db.BeginImportRetry(ctx, dl.ID, dl.GenerationKey)
	if e != nil {
		t.Fatal(e)
	}
	rows, e := db.ImportStageCustody(ctx, dl.ID, dl.GenerationKey)
	if e != nil || len(rows) != 1 {
		t.Fatalf("%+v %v", rows, e)
	}
	if e = db.ResolveRetainedImportTargets(ctx, retry.ImportAttemptKey, []string{rows[0].ID}, "operator_disposed"); e != nil {
		t.Fatal(e)
	}
	rows, e = db.ImportStageCustody(ctx, dl.ID, dl.GenerationKey)
	if e != nil || len(rows) != 0 {
		t.Fatalf("%+v %v", rows, e)
	}
}

func TestCancellationPruningSkipsProtectedRowsWithoutAbortingUnrelatedBatch(t *testing.T) {
	db, dl, a := importControlFixture(t)
	ctx := context.Background()
	if _, e := db.RequestImportCancellation(ctx, a.ImportAttemptKey); e != nil {
		t.Fatal(e)
	}
	if _, e := db.FinalizeImportCancellation(ctx, a.ImportAttemptKey); e != nil {
		t.Fatal(e)
	}
	if _, e := db.W.ExecContext(ctx, `UPDATE downloads SET state='failed' WHERE id=?`, dl.ID); e != nil {
		t.Fatal(e)
	}
	other, e := db.InsertDownload(ctx, Download{MediaItemID: dl.MediaItemID, ReleaseTitle: "unrelated", Protocol: "manual", State: "failed"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.Write.DeleteFailedDownloads(ctx); e != nil {
		t.Fatal(e)
	}
	if _, e = db.GetDownload(ctx, dl.ID); e != nil {
		t.Fatal("bulk pruning discarded suppression", e)
	}
	if _, e = db.GetDownload(ctx, other); !errors.Is(e, ErrNotFound) {
		t.Fatal("unrelated row not pruned", e)
	}
}

func TestFailedManualSelectionCannotClearPriorCancellation(t *testing.T) {
	db, dl, a := importControlFixture(t)
	ctx := context.Background()
	if _, e := db.RequestImportCancellation(ctx, a.ImportAttemptKey); e != nil {
		t.Fatal(e)
	}
	if _, e := db.FinalizeImportCancellation(ctx, a.ImportAttemptKey); e != nil {
		t.Fatal(e)
	}
	proposed, e := db.GetDownload(ctx, dl.ID)
	if e != nil {
		t.Fatal(e)
	}
	proposed.MediaItemID = 999999999
	proposed.ImportPath = "/new-selection"
	proposed.State = "downloaded"
	if _, e = db.CommitManualImportAdmission(ctx, proposed); e == nil {
		t.Fatal("invalid target committed")
	}
	control, e := db.GetImportAttempt(ctx, dl.ID, dl.GenerationKey)
	if e != nil || control.Epoch != a.Epoch || control.Outcome != "cancelled" {
		t.Fatalf("failed request unsuppressed old selection: %+v %v", control, e)
	}
	current, e := db.GetDownload(ctx, dl.ID)
	if e != nil || current.ImportPath != dl.ImportPath || current.MediaItemID != dl.MediaItemID {
		t.Fatalf("failed request mutated target: %+v %v", current, e)
	}
	proposed.MediaItemID = dl.MediaItemID
	if _, e = db.CommitManualImportAdmission(ctx, proposed); e != nil {
		t.Fatal(e)
	}
	control, e = db.GetImportAttempt(ctx, dl.ID, dl.GenerationKey)
	if e != nil || control.Epoch != a.Epoch+1 || control.Outcome != "" {
		t.Fatalf("successful selection not atomically admitted: %+v %v", control, e)
	}
}
