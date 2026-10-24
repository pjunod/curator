package api

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	apigen "github.com/pjunod/monarr/internal/api/gen"
	"github.com/pjunod/monarr/internal/domain"
)

func TestReplacementHTTPAdmissionIsBoundToSelectedCopyScope(t *testing.T) {
	e := newAPIEnv(t)
	ctx := context.Background()
	itemID := e.addMovie(t)
	item, err := e.db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(item.Path, 0o700); err != nil {
		t.Fatal(err)
	}

	copyRoot := t.TempDir()
	root, err := e.lib.AddRootFolder(ctx, copyRoot, domain.RootKindOf(domain.KindMovie))
	if err != nil {
		t.Fatal(err)
	}
	var withCopy libDetail
	e.post(t, fmt.Sprintf("/api/v1/library/%d/copies", itemID),
		fmt.Sprintf(`{"qualityProfileId":1,"rootFolderId":%d,"name":"additional reviewed copy"}`, root.ID)).
		expect(t, http.StatusOK).into(t, &withCopy)
	if len(withCopy.Copies) != 1 || withCopy.Copies[0].ID == 0 || withCopy.Copies[0].Path == "" {
		t.Fatalf("HTTP copy creation did not return its independent storage identity: %+v", withCopy.Copies)
	}
	copy := withCopy.Copies[0]
	if err := os.MkdirAll(copy.Path, 0o700); err != nil {
		t.Fatal(err)
	}

	writeFile := func(path string, data []byte, copyID int64) (int64, string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		id, err := e.db.UpsertFile(ctx, itemID, copyID, path, int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		files, err := e.db.ListFilesForItem(ctx, itemID)
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			if file.ID == id && file.FileGeneration != "" {
				return id, file.FileGeneration
			}
		}
		t.Fatalf("file %d has no immutable generation", id)
		return 0, ""
	}
	primaryBytes := []byte("primary copy bytes remain owned by the primary scope")
	copyBytes := []byte("reviewed additional copy source")
	primaryID, primaryGeneration := writeFile(filepath.Join(item.Path, "primary.mkv"), primaryBytes, 0)
	copyFileID, copyGeneration := writeFile(filepath.Join(copy.Path, "additional.mkv"), copyBytes, copy.ID)

	observe := func(fileID int64, generation, requestID string) string {
		t.Helper()
		url := fmt.Sprintf("/api/v1/library/%d/files/%d/replacement-observations", itemID, fileID)
		var queued apigen.ReplacementObservation
		e.post(t, url, fmt.Sprintf(`{"fileGeneration":%q,"requestId":%q}`, generation, requestID)).
			expect(t, http.StatusAccepted).into(t, &queued)
		return queued.ObservationId
	}
	primaryObservation := observe(primaryID, primaryGeneration, "primary-copy-scope-observation")
	copyObservation := observe(copyFileID, copyGeneration, "additional-copy-scope-observation")
	workerService := e.acq
	workerCtx, cancel := context.WithCancel(ctx)
	workerService.StartImporters(workerCtx)
	t.Cleanup(func() {
		cancel()
		workerService.WaitImporters()
	})
	deadline := time.Now().Add(4 * time.Second)
	ready := map[string]bool{}
	for time.Now().Before(deadline) && len(ready) != 2 {
		for _, observationID := range []string{primaryObservation, copyObservation} {
			if ready[observationID] {
				continue
			}
			var observation apigen.ReplacementObservation
			e.get(t, "/api/v1/replacement-observations/"+observationID).expect(t, http.StatusOK).into(t, &observation)
			switch observation.State {
			case "ready":
				ready[observationID] = true
			case "stale", "failed":
				cancel()
				workerService.WaitImporters()
				t.Fatalf("copy-scope observation failed: %+v", observation)
			}
		}
		if len(ready) != 2 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	cancel()
	workerService.WaitImporters()
	if len(ready) != 2 {
		t.Fatal("both primary and additional-copy observations did not become ready")
	}

	operationURL := fmt.Sprintf("/api/v1/library/%d/replacement-operations", itemID)
	requestBody := func(requestID string, copyID int64, observed string) string {
		return fmt.Sprintf(`{"requestId":%q,"scope":"download","copyId":%d,"sourceFileId":%d,"intent":"replace_bad","blacklistSourceRelease":false,"reasonCode":"corrupt_media","reason":"replace only the selected copy source","observedFiles":[%s]}`,
			requestID, copyID, copyFileID, observed)
	}
	copyFileObservation := fmt.Sprintf(`{"id":%d,"fileGeneration":%q,"observationId":%q}`, copyFileID, copyGeneration, copyObservation)
	primaryFileObservation := fmt.Sprintf(`{"id":%d,"fileGeneration":%q,"observationId":%q}`, primaryID, primaryGeneration, primaryObservation)
	for _, rejected := range []struct {
		name string
		body string
	}{
		{name: "copy file submitted as primary", body: requestBody("copy-file-as-primary", 0, copyFileObservation)},
		{name: "mixed primary and copy observations", body: requestBody("mixed-copy-scope", copy.ID, copyFileObservation+","+primaryFileObservation)},
	} {
		t.Run(rejected.name, func(t *testing.T) {
			e.post(t, operationURL, rejected.body).expect(t, http.StatusConflict)
			var history apigen.LibraryLifecycleReview
			e.get(t, operationURL).expect(t, http.StatusOK).into(t, &history)
			if len(history.Operations) != 0 {
				t.Fatalf("rejected copy scope reserved a durable replacement operation: %+v", history.Operations)
			}
		})
	}

	var created apigen.ReplacementOperation
	e.post(t, operationURL, requestBody("valid-additional-copy-replacement", copy.ID, copyFileObservation)).
		expect(t, http.StatusAccepted).into(t, &created)
	if created.OperationId == "" || created.CopyId == nil || *created.CopyId != copy.ID || created.BlacklistSourceRelease {
		t.Fatalf("accepted HTTP operation did not retain selected copy/explicit blacklist choice: %+v", created)
	}
	persisted, err := e.db.GetReplacementOperation(ctx, created.OperationId)
	if err != nil || persisted.CopyID != copy.ID {
		t.Fatalf("SQLite replacement scope copyId=%d err=%v, want %d", persisted.CopyID, err, copy.ID)
	}
	journal, err := e.db.ListReplacementFiles(ctx, created.OperationId)
	if err != nil || len(journal) != 1 || journal[0].MediaFileID != copyFileID || journal[0].Disposition != "selected" {
		t.Fatalf("durable file review crossed copy scope: journal=%+v err=%v", journal, err)
	}
	primary, err := os.ReadFile(filepath.Join(item.Path, "primary.mkv"))
	if err != nil || string(primary) != string(primaryBytes) {
		t.Fatalf("copy-scoped HTTP request changed primary bytes: %q err=%v", primary, err)
	}
	selected, err := os.ReadFile(filepath.Join(copy.Path, "additional.mkv"))
	if err != nil || string(selected) != string(copyBytes) {
		t.Fatalf("queueing HTTP replacement changed reviewed bytes: %q err=%v", selected, err)
	}
}
