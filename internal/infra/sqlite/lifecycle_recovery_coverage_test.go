package sqlite

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/ports"
)

func openLifecycleCoverageDB(t *testing.T) (*DB, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Migrate(context.Background()); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	return db, dir
}

func coverageDelivery(deliveryID string) ports.RunnerDelivery {
	return ports.RunnerDelivery{
		Sequence: 1, DeliveryID: deliveryID, PublicationGeneration: "publication-generation",
		ArtifactID: "artifact", JobID: "17", RequestToken: "t-17-0123456789abcdef0123456789abcdef",
		RunnerInstanceID: "runner-instance", PublicationPath: "/completed/release",
		ManifestDigest: "manifest-digest", ReadyAt: time.Now(),
		Files: []ports.RunnerFile{{Path: "Movie.mkv", SizeBytes: 5, SHA256: "2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881"}},
	}
}

func TestDeliveryAttemptJournalKeepsInterruptedAndFinishedAttemptsAcrossRestart(t *testing.T) {
	ctx := context.Background()
	db, dir := openLifecycleCoverageDB(t)
	defer func() { _ = db.Close() }()
	receipt, err := db.SaveDiscoveredDelivery(ctx, 3, coverageDelivery("attempt-journal"), "/mounted/release")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.BeginDeliveryAttempt(ctx, 0, "import"); err == nil {
		t.Fatal("attempt without a receipt was admitted")
	}
	if _, err = db.BeginDeliveryAttempt(ctx, receipt.ID, " "); err == nil {
		t.Fatal("attempt without a stage was admitted")
	}
	first, err := db.BeginDeliveryAttempt(ctx, receipt.ID, "import")
	if err != nil || first.Attempt != 1 || first.Result != "running" || !first.FinishedAt.IsZero() {
		t.Fatalf("first attempt=%+v err=%v", first, err)
	}
	if err = db.FinishDeliveryAttempt(ctx, first.ID, "running", "not terminal"); err == nil {
		t.Fatal("running was accepted as a terminal result")
	}
	if err = db.FinishDeliveryAttempt(ctx, first.ID, "retry", "source temporarily busy"); err != nil {
		t.Fatal(err)
	}
	if err = db.FinishDeliveryAttempt(ctx, first.ID, "succeeded", "duplicate finish"); !errors.Is(err, ErrDeliveryReceiptStale) {
		t.Fatalf("finished attempt accepted a second transition: %v", err)
	}
	interrupted, err := db.BeginDeliveryAttempt(ctx, receipt.ID, "import")
	if err != nil || interrupted.Attempt != 2 {
		t.Fatalf("second attempt=%+v err=%v", interrupted, err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	events, err := db.DeliveryAttemptEvents(ctx, receipt.ID)
	if err != nil || len(events) != 2 {
		t.Fatalf("attempt history=%+v err=%v", events, err)
	}
	if events[0].Attempt != 1 || events[0].Result != "retry" || events[0].Error != "source temporarily busy" || events[0].FinishedAt.IsZero() {
		t.Fatalf("finished attempt history lost its outcome: %+v", events[0])
	}
	if events[1].Attempt != 2 || events[1].Result != "running" || !events[1].FinishedAt.IsZero() {
		t.Fatalf("in-flight attempt was falsely finalized during restart: %+v", events[1])
	}
	third, err := db.BeginDeliveryAttempt(ctx, receipt.ID, "import")
	if err != nil || third.Attempt != 3 {
		t.Fatalf("attempt sequence did not continue after reopen: %+v err=%v", third, err)
	}
}

func TestQueueDeliveryReimportAtomicallyPersistsExactSelectionWithAckPending(t *testing.T) {
	ctx := context.Background()
	db, dir := openLifecycleCoverageDB(t)
	defer func() { _ = db.Close() }()
	if _, err := db.W.ExecContext(ctx, `INSERT INTO media_items(id,kind,title,sort_title,quality_profile_id,added_at,updated_at)
		VALUES(91,'movie','Lifecycle Movie','lifecycle movie',1,1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `INSERT INTO downloads(id,media_item_id,release_title,client_id,protocol,handle,transfer,state,added_at,updated_at,generation_key,import_path)
		VALUES(91,91,'Lifecycle Movie 2024 1080p WEB-DL',3,'usenet','17',?,'imported',1,1,'download-generation','/mounted/release')`, coverageDelivery("unused").RequestToken); err != nil {
		t.Fatal(err)
	}
	delivery := coverageDelivery("reimport-transaction")
	receipt, err := db.SaveDiscoveredDelivery(ctx, 3, delivery, "/mounted/release")
	if err != nil {
		t.Fatal(err)
	}
	receipt.DownloadID, receipt.GenerationKey, receipt.DownloadHandle, receipt.MediaItemID = 91, "download-generation", "17", 91
	if err = db.AssociateDelivery(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	receipt.ConsumerReceiptID = "consumer-receipt"
	if err = db.PrepareDeliveryAcceptance(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	if err = db.SetDeliveryImported(ctx, receipt.ID); err != nil {
		t.Fatal(err)
	}
	expected, err := db.DeliveryReceiptByID(ctx, receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	dl, err := db.GetDownload(ctx, 91)
	if err != nil {
		t.Fatal(err)
	}
	dl.State, dl.Handoff = "importing", []HandoffEntry{{Step: "manual_import_queued", At: time.Now().UnixMilli(), Paths: []string{"Movie.mkv"}}}

	wrong := expected
	wrong.GenerationKey = "stale-generation"
	if err = db.QueueDeliveryReimport(ctx, wrong, dl); !errors.Is(err, ErrDeliveryReceiptStale) {
		t.Fatalf("stale generation reimport error=%v", err)
	}
	unchanged, err := db.GetDownload(ctx, dl.ID)
	if err != nil || unchanged.State != "imported" || len(unchanged.Handoff) != 0 {
		t.Fatalf("failed transaction partially rewrote download: %+v err=%v", unchanged, err)
	}
	unchangedReceipt, err := db.DeliveryReceiptByID(ctx, receipt.ID)
	if err != nil || unchangedReceipt.State != "imported" || unchangedReceipt.AckState != "pending" {
		t.Fatalf("failed transaction partially reopened receipt: %+v err=%v", unchangedReceipt, err)
	}

	if err = db.QueueDeliveryReimport(ctx, expected, dl); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	reopenedReceipt, err := db.DeliveryReceiptByID(ctx, receipt.ID)
	if err != nil || reopenedReceipt.State != "ready" || reopenedReceipt.AckState != "pending" || reopenedReceipt.ConsumerReceiptID != "consumer-receipt" {
		t.Fatalf("reopened receipt lost selected retry or ACK obligation: %+v err=%v", reopenedReceipt, err)
	}
	reopenedDownload, err := db.GetDownload(ctx, dl.ID)
	if err != nil || reopenedDownload.State != "importing" || len(reopenedDownload.Handoff) != 1 || !reflect.DeepEqual(reopenedDownload.Handoff[0].Paths, []string{"Movie.mkv"}) {
		t.Fatalf("reopened download lost exact selection: %+v err=%v", reopenedDownload, err)
	}
}

func TestReplacementOperationReservationIsEpisodeScopedAndCopySpecific(t *testing.T) {
	ctx := context.Background()
	db, _ := openLifecycleCoverageDB(t)
	defer func() { _ = db.Close() }()
	first := ReplacementOperation{ID: "scope-first", RequestID: "scope-request-first", Fingerprint: "scope-fp-first",
		State: "searching", Intent: "replace_quality", Scope: "episodes", MediaItemID: 77, CopyID: 0, EpisodeIDs: []int64{1, 2}}
	if _, _, err := db.CreateReplacementOperation(ctx, first, nil); err != nil {
		t.Fatal(err)
	}
	overlap := first
	overlap.ID, overlap.RequestID, overlap.Fingerprint, overlap.EpisodeIDs = "scope-overlap", "scope-request-overlap", "scope-fp-overlap", []int64{2, 3}
	if _, _, err := db.CreateReplacementOperation(ctx, overlap, nil); !errors.Is(err, ErrReplacementScopeConflict) {
		t.Fatalf("overlapping active episode reservation error=%v", err)
	}
	disjoint := first
	disjoint.ID, disjoint.RequestID, disjoint.Fingerprint, disjoint.EpisodeIDs = "scope-disjoint", "scope-request-disjoint", "scope-fp-disjoint", []int64{3}
	if _, _, err := db.CreateReplacementOperation(ctx, disjoint, nil); err != nil {
		t.Fatalf("disjoint episode was unnecessarily blocked: %v", err)
	}
	otherCopy := first
	otherCopy.ID, otherCopy.RequestID, otherCopy.Fingerprint, otherCopy.CopyID = "scope-copy", "scope-request-copy", "scope-fp-copy", 9
	if _, _, err := db.CreateReplacementOperation(ctx, otherCopy, nil); err != nil {
		t.Fatalf("same episode in a separate copy was unnecessarily blocked: %v", err)
	}
	if err := db.TransitionReplacementOperation(ctx, disjoint.ID, "complete", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.TransitionReplacementOperation(ctx, otherCopy.ID, "complete", ""); err != nil {
		t.Fatal(err)
	}
	downloadScope := first
	downloadScope.ID, downloadScope.RequestID, downloadScope.Fingerprint, downloadScope.Scope, downloadScope.EpisodeIDs = "scope-download", "scope-request-download", "scope-fp-download", "download", nil
	if _, _, err := db.CreateReplacementOperation(ctx, downloadScope, nil); !errors.Is(err, ErrReplacementScopeConflict) {
		t.Fatalf("whole-copy reservation did not conflict with active episode scope: %v", err)
	}
	if err := db.TransitionReplacementOperation(ctx, first.ID, "complete", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.CreateReplacementOperation(ctx, downloadScope, nil); err != nil {
		t.Fatalf("terminal episode operation did not release its reservation: %v", err)
	}
}

func TestReplacementObservationPinsExactFileGenerationAndStorage(t *testing.T) {
	ctx := context.Background()
	db, _ := openLifecycleCoverageDB(t)
	defer func() { _ = db.Close() }()
	if _, err := db.W.ExecContext(ctx, `INSERT INTO media_items(id,kind,title,sort_title,quality_profile_id,added_at,updated_at)
		VALUES(77,'movie','Observed Movie','observed movie',1,1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `INSERT INTO media_files(id,media_item_id,path,size,added_at,source_generation)
		VALUES(770,77,'/library/Observed Movie.mkv',123,1,'source-generation')`); err != nil {
		t.Fatal(err)
	}
	var generation string
	if err := db.R.QueryRowContext(ctx, `SELECT file_generation FROM media_files WHERE id=770`).Scan(&generation); err != nil || generation == "" {
		t.Fatalf("fixture file generation=%q err=%v", generation, err)
	}
	request := ReplacementFileObservation{ID: "observation-770", RequestID: "observation-request-770", MediaItemID: 77, MediaFileID: 770, FileGeneration: generation, StorageIdentity: "nfs:server:/library"}
	created, replay, err := db.StartReplacementFileObservation(ctx, request)
	if err != nil || replay || created.State != "pending" || created.Path != "/library/Observed Movie.mkv" || created.SourceGeneration != "source-generation" {
		t.Fatalf("start exact observation=%+v replay=%v err=%v", created, replay, err)
	}
	duplicate, replay, err := db.StartReplacementFileObservation(ctx, request)
	if err != nil || !replay || duplicate.ID != request.ID {
		t.Fatalf("same observation request did not return durable work: %+v replay=%v err=%v", duplicate, replay, err)
	}
	changed := request
	changed.ID, changed.FileGeneration = "observation-reused", "different-generation"
	if _, replay, err = db.StartReplacementFileObservation(ctx, changed); !errors.Is(err, ErrReplacementRequestConflict) || replay {
		t.Fatalf("request replay inherited a replacement file generation: replay=%v err=%v", replay, err)
	}
	pending, err := db.ListPendingReplacementFileObservations(ctx, 0)
	if err != nil || len(pending) != 1 || pending[0].ID != request.ID {
		t.Fatalf("pending observation queue=%+v err=%v", pending, err)
	}
	created.State, created.SHA256, created.FileIdentity, created.FilesystemGeneration, created.Size = "ready", "sha256-reviewed", "file-object-1", "stat-generation-1", 123
	if err = db.FinishReplacementFileObservation(ctx, created); err != nil {
		t.Fatal(err)
	}
	if err = db.FinishReplacementFileObservation(ctx, created); !errors.Is(err, ErrNotFound) {
		t.Fatalf("completed observation accepted a duplicate finish: %v", err)
	}
	ready, err := db.GetReplacementFileObservation(ctx, request.ID)
	if err != nil || ready.State != "ready" || ready.SHA256 != "sha256-reviewed" || ready.FilesystemGeneration != "stat-generation-1" || ready.Size != 123 {
		t.Fatalf("ready observation evidence=%+v err=%v", ready, err)
	}
	if err = db.PinReplacementStorageIdentity(ctx, "/library", ready.StorageIdentity, ready.ID); err != nil {
		t.Fatal(err)
	}
	pinned, err := db.GetReplacementStorageIdentity(ctx, "/library")
	if err != nil || pinned != ready.StorageIdentity {
		t.Fatalf("storage identity pin=%q err=%v", pinned, err)
	}
	if err = db.PinReplacementStorageIdentity(ctx, "/library", "different-source", ready.ID); !errors.Is(err, ErrReplacementStorageIdentityConflict) {
		t.Fatalf("storage pin changed identity without review: %v", err)
	}
	prior, observationID, err := db.PriorVerifiedReplacementStorageIdentity(ctx, 770, generation)
	if err != nil || prior != ready.StorageIdentity || observationID != ready.ID {
		t.Fatalf("legacy verified storage bridge=(%q,%q) err=%v", prior, observationID, err)
	}
}

func TestLibraryReviewFlagCannotTransferAcrossFileGeneration(t *testing.T) {
	ctx := context.Background()
	db, _ := openLifecycleCoverageDB(t)
	defer func() { _ = db.Close() }()
	if _, err := db.W.ExecContext(ctx, `INSERT INTO media_items(id,kind,title,sort_title,quality_profile_id,added_at,updated_at)
		VALUES(78,'movie','Flagged Movie','flagged movie',1,1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `INSERT INTO media_files(id,media_item_id,path,size,added_at)
		VALUES(780,78,'/library/Flagged Movie.mkv',456,1)`); err != nil {
		t.Fatal(err)
	}
	var generation string
	if err := db.R.QueryRowContext(ctx, `SELECT file_generation FROM media_files WHERE id=780`).Scan(&generation); err != nil || generation == "" {
		t.Fatalf("fixture file generation=%q err=%v", generation, err)
	}
	if err := db.CreateLibraryReviewFlag(ctx, 78, 780, generation, "damaged_output", "probe found issues", `{}`); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateLibraryReviewFlag(ctx, 78, 780, generation, "damaged_output", "updated evidence", `{"probe":"failed"}`); err != nil {
		t.Fatalf("same-generation report did not refresh the open flag: %v", err)
	}
	flags, err := db.ListOpenMediaFileReviewFlags(ctx, 78)
	if err != nil || len(flags) != 1 || flags[0].Reason != "updated evidence" {
		t.Fatalf("open review evidence=%+v err=%v", flags, err)
	}
	open, err := db.HasOpenMediaFileReviewFlag(ctx, 780, generation)
	if err != nil || !open {
		t.Fatalf("exact file generation was not fenced while under review: open=%v err=%v", open, err)
	}
	if err = db.ResolveMediaFileReviewFlag(ctx, 78, flags[0].ID, "replacement-generation"); !errors.Is(err, ErrReplacementScopeConflict) {
		t.Fatalf("flag transferred to a different library generation: %v", err)
	}
	if err = db.ResolveMediaFileReviewFlag(ctx, 78, flags[0].ID, generation); err != nil {
		t.Fatal(err)
	}
	open, err = db.HasOpenMediaFileReviewFlag(ctx, 780, generation)
	if err != nil || open {
		t.Fatalf("resolved review still blocks exact generation: open=%v err=%v", open, err)
	}
	if err = db.ResolveMediaFileReviewFlag(ctx, 78, flags[0].ID, generation); !errors.Is(err, ErrNotFound) {
		t.Fatalf("resolved flag accepted a second resolution: %v", err)
	}
}

func TestOpenReplacementQueryServesDueWorkAheadOfDeferredBacklogAfterRestart(t *testing.T) {
	ctx := context.Background()
	db, dir := openLifecycleCoverageDB(t)
	for i := 0; i < 33; i++ {
		op := ReplacementOperation{ID: fmt.Sprintf("deferred-%02d", i), RequestID: fmt.Sprintf("deferred-request-%02d", i), Fingerprint: fmt.Sprintf("deferred-fingerprint-%02d", i),
			State: "searching", Intent: "replace_quality", Scope: "episodes", MediaItemID: int64(1000 + i), EpisodeIDs: []int64{1}}
		created, _, err := db.CreateReplacementOperation(ctx, op, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err = db.TransitionReplacementOperation(ctx, created.ID, "waiting_for_release", "provider temporarily unavailable"); err != nil {
			t.Fatal(err)
		}
	}
	due, _, err := db.CreateReplacementOperation(ctx, ReplacementOperation{ID: "due-now", RequestID: "due-now-request", Fingerprint: "due-now-fingerprint",
		State: "searching", Intent: "replace_quality", Scope: "episodes", MediaItemID: 2000, EpisodeIDs: []int64{7}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	page, err := db.ListOpenReplacementOperations(ctx, 1)
	if err != nil || len(page) != 1 || page[0].ID != due.ID {
		t.Fatalf("due replacement work was starved behind deferred rows: rows=%+v err=%v", page, err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	page, err = db.ListOpenReplacementOperations(ctx, 1)
	if err != nil || len(page) != 1 || page[0].ID != due.ID {
		t.Fatalf("restart changed due-work priority: rows=%+v err=%v", page, err)
	}
	deferred, err := db.GetReplacementOperation(ctx, "deferred-00")
	if err != nil || deferred.State != "waiting_for_release" || deferred.RetryAttempts != 1 || deferred.NextAttemptAt.Before(time.Now()) {
		t.Fatalf("deferred retry deadline did not survive reopen: operation=%+v err=%v", deferred, err)
	}
}
