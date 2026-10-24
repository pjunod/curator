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
	"github.com/pjunod/monarr/internal/app/acquisition"
)

func TestReplacementObservationHTTPExposesStalePathAndAllowsFreshRetryAfterRepair(t *testing.T) {
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
	path := filepath.Join(item.Path, "observation-http-repair.mkv")
	target := filepath.Join(item.Path, "unselected-target.mkv")
	if err := os.WriteFile(path, []byte("selected bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("different target"), 0o600); err != nil {
		t.Fatal(err)
	}
	fileID, err := e.db.UpsertFile(ctx, itemID, 0, path, int64(len("selected bytes")))
	if err != nil {
		t.Fatal(err)
	}
	files, err := e.db.ListFilesForItem(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	var generation string
	for _, file := range files {
		if file.ID == fileID {
			generation = file.FileGeneration
		}
	}
	if generation == "" {
		t.Fatal("fixture file has no immutable generation")
	}

	postPath := fmt.Sprintf("/api/v1/library/%d/files/%d/replacement-observations", itemID, fileID)
	var queued apigen.ReplacementObservation
	e.post(t, postPath, fmt.Sprintf(`{"fileGeneration":%q,"requestId":"http-symlink-observation"}`, generation)).
		expect(t, http.StatusAccepted).into(t, &queued)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}

	originalService := e.acq
	workerCtx, cancel := context.WithCancel(ctx)
	originalService.StartImporters(workerCtx)
	t.Cleanup(func() { cancel(); originalService.WaitImporters() })
	deadline := time.Now().Add(4 * time.Second)
	var stale apigen.ReplacementObservation
	for time.Now().Before(deadline) {
		e.get(t, "/api/v1/replacement-observations/"+queued.ObservationId).
			expect(t, http.StatusOK).into(t, &stale)
		if stale.State == "stale" || stale.State == "failed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if stale.State != "stale" || stale.Error == nil || *stale.Error != "selected file path requires review" {
		t.Fatalf("HTTP observation did not expose its stale path-review reason: %+v", stale)
	}
	if stale.Sha256 != nil || stale.Size != nil {
		t.Fatalf("stale observation exposed ready-file evidence: %+v", stale)
	}
	persisted, err := e.db.GetReplacementFileObservation(ctx, queued.ObservationId)
	if err != nil || persisted.State != "stale" || persisted.Error != *stale.Error {
		t.Fatalf("HTTP stale result was not the durable worker outcome: %+v err=%v", persisted, err)
	}
	// Model a process restart: the retry uses the persisted DB record and a
	// fresh service worker, rather than relying on an in-process retry timer.
	cancel()
	originalService.WaitImporters()

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("repaired selected bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	var retried apigen.ReplacementObservation
	e.post(t, postPath, fmt.Sprintf(`{"fileGeneration":%q,"requestId":"http-repaired-observation"}`, generation)).
		expect(t, http.StatusAccepted).into(t, &retried)
	freshService := acquisition.New(e.db, e.bus, nil, nil, nil)
	e.acq = freshService
	e.srv.deps.Acquisition = freshService
	e.h = e.srv.Handler()
	retryCtx, cancelRetry := context.WithCancel(ctx)
	freshService.StartImporters(retryCtx)
	t.Cleanup(func() { cancelRetry(); freshService.WaitImporters() })
	deadline = time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		e.get(t, "/api/v1/replacement-observations/"+retried.ObservationId).
			expect(t, http.StatusOK).into(t, &retried)
		if retried.State == "ready" || retried.State == "stale" || retried.State == "failed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if retried.State != "ready" || retried.Error != nil || retried.Sha256 == nil || *retried.Sha256 == "" || retried.Size == nil || *retried.Size != int64(len("repaired selected bytes")) {
		t.Fatalf("fresh HTTP observation did not recover after the path was repaired: %+v", retried)
	}
	untouched, err := os.ReadFile(target)
	if err != nil || string(untouched) != "different target" {
		t.Fatalf("symlink target changed during stale observation and repair: contents=%q err=%v", untouched, err)
	}
}
