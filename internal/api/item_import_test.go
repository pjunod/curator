package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apigen "github.com/pjunod/monarr/internal/api/gen"
)

// Import file… on the item page: the one-level listing carries subtitle
// counts, and /import/plan names where a file goes without moving it.
func TestItemImportScanAndPlanHTTP(t *testing.T) {
	e := newAPIEnv(t)
	itemID := e.addMovie(t)
	dir := t.TempDir()
	movie := filepath.Join(dir, "Fight.Club.1999.1080p.WEB-DL.mkv")
	for path, body := range map[string]string{
		movie: "movie", filepath.Join(dir, "Fight.Club.1999.1080p.WEB-DL.en.srt"): "sub",
		filepath.Join(dir, "nested", "Other.mkv"): "other",
	} {
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var listed []apigen.ScannedFile
	e.get(t, "/api/v1/import/scan?depth=1&path="+url.QueryEscape(dir)).expect(t, http.StatusOK).into(t, &listed)
	if len(listed) != 1 || listed[0].Path != movie || listed[0].Subtitles == nil || *listed[0].Subtitles != 1 || listed[0].Publication != nil {
		t.Fatalf("listing = %+v, want only the movie, with its subtitle", listed)
	}
	body, _ := json.Marshal(map[string]any{"mediaItemId": itemID, "files": []map[string]any{{"path": movie}}})
	var plan []apigen.ImportPlanFile
	e.post(t, "/api/v1/import/plan", string(body)).expect(t, http.StatusOK).into(t, &plan)
	if len(plan) != 1 || plan[0].Error != nil || plan[0].Destination == nil || !strings.Contains(*plan[0].Destination, "Fight Club") ||
		plan[0].Action == nil || *plan[0].Action != "new" || plan[0].Subtitles != 1 {
		t.Fatalf("plan = %+v", plan)
	}
	if _, err := os.Stat(movie); err != nil {
		t.Fatal("planning moved the file")
	}
	e.post(t, "/api/v1/import/plan", fmt.Sprintf(`{"mediaItemId":%d,"files":[]}`, itemID)).expect(t, http.StatusBadRequest)
}
