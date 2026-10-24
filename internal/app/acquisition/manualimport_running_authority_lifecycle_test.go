package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pjunod/monarr/internal/app/transfers"
)

func TestRunningManualImportRejectsSecondSelectionWithoutReplacingAdmission(t *testing.T) {
	svc, db, itemID := setup(t, nil, &fakeClient{})
	svc.SetRegistry(transfers.New())
	// Keep the first admission in the worker queue, then model the worker's
	// queued->running transition without introducing a filesystem blocking
	// hook or a goroutine that can contend with the admission locks.
	svc.importCh = make(chan importJob, 1)
	svc.importCtx = context.Background()
	svc.importJobs = make(map[int64]*importTicket)

	root := t.TempDir()
	firstDir := filepath.Join(root, "first-reviewed-payload")
	firstPath := filepath.Join(firstDir, "Test.Show.S01E01.1080p.WEB-DL.mkv")
	secondDir := filepath.Join(root, "second-reviewed-payload")
	secondPath := filepath.Join(secondDir, "Test.Show.S01E01.1080p.WEB-DL.mkv")
	for path, body := range map[string]string{firstPath: "first user-selected bytes", secondPath: "second user-selected bytes"} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	id, err := svc.QueueManualImport(context.Background(), ManualImportRequest{
		Path: firstDir, Paths: []string{firstPath}, MediaItemID: itemID,
	})
	if err != nil || id == 0 {
		t.Fatalf("first manual admission id=%d err=%v", id, err)
	}
	if got := svc.ImportStatus(id); got != "queued" {
		t.Fatalf("first manual import status=%q, want queued", got)
	}
	before, err := db.GetDownload(context.Background(), id)
	if err != nil || before.ImportPath != firstDir || len(before.Handoff) < 1 || before.Handoff[0].Step != stepManualQueued || len(before.Handoff[0].Paths) != 1 || before.Handoff[0].Paths[0] != firstPath {
		t.Fatalf("first durable selection=%+v err=%v", before, err)
	}

	svc.importMu.Lock()
	svc.importJobs[id].queued = false
	svc.importMu.Unlock()
	if got := svc.ImportStatus(id); got != "running" {
		t.Fatalf("first manual import status=%q, want running", got)
	}
	if _, err := svc.QueueManualImport(context.Background(), ManualImportRequest{
		Path: secondDir, Paths: []string{secondPath}, MediaItemID: itemID, DownloadID: id,
	}); err == nil || !strings.Contains(err.Error(), "import is already running") {
		t.Fatalf("concurrent manual re-selection error=%v, want running import refusal", err)
	}
	after, err := db.GetDownload(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if after.ImportPath != before.ImportPath || len(after.Handoff) != len(before.Handoff) || after.Handoff[0].Step != stepManualQueued || len(after.Handoff[0].Paths) != 1 || after.Handoff[0].Paths[0] != firstPath {
		t.Fatalf("rejected concurrent request replaced the durable selection: before=%+v after=%+v", before, after)
	}
	if got, err := os.ReadFile(firstPath); err != nil || string(got) != "first user-selected bytes" {
		t.Fatalf("active selection changed: %q err=%v", got, err)
	}
	if got, err := os.ReadFile(secondPath); err != nil || string(got) != "second user-selected bytes" {
		t.Fatalf("rejected selection changed: %q err=%v", got, err)
	}
}
