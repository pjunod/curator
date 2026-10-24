package acquisition

import (
	"context"
	"os"
	"testing"
)

func TestReplacementObservationPinWriteFailureRequiresFreshRetry(t *testing.T) {
	ctx := context.Background()
	client := &fakeClient{}
	svc, db, itemID := autoSetup(t, nil, client)
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	fileID, path := acq2AddFile(t, svc, itemID, 0, item.Path, "pin-retry.mkv", "Test.Movie.2024.720p.WEB-DL-GROUP")
	files, err := db.ListFilesForItem(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	var generation string
	for _, file := range files {
		if file.ID == fileID {
			generation = file.FileGeneration
		}
	}
	first, _, err := svc.StartReplacementFileObservation(ctx, itemID, fileID, generation, "pin-write-failure")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER fail_storage_identity_pin BEFORE INSERT ON replacement_storage_identity_pins BEGIN SELECT RAISE(ABORT,'simulated storage pin outage'); END`); err != nil {
		t.Fatal(err)
	}
	svc.processReplacementFileObservations(ctx)
	failed, err := svc.GetReplacementFileObservation(ctx, first.ID)
	if err != nil || failed.State != "failed" || failed.Error != "storage identity unverified; restore expected storage" {
		t.Fatalf("pin failure did not produce a review hold: observation=%+v err=%v", failed, err)
	}
	if _, err := db.GetReplacementStorageIdentity(ctx, item.Path); err == nil {
		t.Fatal("failed pin write enrolled an unverified storage identity")
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "bytes" {
		t.Fatalf("failed pin write changed the reviewed source: %q err=%v", got, err)
	}
	if len(client.added) != 0 {
		t.Fatalf("failed observation started replacement admission: %v", client.added)
	}

	if _, err := db.W.ExecContext(ctx, `DROP TRIGGER fail_storage_identity_pin`); err != nil {
		t.Fatal(err)
	}
	second, replayed, err := svc.StartReplacementFileObservation(ctx, itemID, fileID, generation, "pin-write-retry")
	if err != nil || replayed {
		t.Fatalf("new observation after database recovery=%+v replayed=%v err=%v", second, replayed, err)
	}
	svc.processReplacementFileObservations(ctx)
	ready, err := svc.GetReplacementFileObservation(ctx, second.ID)
	if err != nil || ready.State != "ready" || ready.SHA256 == "" || ready.FileIdentity == "" {
		t.Fatalf("fresh observation did not pin and verify the mounted source: observation=%+v err=%v", ready, err)
	}
	if _, err := db.GetReplacementStorageIdentity(ctx, item.Path); err != nil {
		t.Fatalf("recovered observation did not persist its trusted mount pin: %v", err)
	}
	if len(client.added) != 0 {
		t.Fatalf("storage observation alone unexpectedly admitted a replacement: %v", client.added)
	}
}
