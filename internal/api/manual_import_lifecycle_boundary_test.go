package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/pjunod/monarr/internal/app/acquisition"
)

func TestManualImportHTTPBindsLegacyConsentToCurrentFileSnapshot(t *testing.T) {
	e := newAPIEnv(t)
	ctx := context.Background()
	itemID := e.addMovie(t)
	root := t.TempDir()
	if err := e.acq.SetCompletedRoots(ctx, root); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(root, "legacy-http-review")
	selectedPath := filepath.Join(payload, "Movie.2026.1080p.WEB-DL.mkv")
	siblingPath := filepath.Join(payload, "Movie.2026.1080p.WEB-DL.notes")
	for path, body := range map[string]string{selectedPath: "reviewed source", siblingPath: "untouched sibling"} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.db.W.ExecContext(ctx, `INSERT INTO legacy_cleanup_review(generation_key,path) VALUES('',?)`, payload); err != nil {
		t.Fatal(err)
	}
	if err := e.acq.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	entry := apiCompletedEntryForPath(t, e, payload)
	selection := apiCompletedSelectionForPath(t, &entry, selectedPath)
	before, err := e.db.ListRecentDownloads(ctx)
	if err != nil {
		t.Fatal(err)
	}

	for _, body := range []string{`{`, `{"path":"` + payload + `"}`, `{"path":"` + payload + `","mediaItemId":0}`} {
		e.post(t, "/api/v1/import/manual", body).expect(t, http.StatusBadRequest)
	}
	if got, err := e.db.ListRecentDownloads(ctx); err != nil || len(got) != len(before) {
		t.Fatalf("invalid manual-import requests created Activity rows: before=%d after=%d err=%v", len(before), len(got), err)
	}

	// Replace bytes at the same selected path after the read-only inventory.
	// The HTTP admission must reject the stale approval instead of silently
	// binding the old consent to the replacement file.
	if err := os.WriteFile(selectedPath, []byte("changed source!"), 0o600); err != nil {
		t.Fatal(err)
	}
	staleBody := manualImportReviewJSON(t, payload, itemID, entry.Fingerprint, selection)
	e.post(t, "/api/v1/import/manual", staleBody).expect(t, http.StatusBadRequest)
	if got, err := e.db.ListRecentDownloads(ctx); err != nil || len(got) != len(before) {
		t.Fatalf("stale reviewed selection reserved an Activity row: before=%d after=%d err=%v", len(before), len(got), err)
	}
	if got, err := os.ReadFile(selectedPath); err != nil || string(got) != "changed source!" {
		t.Fatalf("stale HTTP approval changed replacement bytes: %q err=%v", got, err)
	}

	if err := e.acq.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	entry = apiCompletedEntryForPath(t, e, payload)
	selection = apiCompletedSelectionForPath(t, &entry, selectedPath)
	var accepted struct {
		DownloadID int64 `json:"downloadId"`
	}
	e.post(t, "/api/v1/import/manual", manualImportReviewJSON(t, payload, itemID, entry.Fingerprint, selection)).
		expect(t, http.StatusAccepted).into(t, &accepted)
	if accepted.DownloadID <= 0 {
		t.Fatalf("accepted HTTP import has no durable Activity row: %+v", accepted)
	}
	dl, err := e.db.GetDownload(ctx, accepted.DownloadID)
	if err != nil || dl.MediaItemID != itemID || dl.ImportPath != payload || len(dl.Handoff) == 0 {
		t.Fatalf("accepted HTTP review did not persist selected target and handoff: row=%+v err=%v", dl, err)
	}
	if got, err := os.ReadFile(siblingPath); err != nil || string(got) != "untouched sibling" {
		t.Fatalf("manual admission changed an unselected sibling: %q err=%v", got, err)
	}
}

func TestCompletedHTTPSelectedLegacyDeletionUsesReviewedSnapshot(t *testing.T) {
	e := newAPIEnv(t)
	ctx := context.Background()
	root := t.TempDir()
	if err := e.acq.SetCompletedRoots(ctx, root); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(root, "legacy-http-delete")
	selectedPath := filepath.Join(payload, "selected.mkv")
	keepPath := filepath.Join(payload, "keep.mkv")
	for path, body := range map[string]string{selectedPath: "selected legacy bytes", keepPath: "protected sibling"} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.db.W.ExecContext(ctx, `INSERT INTO legacy_cleanup_review(generation_key,path) VALUES('',?)`, payload); err != nil {
		t.Fatal(err)
	}
	if err := e.acq.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	entry := apiCompletedEntryForPath(t, e, payload)
	selection := apiCompletedSelectionForPath(t, &entry, selectedPath)
	body, err := json.Marshal(map[string]any{
		"path": entry.Path, "fingerprint": entry.Fingerprint, "reviewLegacy": true,
		"files": []map[string]string{{"path": selection.Path, "snapshotIdentity": selection.SnapshotIdentity}},
	})
	if err != nil {
		t.Fatal(err)
	}
	(&httptestRecorder{do(t, e.h, http.MethodDelete, "/api/v1/downloads/inventory/entry", string(body))}).expect(t, http.StatusNoContent)
	if _, err := os.Stat(selectedPath); !os.IsNotExist(err) {
		t.Fatalf("HTTP selected deletion left selected file: %v", err)
	}
	if got, err := os.ReadFile(keepPath); err != nil || string(got) != "protected sibling" {
		t.Fatalf("HTTP selected deletion changed unselected legacy sibling: %q err=%v", got, err)
	}
}

func TestCompletedHTTPRejectsStaleSelectedLegacyFingerprint(t *testing.T) {
	e := newAPIEnv(t)
	ctx := context.Background()
	root := t.TempDir()
	if err := e.acq.SetCompletedRoots(ctx, root); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(root, "legacy-http-stale")
	path := filepath.Join(payload, "selected.mkv")
	if err := os.MkdirAll(payload, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("reviewed bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.W.ExecContext(ctx, `INSERT INTO legacy_cleanup_review(generation_key,path) VALUES('',?)`, payload); err != nil {
		t.Fatal(err)
	}
	if err := e.acq.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	entry := apiCompletedEntryForPath(t, e, payload)
	selection := apiCompletedSelectionForPath(t, &entry, path)
	if err := os.WriteFile(path, []byte("replacement bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{
		"path": entry.Path, "fingerprint": entry.Fingerprint, "reviewLegacy": true,
		"files": []map[string]string{{"path": selection.Path, "snapshotIdentity": selection.SnapshotIdentity}},
	})
	if err != nil {
		t.Fatal(err)
	}
	(&httptestRecorder{do(t, e.h, http.MethodDelete, "/api/v1/downloads/inventory/entry", string(body))}).expect(t, http.StatusConflict)
	if got, err := os.ReadFile(path); err != nil || string(got) != "replacement bytes" {
		t.Fatalf("stale HTTP selection changed replacement bytes: %q err=%v", got, err)
	}
}

func manualImportReviewJSON(t *testing.T, payload string, itemID int64, fingerprint string, selection acquisition.CompletedFileSelection) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"path": payload, "paths": []string{selection.Path}, "mediaItemId": itemID,
		"reviewLegacy": true, "reviewLegacyFingerprint": fingerprint,
		"reviewLegacyFiles": []map[string]string{{"path": selection.Path, "snapshotIdentity": selection.SnapshotIdentity}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func apiCompletedEntryForPath(t *testing.T, e *apiEnv, path string) acquisition.CompletedEntry {
	t.Helper()
	inventory, err := e.acq.CompletedInventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range inventory.Roots {
		for _, entry := range root.Entries {
			if canonicalAPICompletedPath(entry.Path) == canonicalAPICompletedPath(path) {
				return entry
			}
		}
	}
	t.Fatalf("completed entry %q missing from inventory: %+v", path, inventory)
	return acquisition.CompletedEntry{}
}

func apiCompletedSelectionForPath(t *testing.T, entry *acquisition.CompletedEntry, path string) acquisition.CompletedFileSelection {
	t.Helper()
	for _, file := range entry.ReviewFiles {
		if canonicalAPICompletedPath(file.Path) == canonicalAPICompletedPath(path) {
			return acquisition.CompletedFileSelection{Path: file.Path, SnapshotIdentity: file.SnapshotIdentity}
		}
	}
	t.Fatalf("reviewed file %q missing from entry %+v", path, entry)
	return acquisition.CompletedFileSelection{}
}

func canonicalAPICompletedPath(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(path)
}
