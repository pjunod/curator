package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestQueuedManualBookImportPersistsBookSelectionForWorker(t *testing.T) {
	ctx := context.Background()
	svc, db, bookID := bookSetup(t, nil, &fakeClient{})
	svc.importCh = make(chan importJob, 1)
	svc.importCtx = ctx
	svc.importJobs = make(map[int64]*importTicket)

	payload := t.TempDir()
	bookPath := filepath.Join(payload, "Project Hail Mary - Andy Weir.epub")
	bookBytes := []byte("queued book payload")
	artPath := filepath.Join(payload, "cover.jpg")
	artBytes := []byte("unselected artwork")
	videoPath := filepath.Join(payload, "unrelated.mkv")
	videoBytes := []byte("not a book")
	if err := os.WriteFile(bookPath, bookBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artPath, artBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(videoPath, videoBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	id, err := svc.QueueManualImport(ctx, ManualImportRequest{Path: payload, MediaItemID: bookID})
	if err != nil {
		t.Fatalf("queue manual book import: %v", err)
	}
	var job importJob
	select {
	case job = <-svc.importCh:
	default:
		t.Fatal("manual book import was not dispatched to the worker queue")
	}
	if job.dl.ID != id || job.dl.Protocol != "manual" || job.dl.MediaItemID != bookID {
		t.Fatalf("queued book job lost its durable target: %+v", job.dl)
	}
	if len(job.dl.Handoff) == 0 || len(job.dl.Handoff[len(job.dl.Handoff)-1].Paths) != 1 || job.dl.Handoff[len(job.dl.Handoff)-1].Paths[0] != bookPath {
		t.Fatalf("queued book selection included non-book siblings: handoff=%+v", job.dl.Handoff)
	}
	svc.runImportTracked(job.ctx, job)
	files, err := db.ListFilesForItem(ctx, bookID)
	if err != nil || len(files) != 1 || files[0].Path == "" {
		t.Fatalf("worker did not commit the selected book: files=%+v err=%v", files, err)
	}
	if canonicalCompletedPath(files[0].Path) == canonicalCompletedPath(bookPath) {
		t.Fatalf("worker indexed the source path instead of placing a distinct library copy: %+v", files[0])
	}
	if got, err := os.ReadFile(files[0].Path); err != nil || string(got) != string(bookBytes) {
		t.Fatalf("worker changed queued book bytes: %q err=%v", got, err)
	}
	if got, err := os.ReadFile(artPath); err != nil || string(got) != string(artBytes) {
		t.Fatalf("unselected artwork bytes changed during the book import: %q err=%v", got, err)
	}
	if got, err := os.ReadFile(videoPath); err != nil || string(got) != string(videoBytes) {
		t.Fatalf("non-book sibling bytes changed during the book import: %q err=%v", got, err)
	}
}

func TestQueueManualImportRejectsFileGenerationChangedWhileWaitingForAdmission(t *testing.T) {
	ctx := context.Background()
	svc, db, itemID := setup(t, nil, &fakeClient{})
	svc.importCh = make(chan importJob, 1)
	svc.importCtx = ctx
	svc.importJobs = make(map[int64]*importTicket)
	payload := t.TempDir()
	selected := filepath.Join(payload, "Test.Show.S01E01.1080p.WEB-DL.mkv")
	originalBytes := []byte("original selected episode")
	if err := os.WriteFile(selected, originalBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	originalInfo, err := os.Lstat(selected)
	if err != nil {
		t.Fatal(err)
	}
	originalGeneration := deliveryFileGeneration(originalInfo)
	before, err := db.ListRecentDownloads(ctx)
	if err != nil {
		t.Fatal(err)
	}

	svc.importTargetMu.Lock()
	locked := true
	defer func() {
		if locked {
			svc.importTargetMu.Unlock()
		}
	}()
	type admissionResult struct {
		id  int64
		err error
	}
	result := make(chan admissionResult, 1)
	go func() {
		id, err := svc.QueueManualImport(ctx, ManualImportRequest{Path: payload, Paths: []string{selected}, MediaItemID: itemID})
		result <- admissionResult{id: id, err: err}
	}()
	if !waitForQueueManualImportAdmissionLockWait() {
		svc.importTargetMu.Unlock()
		locked = false
		select {
		case <-result:
		case <-time.After(3 * time.Second):
			t.Fatal("manual import admission did not finish after releasing the storage lock")
		}
		t.Fatal("QueueManualImport did not reach the serialized admission section")
	}

	held := selected + ".reviewed-original"
	if err := os.Rename(selected, held); err != nil {
		svc.importTargetMu.Unlock()
		locked = false
		t.Fatal(err)
	}
	replacementBytes := []byte("replacement selected episode")
	if err := os.WriteFile(selected, replacementBytes, 0o644); err != nil {
		svc.importTargetMu.Unlock()
		locked = false
		t.Fatal(err)
	}
	if err := os.Chtimes(selected, time.Now().Add(2*time.Second), time.Now().Add(2*time.Second)); err != nil {
		svc.importTargetMu.Unlock()
		locked = false
		t.Fatal(err)
	}
	newInfo, err := os.Lstat(selected)
	if err != nil || deliveryFileGeneration(newInfo) == originalGeneration {
		svc.importTargetMu.Unlock()
		locked = false
		t.Fatalf("fixture did not create a new selected-file generation: old=%q new=%q err=%v", originalGeneration, deliveryFileGeneration(newInfo), err)
	}
	svc.importTargetMu.Unlock()
	locked = false
	var admitted admissionResult
	select {
	case admitted = <-result:
	case <-time.After(3 * time.Second):
		t.Fatal("manual import admission did not return after releasing the storage lock")
	}
	if admitted.err == nil || !strings.Contains(admitted.err.Error(), "selected file changed before import admission") {
		t.Fatalf("manual import admitted a different file generation: id=%d err=%v", admitted.id, admitted.err)
	}
	if got, err := os.ReadFile(held); err != nil || string(got) != string(originalBytes) {
		t.Fatalf("original selected bytes changed during refused admission: %q err=%v", got, err)
	}
	if got, err := os.ReadFile(selected); err != nil || string(got) != string(replacementBytes) {
		t.Fatalf("replacement selected bytes changed during refused admission: %q err=%v", got, err)
	}
	after, err := db.ListRecentDownloads(ctx)
	if err != nil || len(after) != len(before) || len(svc.importCh) != 0 {
		t.Fatalf("stale generation left a partial admission: before=%d after=%+v queued=%d err=%v", len(before), after, len(svc.importCh), err)
	}

	id, err := svc.QueueManualImport(ctx, ManualImportRequest{Path: payload, Paths: []string{selected}, MediaItemID: itemID})
	if err != nil {
		t.Fatalf("current generation could not be admitted after the refusal: %v", err)
	}
	var job importJob
	select {
	case job = <-svc.importCh:
	default:
		t.Fatal("recovered manual admission did not enter the worker queue")
	}
	if job.dl.ID != id || len(job.dl.Handoff) == 0 || len(job.dl.Handoff[len(job.dl.Handoff)-1].Paths) != 1 || job.dl.Handoff[len(job.dl.Handoff)-1].Paths[0] != selected {
		t.Fatalf("retried admission did not persist the exact current selection: %+v", job.dl)
	}
}

func waitForQueueManualImportAdmissionLockWait() bool {
	deadline := time.Now().Add(3 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	stack := make([]byte, 1<<20)
	for time.Now().Before(deadline) {
		n := runtime.Stack(stack, true)
		for _, goroutine := range strings.Split(string(stack[:n]), "\n\n") {
			if strings.Contains(goroutine, "QueueManualImport") && strings.Contains(goroutine, "sync.(*Mutex).Lock") {
				return true
			}
		}
		<-ticker.C
	}
	return false
}
