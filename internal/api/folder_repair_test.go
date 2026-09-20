package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestRepairLibraryFolder(t *testing.T) {
	e := newAPIEnv(t)
	var roots []libRoot
	e.get(t, "/api/v1/rootfolders").expect(t, http.StatusOK).into(t, &roots)
	rootID := strconv.FormatInt(roots[0].ID, 10)
	var item libDetail
	e.post(t, "/api/v1/library", `{"kind":"movie","tmdbId":550,"rootFolderId":`+rootID+`}`).expect(t, http.StatusCreated).into(t, &item)
	libDir(t, item.Path)
	libVideo(t, item.Path, "movie.mkv")
	libScan(t, e)
	moved := filepath.Join(e.root, "moved")
	if err := os.Rename(item.Path, moved); err != nil {
		t.Fatal(err)
	}
	libScan(t, e)
	url := "/api/v1/library/" + strconv.FormatInt(item.ID, 10) + "/repair-folder"
	body := func(path string) string {
		raw, err := json.Marshal(map[string]string{"expectedPath": item.Path, "path": path})
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	e.post(t, url, "{").expect(t, http.StatusBadRequest)
	e.post(t, url, body(e.root)).expect(t, http.StatusBadRequest)
	e.post(t, "/api/v1/library/999999/repair-folder", body(moved)).expect(t, http.StatusNotFound)

	other := libManualSeries(t, e, "Other title")
	alias := filepath.Join(e.root, "alias")
	if err := os.Symlink(other.Path, alias); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{other.Path, alias, libDir(t, other.Path, "Season 1"), t.TempDir()} {
		e.post(t, url, body(invalid)).expect(t, http.StatusBadRequest)
	}
	e.post(t, url, body(moved)).expect(t, http.StatusNoContent)
	var fixed libDetail
	e.get(t, "/api/v1/library/"+strconv.FormatInt(item.ID, 10)).expect(t, http.StatusOK).into(t, &fixed)
	resolved := moved
	if fixed.Path != resolved || len(fixed.Files) != 1 || fixed.Files[0].Path != filepath.Join(resolved, "movie.mkv") {
		t.Fatalf("repair did not persist location and files: %+v", fixed)
	}
	var report struct {
		MissingPaths []string `json:"missingPaths"`
	}
	e.get(t, "/api/v1/library/scan/report").expect(t, http.StatusOK).into(t, &report)
	if len(report.MissingPaths) != 0 {
		t.Fatalf("repaired folder still reported: %+v", report)
	}
}
