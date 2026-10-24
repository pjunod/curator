package acquisition

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/pjunod/monarr/internal/domain/mediainfo"
	"github.com/pjunod/monarr/internal/domain/quality"
)

func TestQualityReplacementKeepsOldFileWhenCommittedPlacementDigestDrifts(t *testing.T) {
	ctx := context.Background()
	svc, db, itemID := autoSetup(t, nil, &fakeClient{})
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	oldID, oldPath := acq2AddFile(t, svc, itemID, 0, item.Path, "old-quality.mkv", "")
	if err := db.SetFileQualityFrom(ctx, oldID, quality.Quality{Source: quality.SourceWEBDL, Resolution: 720},
		mediainfo.ProvenanceFilename, mediainfo.ConfidenceNone); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE media_files SET source_generation='old-quality-source',source_transfer='old-quality-transfer' WHERE id=?`, oldID); err != nil {
		t.Fatal(err)
	}
	files, err := db.ListFilesForItem(ctx, itemID)
	if err != nil || len(files) != 1 {
		t.Fatalf("old library rows=%+v err=%v", files, err)
	}
	observation := observeReplacementFile(t, svc, itemID, oldID, files[0].FileGeneration, "quality-digest-drift")
	blacklist := false
	op, _, err := svc.CreateReplacement(ctx, ReplacementRequest{
		RequestID: "quality-digest-drift", Scope: "download", MediaItemID: itemID,
		SourceFileID: oldID, Intent: "replace_quality", BlacklistSourceRelease: &blacklist,
		ReasonCode: "quality_preference", Reason: "verify current replacement bytes before cleanup",
		ObservedFiles: []ReplacementObservedFile{{ID: oldID, FileGeneration: files[0].FileGeneration, ObservationID: observation.ID}},
	})
	if err != nil {
		t.Fatal(err)
	}

	source := filepath.Join(t.TempDir(), "new-quality.mkv")
	payload := []byte("reviewed replacement quality bytes")
	if err := os.WriteFile(source, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(item.Path, "new-quality.mkv")
	if _, err := svc.commitPlacement(ctx, item, importScope{
		Attempt: "quality-digest-drift", ReplacementOperationID: op.ID,
		Release: "Test.Movie.2024.1080p.WEB-DL.NEW", Indexer: "idx",
	}, source, target, quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080}, nil, true); err != nil {
		t.Fatalf("commit replacement placement: %v", err)
	}

	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	changed := bytes.Repeat([]byte("x"), len(payload))
	if err := os.WriteFile(target, changed, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(target, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(changed, payload) {
		t.Fatal("fixture replacement unexpectedly retained the committed bytes")
	}
	if err := svc.processReplacementOperation(ctx, op); err == nil {
		t.Fatal("worker retired the old quality file after the committed replacement digest changed")
	}
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatalf("old usable file was retired after replacement proof failed: %v", err)
	}
	if _, err := db.GetMediaFileCustodyByPath(ctx, oldPath); err != nil {
		t.Fatalf("failed replacement proof discarded old library ownership: %v", err)
	}
	if got, err := os.ReadFile(target); err != nil || !bytes.Equal(got, changed) {
		t.Fatalf("worker changed the externally modified replacement: bytes=%q err=%v", got, err)
	}

	if err := os.WriteFile(target, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	updated, err := db.GetReplacementOperation(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.processReplacementOperation(ctx, updated); err != nil {
		t.Fatalf("worker did not retry cleanup after exact replacement bytes were restored: %v", err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("exactly superseded quality file remains after valid retry: %v", err)
	}
	if _, err := db.GetMediaFileCustodyByPath(ctx, oldPath); err == nil {
		t.Fatal("old library ownership remained after valid replacement cleanup")
	}
	if got, err := os.ReadFile(target); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("restored replacement was lost during cleanup retry: bytes=%q err=%v", got, err)
	}
	final, err := db.GetReplacementOperation(ctx, op.ID)
	if err != nil || final.State != "complete" {
		t.Fatalf("replacement operation after exact retry=%+v err=%v", final, err)
	}
}
