package sqlite

import (
	"context"
	"github.com/pressly/goose/v3"
	"testing"
)

func TestAbandonDeleteRequestRetainsCompletedHistoryAndCannotRevive(t *testing.T) {
	ctx := context.Background()
	db, r, p := atomicDeliveryFixture(t)
	seedLegacyFileOutcomes(t, db, r.ID, []DeliveryManifestFile{{Path: "done.mkv", SizeBytes: 1, SHA256: "other-digest", FileGeneration: "other-stat"}}, nil)
	batch := ExplicitDeliveryDeleteBatch{ID: "delete-request", ReceiptID: r.ID, PayloadPath: r.MappedPath, InventoryFingerprint: "review", PublicationDirectoryIdentity: "directory", StorageIdentity: "storage", GenerationKey: p.SourceGeneration, RequestToken: p.SourceTransfer, PublicationGeneration: p.SourcePayloadGeneration, Files: []ExplicitDeliveryDeleteFile{
		{Path: "movie.mkv", ReviewedPath: p.Source, SnapshotIdentity: "source-stat", ParentIdentity: "parent"},
		{Path: "done.mkv", ReviewedPath: r.MappedPath + "/done.mkv", SnapshotIdentity: "other-stat", ParentIdentity: "parent"},
	}}
	if err := db.PrepareExplicitDeliveryDeleteBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteExplicitDeliveryFileDelete(ctx, batch.ID, r.ID, "done.mkv", "delivery_file_deleted", r.MediaItemID, "done", map[string]any{"actualUnlink": true}); err != nil {
		t.Fatal(err)
	}
	if err := db.AbandonExplicitDeliveryDelete(ctx, r.ID+1, batch.ID); err == nil {
		t.Fatal("wrong receipt abandoned deletion")
	}
	if err := db.AbandonExplicitDeliveryDelete(ctx, r.ID, batch.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.AbandonExplicitDeliveryDelete(ctx, r.ID, batch.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.ScheduleExplicitDeliveryDeleteRetry(ctx, batch.ID, "late worker failure"); err != nil {
		t.Fatal(err)
	}
	pending, err := db.ExplicitDeliveryDeleteBatchesPending(ctx, r.ID, 10)
	if err != nil || len(pending) != 0 {
		t.Fatalf("cancelled request remained pending: %+v %v", pending, err)
	}
	pendingFile, err := db.DeliveryFileOutcomeByPath(ctx, r.ID, "movie.mkv")
	if err != nil || pendingFile.CleanupState != "protected" {
		t.Fatalf("cancelled selection gained deletion eligibility: %+v %v", pendingFile, err)
	}
	completedFile, err := db.DeliveryFileOutcomeByPath(ctx, r.ID, "done.mkv")
	if err != nil || completedFile.CleanupState != "removed" {
		t.Fatalf("cancel lost completed outcome: %+v %v", completedFile, err)
	}
	var deletes, cancels int
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM history_events WHERE type='delivery_file_deleted'`).Scan(&deletes); err != nil {
		t.Fatal(err)
	}
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM history_events WHERE type='delivery_delete_request_cancelled'`).Scan(&cancels); err != nil {
		t.Fatal(err)
	}
	if deletes != 1 || cancels != 1 {
		t.Fatalf("history changed: deletes=%d cancels=%d", deletes, cancels)
	}
	if err := db.BeginExplicitDeliveryFileUnlink(ctx, batch.ID, "movie.mkv"); err == nil {
		t.Fatal("late worker revived cancelled request")
	}
	if err := db.PrepareExplicitDeliveryDeleteBatch(ctx, batch); err == nil {
		t.Fatal("cancelled reviewed identity reused")
	}
	if err := db.CompleteExplicitDeliveryFileDelete(ctx, batch.ID, r.ID, "movie.mkv", "delivery_file_deleted", r.MediaItemID, "movie", nil); err == nil {
		t.Fatal("late worker fabricated cancellation deletion")
	}
	if err := goose.DownToContext(ctx, db.W, "migrations", 64); err == nil {
		t.Fatal("downgrade erased cancelled deletion authority")
	}
	cancelled, err := db.ExplicitDeliveryDeleteBatch(ctx, batch.ID)
	if err != nil || cancelled.AbandonedAt.IsZero() {
		t.Fatalf("failed downgrade lost cancellation: %+v %v", cancelled, err)
	}
	// A newly reviewed request has its own authority; cancelled rows do not block it.
	batch.ID = "new-delete-request"
	batch.Files = batch.Files[:1]
	if err := db.PrepareExplicitDeliveryDeleteBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
	conflict := batch
	conflict.ID = "competing-delete-request"
	if err := db.PrepareExplicitDeliveryDeleteBatch(ctx, conflict); err == nil {
		t.Fatal("overlapping pending deletion request admitted")
	}
}
