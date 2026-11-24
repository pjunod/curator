package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/pjunod/monarr/internal/app/acquisition"
	"github.com/pjunod/monarr/internal/ports"
)

func TestStorageObservationsReturnCachedRootMetadataAndRecordedCleanup(t *testing.T) {
	e := newAPIEnv(t)
	ctx := context.Background()
	const cur = "/not-mounted/cache-evidence"
	if err := e.db.SaveCompletedScan(ctx, cur, `{
		"path":"/not-mounted/cache-evidence","checkedAt":"2026-11-18T16:00:00Z","freeBytes":0,"totalBytes":300,
		"entries":[{"path":"/inventory-file-must-not-leave-cache"}]
	}`); err != nil {
		t.Fatal(err)
	}
	id, err := e.db.AddDownloadClient(ctx, ports.ClientConfig{
		Type: "nzbd", Name: "Source Runner", URL: "http://runner.invalid", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.db.SetMeta(ctx, "runner_storage_observations", `{"`+strconv.FormatInt(id, 10)+`":[
		{"owner":"Runner","role":"processing","name":"Source Runner","path":"/runner/source","status":"available",
		"observedAt":"2026-11-18T16:00:00Z","freeBytes":100,"totalBytes":250}
	]}`); err != nil {
		t.Fatal(err)
	}
	item := e.addMovie(t)
	if err := e.db.AddHistory(ctx, acquisition.HistoryPayloadRemoved, item, "Manifest cleanup", map[string]any{
		"files": 2, "bytes": 80, "by": "per-file cleanup",
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.db.AddHistory(ctx, acquisition.HistoryPayloadRemoved, item, "Legacy release size", map[string]any{
		"bytes": 900,
	}); err != nil {
		t.Fatal(err)
	}
	response := e.get(t, "/api/v1/system/storage-observations").expect(t, http.StatusOK)
	if strings.Contains(response.Body.String(), "inventory-file-must-not-leave-cache") {
		t.Fatalf("root observations exposed inventory files: %s", response.Body.String())
	}
	var observed acquisition.StorageObservations
	response.into(t, &observed)
	if len(observed.Roots) != 2 {
		t.Fatalf("expected Curator and Runner observations: %+v", observed)
	}
	local, runner := observed.Roots[0], observed.Roots[1]
	if local.Owner != "Curator" || local.Path != cur || local.ObservedAt == nil || local.FreeBytes == nil || *local.FreeBytes != 0 || local.TotalBytes == nil || *local.TotalBytes != 300 {
		t.Fatalf("missing cached Curator measurement: %+v", local)
	}
	if runner.Owner != "Runner" || runner.Role != "processing" || runner.ObservedAt == nil || runner.FreeBytes == nil || *runner.FreeBytes != 100 {
		t.Fatalf("missing cached Runner measurement: %+v", runner)
	}
	if observed.Cleanup.LogicalBytesRemoved == nil || *observed.Cleanup.LogicalBytesRemoved != 80 ||
		observed.Cleanup.FilesRemoved == nil || *observed.Cleanup.FilesRemoved != 2 || observed.Cleanup.LastRemovalAt == nil {
		t.Fatalf("cleanup totals must count observed files without inferring space from legacy release sizes: %+v", observed.Cleanup)
	}
}
