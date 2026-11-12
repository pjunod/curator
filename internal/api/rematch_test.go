package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apigen "github.com/pjunod/monarr/internal/api/gen"
)

// Re-match over HTTP: a preview of a title not in the library moves nothing
// and adds nothing; executing it adds the title through the normal add path,
// runs the move, and GET /rematch/{id} follows it to the end. A stale
// fingerprint is 409 stale; an unknown operation is 404.
func TestRematchHTTP(t *testing.T) {
	e := newAPIEnv(t)
	ctx := context.Background()
	itemID := e.addMovie(t)
	item, err := e.db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(item.Path, "Fight Club (1999) [Bluray-1080p].mkv")
	_ = os.MkdirAll(item.Path, 0o755)
	if err := os.WriteFile(path, []byte("movie"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.UpsertFile(ctx, itemID, 0, path, 5); err != nil {
		t.Fatal(err)
	}
	// The stub provider gives every title the same IMDb id.
	if _, err := e.db.W.ExecContext(ctx, `UPDATE media_items SET imdb_id='' WHERE id=?`, itemID); err != nil {
		t.Fatal(err)
	}
	body := `{"target":{"kind":"movie","tmdbId":551}}`
	var preview apigen.RematchPreview
	e.post(t, fmt.Sprintf("/api/v1/library/%d/rematch/preview", itemID), body).expect(t, http.StatusOK).into(t, &preview)
	if len(preview.Moves) != 1 || preview.Target.InLibrary || preview.Moves[0].To == nil || !strings.Contains(*preview.Moves[0].To, "{tmdb-551}") {
		t.Fatalf("preview = %+v", preview)
	}
	if id, _ := e.db.GetMediaItemByKindTmdb(ctx, "movie", 551); id != 0 {
		t.Fatal("the preview added the title")
	}

	stale, _ := json.Marshal(map[string]any{"target": map[string]any{"kind": "movie", "tmdbId": 551}, "requestId": "r-stale",
		"sourceRevision": preview.SourceRevision, "fingerprint": "not-the-preview"})
	e.post(t, fmt.Sprintf("/api/v1/library/%d/rematch", itemID), string(stale)).expect(t, http.StatusConflict)

	exec, _ := json.Marshal(map[string]any{"target": map[string]any{"kind": "movie", "tmdbId": 551}, "requestId": "r-1",
		"sourceRevision": preview.SourceRevision, "fingerprint": preview.Fingerprint})
	var op apigen.RematchOperation
	e.post(t, fmt.Sprintf("/api/v1/library/%d/rematch", itemID), string(exec)).expect(t, http.StatusAccepted).into(t, &op)
	deadline := time.Now().Add(10 * time.Second)
	for op.State != "complete" && op.State != "failed" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		e.get(t, "/api/v1/rematch/"+op.OperationId).expect(t, http.StatusOK).into(t, &op)
	}
	if op.State != "complete" {
		t.Fatalf("operation = %+v", op)
	}
	targetID, err := e.db.GetMediaItemByKindTmdb(ctx, "movie", 551)
	if err != nil || targetID == 0 {
		t.Fatal("executing did not add the title")
	}
	target, _ := e.db.GetMediaItemFull(ctx, targetID)
	if len(target.Files) != 1 || target.Files[0].Path != *preview.Moves[0].To {
		t.Fatalf("target files = %+v", target.Files)
	}
	e.get(t, "/api/v1/rematch/rm-missing").expect(t, http.StatusNotFound)
	e.post(t, "/api/v1/rematch/rm-missing/cancel", "").expect(t, http.StatusNotFound)
	e.post(t, "/api/v1/rematch/"+op.OperationId+"/cancel", "").expect(t, http.StatusConflict)
}
