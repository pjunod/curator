package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	apigen "github.com/pjunod/monarr/internal/api/gen"
	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

func TestActivityImportPreviewDistinguishesUnownedAndAlreadyPlacedFiles(t *testing.T) {
	e := newAPIEnv(t)
	ctx := t.Context()
	item := e.addMovie(t)
	another, err := e.db.CreateMediaItem(ctx, domain.MediaItem{
		Title: "Original publication title", SortTitle: "original publication title", Kind: domain.KindMovie,
		Path: filepath.Join(e.root, "Original publication title"),
	})
	if err != nil {
		t.Fatal(err)
	}
	saved := insertActivityApprovalJob(t, e.db, another, "Runner delivery already used for its own title")
	dir := t.TempDir()
	historicalDir := t.TempDir()
	write := func(parent, name string) string {
		t.Helper()
		path := filepath.Join(parent, name)
		if err := os.WriteFile(path, []byte("recorded "+name), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	placed := write(dir, "already-placed.mkv")
	mismatched := write(dir, "still-pending.mkv")
	unlisted := write(dir, "unlisted.mkv")
	historical := write(historicalDir, "without-activity.mkv")
	wrongKind := write(t.TempDir(), "not-a-movie.epub")
	link := filepath.Join(dir, "link.mkv")
	if err := os.Symlink(wrongKind, link); err != nil {
		t.Fatal(err)
	}
	current, err := e.db.InsertPublication(ctx, sqlite.PublicationWrite{
		DownloadID: saved.ID, Kind: "completed", Path: dir, Generation: "still-pending", Cohort: "live",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.InsertPublication(ctx, sqlite.PublicationWrite{
		Kind: "completed", Path: historicalDir, Generation: "prior-without-a-job", Cohort: "historical",
	}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []sqlite.PublicationFile{
		{PublicationID: current, Path: filepath.Base(placed), Bytes: 18, Outcome: "placed", LibraryPath: filepath.Join(e.root, "already-placed.mkv")},
		{PublicationID: current, Path: filepath.Base(mismatched), Bytes: 20, Outcome: "pending"},
	} {
		if err := e.db.SeedPublicationFiles(ctx, current, []sqlite.PublicationFile{f}); err != nil {
			t.Fatal(err)
		}
		if err := e.db.SetPublicationFileOutcome(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	var scanned []apigen.ScannedFile
	e.get(t, "/api/v1/import/scan?depth=1&path="+url.QueryEscape(dir)).expect(t, http.StatusOK).into(t, &scanned)
	if !slices.ContainsFunc(scanned, func(f apigen.ScannedFile) bool {
		return f.Path == mismatched && f.Publication != nil && f.Publication.PublicationId == current &&
			f.Publication.DownloadId == saved.ID && f.Publication.Outcome == "pending"
	}) || !slices.ContainsFunc(scanned, func(f apigen.ScannedFile) bool {
		return f.Path == link && f.Skipped != nil && strings.Contains(*f.Skipped, "symlink")
	}) {
		t.Fatalf("scanner did not distinguish placement evidence and symlink refusal: %+v", scanned)
	}

	cases := []struct{ path, reason string }{
		{placed, "already recorded this file as placed"},
		{mismatched, "already placed"},
		{unlisted, "manifest for this delivery does not name this file"},
		{historical, "has no activity row"},
		{link, "not a regular file"},
		{wrongKind, "not media of this item's kind"},
		{filepath.Join(dir, "missing.mkv"), "path not accessible"},
	}
	files := make([]map[string]string, 0, len(cases))
	for _, c := range cases {
		files = append(files, map[string]string{"path": c.path})
	}
	body, err := json.Marshal(map[string]any{"mediaItemId": item, "files": files})
	if err != nil {
		t.Fatal(err)
	}
	var plan []apigen.ImportPlanFile
	e.post(t, "/api/v1/import/plan", string(body)).expect(t, http.StatusOK).into(t, &plan)
	if len(plan) != len(cases) {
		t.Fatalf("preview omitted selected files: %+v", plan)
	}
	for i, c := range cases {
		got := plan[i]
		if got.Path != c.path || got.Error == nil || !strings.Contains(strings.ToLower(*got.Error), c.reason) ||
			got.Destination != nil || got.Mode != nil {
			t.Errorf("unsafe file preview %q has unexpected result: %+v, want %q", c.path, got, c.reason)
		}
	}
	if _, err := os.Stat(placed); err != nil {
		t.Fatalf("the plan touched bytes of the original delivery: %v", err)
	}
	if count, err := e.db.GetMediaItemFull(ctx, item); err != nil || len(count.Files) != 0 {
		t.Fatalf("preview wrote Library media instead of describing the selection: %+v err=%v", count.Files, err)
	}
}

func TestActivityImportPreviewRefusesRemovalFromTheLibrary(t *testing.T) {
	e := newAPIEnv(t)
	ctx := t.Context()
	item := e.addMovie(t)
	media, err := e.db.GetMediaItemFull(ctx, item)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(media.Path, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(media.Path, "Fight.Club.1999.1080p.mkv")
	if err := os.WriteFile(source, []byte("already in the Library"), 0o600); err != nil {
		t.Fatal(err)
	}
	var lines []apigen.ImportPlanFile
	input := fmt.Sprintf(`{"mediaItemId":%d,"removeOriginal":true,"files":[{"path":%q}]}`, item, source)
	e.post(t, "/api/v1/import/plan", input).expect(t, http.StatusOK).into(t, &lines)
	if len(lines) != 1 || lines[0].Error == nil || !strings.Contains(*lines[0].Error, "Remove original is refused") || lines[0].Mode != nil {
		t.Fatalf("preview offered to move and remove an original inside the Library: %+v", lines)
	}
	if got, err := os.ReadFile(source); err != nil || string(got) != "already in the Library" {
		t.Fatalf("preview changed the existing file: %q err=%v", got, err)
	}
}
