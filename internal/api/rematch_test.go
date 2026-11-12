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
	"github.com/pjunod/monarr/internal/ports"
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
	// The title added for the refused move does not stay behind.
	if id, _ := e.db.GetMediaItemByKindTmdb(ctx, "movie", 551); id != 0 {
		t.Fatal("a refused move left the title it added in the library")
	}
	e.get(t, fmt.Sprintf("/api/v1/library/%d/rematch", itemID)).expect(t, http.StatusNoContent)

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
	if _, err := e.db.W.ExecContext(ctx, `INSERT INTO rematch_operations(operation_id,request_id,fingerprint,state,source_item_id,target_item_id,created_at,updated_at) VALUES('rm-open','r-open','f','moving',?,?,1,1)`, itemID, targetID); err != nil {
		t.Fatal(err)
	}
	var open apigen.RematchOperation
	e.get(t, fmt.Sprintf("/api/v1/library/%d/rematch", targetID)).expect(t, http.StatusOK).into(t, &open)
	if open.OperationId != "rm-open" {
		t.Fatalf("open operation = %+v", open)
	}
	e.get(t, "/api/v1/rematch/rm-missing").expect(t, http.StatusNotFound)
	e.post(t, "/api/v1/rematch/rm-missing/cancel", "").expect(t, http.StatusNotFound)
	e.post(t, "/api/v1/rematch/"+op.OperationId+"/cancel", "").expect(t, http.StatusConflict)
}

// The Dev tab's advisory reads each notifier's stored features and when.
func TestNotifierFeaturesHTTP(t *testing.T) {
	e := newAPIEnv(t)
	ctx := context.Background()
	id, err := e.db.AddNotifier(ctx, ports.NotifierConfig{Type: "plurx", Name: "cinema", Enabled: true, Settings: map[string]string{"url": "http://c", "apiKey": "k"}})
	if err != nil {
		t.Fatal(err)
	}
	var list []apigen.Notifier
	e.get(t, "/api/v1/notifiers").expect(t, http.StatusOK).into(t, &list)
	if len(list) != 1 || list[0].FeaturesReadAt != nil {
		t.Fatalf("notifiers = %+v, want features never read", list)
	}
	if err := e.db.SetNotifierFeatures(ctx, id, []string{"removed_paths"}); err != nil {
		t.Fatal(err)
	}
	e.get(t, "/api/v1/notifiers").expect(t, http.StatusOK).into(t, &list)
	if list[0].Features == nil || len(*list[0].Features) != 1 || list[0].FeaturesReadAt == nil || *list[0].FeaturesReadAt == 0 {
		t.Fatalf("notifiers = %+v, want removed_paths and a read time", list)
	}
}
