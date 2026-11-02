package acquisition

import (
	"context"
	"errors"
	"github.com/pjunod/monarr/internal/domain/quality"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/pjunod/monarr/internal/infra/sqlite"
)

func TestCancellationPersistsWhileActualWriterOwnsDownload(t *testing.T) {
	svc, db, _ := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	id := grabEpisode(t, svc)
	dl, e := db.GetDownload(ctx, id)
	if e != nil {
		t.Fatal(e)
	}
	dl.State = "importing"
	dl.ImportPath = "/source/payload"
	if e = db.UpdateDownloadHandoff(ctx, dl); e != nil {
		t.Fatal(e)
	}
	a, e := db.EnsureImportAttempt(ctx, id, dl.GenerationKey)
	if e != nil {
		t.Fatal(e)
	}
	unlock := svc.lockDownload(id)
	writer, cancel := context.WithCancel(withImportAttempt(ctx, a.ImportAttemptKey))
	svc.importJobs = map[int64]*importTicket{id: {cancel: cancel, attempt: a.ImportAttemptKey}}
	result, e := svc.CancelImportAttempt(ctx, id, &a.ImportAttemptKey)
	if e != nil || result.Status != "cancel_requested" {
		unlock()
		t.Fatalf("%+v %v", result, e)
	}
	if !errors.Is(writer.Err(), context.Canceled) {
		unlock()
		t.Fatal("actual ticket not signalled")
	}
	control, e := db.GetImportAttempt(ctx, id, dl.GenerationKey)
	if e != nil || control.Outcome != "pending" {
		unlock()
		t.Fatalf("intent not durable: %+v %v", control, e)
	}
	// A heartbeat, timeout or second request cannot claim this writer exited.
	result, e = svc.CancelImportAttempt(ctx, id, &a.ImportAttemptKey)
	if e != nil || result.Status != "cancel_requested" {
		unlock()
		t.Fatalf("%+v %v", result, e)
	}
	svc.finishCancelledImport(writer, a.ImportAttemptKey)
	unlock()
	control, e = db.GetImportAttempt(ctx, id, dl.GenerationKey)
	if e != nil || control.Outcome != "cancelled" {
		t.Fatalf("%+v %v", control, e)
	}
}

func TestCancellationReviewCannotWaitOnOrBypassTitleOwner(t *testing.T) {
	svc, db, _ := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	id := grabEpisode(t, svc)
	dl, e := db.GetDownload(ctx, id)
	if e != nil {
		t.Fatal(e)
	}
	a, e := db.EnsureImportAttempt(ctx, id, dl.GenerationKey)
	if e != nil {
		t.Fatal(e)
	}
	_, release := svc.lockImportTarget(ctx, dl.MediaItemID)
	if _, e = svc.ReviewImportCancellation(ctx, a.ImportAttemptKey, "retain_uncertain", nil, true); !errors.Is(e, sqlite.ErrImportPublicationPending) {
		release()
		t.Fatalf("review bypassed title owner: %v", e)
	}
	release()
	if _, e = svc.ReviewImportCancellation(ctx, a.ImportAttemptKey, "retain_uncertain", nil, false); e == nil {
		t.Fatal("review lacked fencing attestation")
	}
}

func TestQueuedCancellationSuppressesRestartRecoveryAndOldEpoch(t *testing.T) {
	svc, db, _ := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	id := grabEpisode(t, svc)
	dl, e := db.GetDownload(ctx, id)
	if e != nil {
		t.Fatal(e)
	}
	dl.Protocol = "manual"
	dl.ClientID = 0
	dl.State = "downloaded"
	dl.ImportPath = "/never-touch-storage"
	if e = db.UpdateDownloadHandoff(ctx, dl); e != nil {
		t.Fatal(e)
	}
	a, e := db.EnsureImportAttempt(ctx, id, dl.GenerationKey)
	if e != nil {
		t.Fatal(e)
	}
	jobCtx, cancel := context.WithCancel(withImportAttempt(ctx, a.ImportAttemptKey))
	ticket := &importTicket{cancel: cancel, queued: true, attempt: a.ImportAttemptKey}
	svc.importJobs = map[int64]*importTicket{id: ticket}
	result, e := svc.CancelImportAttempt(ctx, id, &a.ImportAttemptKey)
	if e != nil || result.Status != "cancelled" {
		t.Fatalf("%+v %v", result, e)
	}
	svc.recoverManualImports(ctx)
	control, e := db.GetImportAttempt(ctx, id, dl.GenerationKey)
	if e != nil || control.Outcome != "cancelled" {
		t.Fatalf("restart erased cancellation: %+v %v", control, e)
	}
	fresh, e := db.BeginImportRetry(ctx, id, dl.GenerationKey)
	if e != nil {
		t.Fatal(e)
	}
	// A canceled channel entry from the old epoch cannot finalize/signal the new attempt.
	svc.runImportOwned(jobCtx, importJob{dl: dl, ctx: jobCtx, ticket: ticket})
	if _, e = svc.CancelImportAttempt(ctx, id, &a.ImportAttemptKey); !errors.Is(e, sqlite.ErrImportAttemptChanged) {
		t.Fatalf("stale request affected retry: %v", e)
	}
	current, e := db.GetImportAttempt(ctx, id, dl.GenerationKey)
	if e != nil || current.ImportAttemptKey != fresh.ImportAttemptKey || current.Outcome != "" {
		t.Fatalf("%+v %v", current, e)
	}
}

func TestNamedStageCannotAdoptIdenticalReplacementAfterClose(t *testing.T) {
	svc, db, itemID := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	item, e := db.GetMediaItemFull(ctx, itemID)
	if e != nil {
		t.Fatal(e)
	}
	root := t.TempDir()
	src := filepath.Join(root, "source.mkv")
	dest := filepath.Join(root, "library", "target.mkv")
	bytes := corpusFile(t, wholeCorpus1080)
	if e = os.WriteFile(src, bytes, 0644); e != nil {
		t.Fatal(e)
	}
	id, e := db.InsertDownload(ctx, sqlite.Download{MediaItemID: itemID, Protocol: "manual", State: "downloaded", ReleaseTitle: "stage identity", ImportPath: root})
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
	if e = db.SealImportAttempt(ctx, a.ImportAttemptKey, []sqlite.ImportAttemptFile{{Source: src, Kind: "media"}}, false); e != nil {
		t.Fatal(e)
	}
	prior := placeFile
	placeFile = func(c context.Context, source, target string, progress func(int64, int64)) error {
		if e := prior(c, source, target, progress); e != nil {
			return e
		}
		// Keep the original inode alive under another name so inode reuse cannot
		// accidentally make this same-bytes pathname replacement look identical.
		if e := os.Rename(target, target+".original"); e != nil {
			return e
		}
		return os.WriteFile(target, bytes, 0644)
	}
	t.Cleanup(func() { placeFile = prior })
	_, e = svc.commitPlacement(withImportAttempt(ctx, a.ImportAttemptKey), item, importScope{DownloadID: id, SourceGeneration: dl.GenerationKey, Attempt: "epoch-stage"}, src, dest, quality.Quality{}, nil, false)
	if e == nil || !strings.Contains(e.Error(), "opened object") {
		t.Fatalf("replacement stage adopted: %v", e)
	}
	if _, e = os.Stat(dest); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("replacement stage published", e)
	}
	pubs, e := db.ImportPublications(ctx, id, dl.GenerationKey)
	if e != nil || len(pubs) != 0 {
		t.Fatalf("replacement authorized: %+v %v", pubs, e)
	}
}

func TestJournaledStageAppliesConfiguredModeUnderRestrictiveUmask(t *testing.T) {
	svc, db, itemID := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	root := t.TempDir()
	src := filepath.Join(root, "source")
	stage := filepath.Join(root, ".stage")
	if e := os.WriteFile(src, []byte("payload"), 0644); e != nil {
		t.Fatal(e)
	}
	id, e := db.InsertDownload(ctx, sqlite.Download{MediaItemID: itemID, Protocol: "manual", State: "downloaded", ReleaseTitle: "mode"})
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
	epoch := a.Epoch
	p := sqlite.Placement{ID: "mode-stage", DownloadID: id, SourceGeneration: dl.GenerationKey, ImportEpoch: &epoch, Source: src, Temporary: stage, Target: filepath.Join(root, "target"), State: "prepared", Size: 7}
	if e = db.PreparePlacement(ctx, p); e != nil {
		t.Fatal(e)
	}
	previous := syscall.Umask(0077)
	defer syscall.Umask(previous)
	if e = svc.stageImportPlacement(withImportAttempt(ctx, a.ImportAttemptKey), &p, nil); e != nil {
		t.Fatal(e)
	}
	info, e := os.Stat(stage)
	if e != nil {
		t.Fatal(e)
	}
	if info.Mode().Perm() != fileMode().Perm() {
		t.Fatalf("journaled mode=%v expected=%v", info.Mode().Perm(), fileMode().Perm())
	}
}
