package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
)

func TestFinishStaleReplacementObservationIsBoundToItsImmutableTuple(t *testing.T) {
	ctx := context.Background()
	db, _ := openLifecycleCoverageDB(t)
	defer func() { _ = db.Close() }()
	itemID, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Stale observation tuple", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	const fileID = int64(943)
	const path = "/library/stale-tuple.mkv"
	const generation = "review-generation-1"
	if _, err := db.W.ExecContext(ctx, `INSERT INTO media_files(id,media_item_id,path,size,added_at,file_generation,source_generation) VALUES(?,?,?,?,?,?,?)`, fileID, itemID, path, 120, time.Now().UnixMilli(), generation, "source-generation"); err != nil {
		t.Fatal(err)
	}
	o, replay, err := db.StartReplacementFileObservation(ctx, ReplacementFileObservation{ID: "stale-observation-943", RequestID: "stale-observation-request-943", MediaItemID: itemID, MediaFileID: fileID, FileGeneration: generation, StorageIdentity: "disk:library"})
	if err != nil || replay || o.State != "pending" {
		t.Fatalf("start observation=%+v replay=%v err=%v", o, replay, err)
	}
	if _, err := db.W.ExecContext(ctx, `DELETE FROM media_files WHERE id=?`, fileID); err != nil {
		t.Fatal(err)
	}

	wrongTuple := o
	wrongTuple.State = "stale"
	wrongTuple.MediaFileID++
	wrongTuple.Error = "must not finish another tuple"
	if err := db.FinishReplacementFileObservation(ctx, wrongTuple); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale finish with mismatched immutable tuple=%v, want not found", err)
	}
	pending, err := db.GetReplacementFileObservation(ctx, o.ID)
	if err != nil || pending.State != "pending" {
		t.Fatalf("mismatched stale finish changed the pending observation: %+v err=%v", pending, err)
	}

	o.State, o.Error = "stale", "selected library row disappeared during observation"
	o.SHA256, o.FileIdentity, o.FilesystemGeneration, o.Size = "untrusted-digest", "untrusted-object", "untrusted-stat", 120
	if err := db.FinishReplacementFileObservation(ctx, o); err != nil {
		t.Fatalf("exact stale tuple could not close after row removal: %v", err)
	}
	finished, err := db.GetReplacementFileObservation(ctx, o.ID)
	if err != nil || finished.State != "stale" || finished.Error != o.Error {
		t.Fatalf("stale terminal state was not persisted: %+v err=%v", finished, err)
	}
	if finished.SHA256 != "" || finished.FileIdentity != "" || finished.FilesystemGeneration != "" || finished.Size != 0 {
		t.Fatalf("stale transition retained caller-provided filesystem evidence: %+v", finished)
	}
}
