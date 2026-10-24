package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestReplacementObservationRejectsPathSubstitutionAndRetriesAfterRepair(t *testing.T) {
	ctx := context.Background()
	client := &fakeClient{}
	svc, db, itemID := autoSetup(t, nil, client)
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	fileID, source := acq2AddFile(t, svc, itemID, 0, item.Path, "reviewed-source.mkv", "Test.Movie.2024.720p.WEB-DL-GROUP")
	files, err := db.ListFilesForItem(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	var fileGeneration string
	for _, file := range files {
		if file.ID == fileID {
			fileGeneration = file.FileGeneration
		}
	}
	if fileGeneration == "" {
		t.Fatal("fixture file has no filesystem generation")
	}
	first, _, err := svc.StartReplacementFileObservation(ctx, itemID, fileID, fileGeneration, "symlink-substitution")
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.mkv")
	const outsideBytes = "must remain outside the selected library path"
	if err := os.WriteFile(outside, []byte(outsideBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, source); err != nil {
		t.Fatal(err)
	}
	svc.processReplacementFileObservations(ctx)
	stale, err := svc.GetReplacementFileObservation(ctx, first.ID)
	if err != nil || stale.State != "stale" || stale.Error != "selected file path requires review" {
		t.Fatalf("substituted path observation=%+v err=%v", stale, err)
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != outsideBytes {
		t.Fatalf("observation followed or changed the symlink target: %q err=%v", got, err)
	}
	if info, err := os.Lstat(source); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("observation removed or followed the substituted link: info=%v err=%v", info, err)
	}
	if len(client.added) != 0 {
		t.Fatalf("stale observation admitted a replacement search: %v", client.added)
	}

	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	const repairedBytes = "repaired reviewed media bytes"
	if err := os.WriteFile(source, []byte(repairedBytes), 0o644); err != nil {
		t.Fatal(err)
	}
	second, replayed, err := svc.StartReplacementFileObservation(ctx, itemID, fileID, fileGeneration, "symlink-repaired")
	if err != nil || replayed {
		t.Fatalf("new observation after path repair=%+v replayed=%v err=%v", second, replayed, err)
	}
	svc.processReplacementFileObservations(ctx)
	ready, err := svc.GetReplacementFileObservation(ctx, second.ID)
	if err != nil || ready.State != "ready" || ready.Size != int64(len(repairedBytes)) || ready.SHA256 != sha256Hex([]byte(repairedBytes)) {
		t.Fatalf("repaired source was not freshly observed: observation=%+v err=%v", ready, err)
	}
	if len(client.added) != 0 {
		t.Fatalf("file observation alone unexpectedly admitted a replacement: %v", client.added)
	}
}
