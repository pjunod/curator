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
)

// movieOnDisk adds a movie with one file under its folder and returns the id,
// the folder and the file's path.
func (e *apiEnv) movieOnDisk(t *testing.T) (int64, string) {
	t.Helper()
	ctx := context.Background()
	id := e.addMovie(t)
	item, err := e.db.GetMediaItemFull(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(item.Path, "Fight Club (1999) [Bluray-1080p].mkv")
	if err := os.MkdirAll(item.Path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("movie"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.UpsertFile(ctx, id, 0, path, 5); err != nil {
		t.Fatal(err)
	}
	return id, path
}

// Offload over HTTP: preview changes nothing; a stale fingerprint is 409
// stale; executing deletes the file and keeps the title; the monitored toggle
// is then refused with 409 offloaded; Re-acquire answers searching and clears it.
func TestOffloadHTTP(t *testing.T) {
	e := newAPIEnv(t)
	id, path := e.movieOnDisk(t)
	base := fmt.Sprintf("/api/v1/library/%d", id)

	var preview apigen.OffloadPreview
	e.post(t, base+"/offload/preview", `{"scope":"item"}`).expect(t, http.StatusOK).into(t, &preview)
	if len(preview.Files) != 1 || preview.Bytes != 5 || preview.Fingerprint == "" {
		t.Fatalf("preview = %+v", preview)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the preview deleted something: %v", err)
	}
	e.post(t, base+"/offload", `{"scope":"item","requestId":"r0","fingerprint":"nope"}`).expect(t, http.StatusConflict)
	e.post(t, base+"/offload", `{"scope":"item"}`).expect(t, http.StatusBadRequest)
	e.get(t, base+"/offload").expect(t, http.StatusNoContent)

	var op apigen.OffloadOperation
	body := fmt.Sprintf(`{"scope":"item","requestId":"r1","fingerprint":%q}`, preview.Fingerprint)
	e.post(t, base+"/offload", body).expect(t, http.StatusAccepted).into(t, &op)
	// Retrying the same request answers the same operation.
	var again apigen.OffloadOperation
	e.post(t, base+"/offload", body).expect(t, http.StatusAccepted).into(t, &again)
	if again.OperationId != op.OperationId {
		t.Fatalf("a retried request started a second operation: %s vs %s", again.OperationId, op.OperationId)
	}
	deadline := time.Now().Add(10 * time.Second)
	for op.State != "complete" && op.State != "failed" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		e.get(t, "/api/v1/offload/"+op.OperationId).expect(t, http.StatusOK).into(t, &op)
	}
	if op.State != "complete" || op.FilesDone != 1 || op.BytesFreed != 5 {
		t.Fatalf("operation = %+v", op)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the file survived: %v", err)
	}

	var detail apigen.MediaItemDetail
	e.get(t, base).expect(t, http.StatusOK).into(t, &detail)
	if detail.Offloaded == nil || !*detail.Offloaded {
		t.Fatalf("the title is not marked offloaded: %+v", detail.Offloaded)
	}
	var list []apigen.MediaItemSummary
	e.get(t, "/api/v1/library").expect(t, http.StatusOK).into(t, &list)
	if len(list) != 1 || list[0].Offloaded == nil || !*list[0].Offloaded {
		t.Fatalf("library list = %+v", list)
	}
	e.patch(t, base, `{"monitored":true}`).expect(t, http.StatusConflict)

	var done apigen.OffloadOperation
	e.get(t, base+"/offload").expect(t, http.StatusOK).into(t, &done)
	if done.OperationId != op.OperationId {
		t.Fatalf("finished operation not offered to the page: %+v", done)
	}
	e.post(t, "/api/v1/offload/"+op.OperationId+"/dismiss", "").expect(t, http.StatusNoContent)
	e.get(t, base+"/offload").expect(t, http.StatusNoContent)
	e.get(t, "/api/v1/offload/of-missing").expect(t, http.StatusNotFound)
	e.post(t, "/api/v1/offload/of-missing/cancel", "").expect(t, http.StatusNotFound)

	var res apigen.ReacquireResult
	e.post(t, base+"/reacquire", `{"scope":"item"}`).expect(t, http.StatusOK).into(t, &res)
	if res.State != "searching" || !res.Cleared {
		t.Fatalf("reacquire = %+v", res)
	}
	detail = apigen.MediaItemDetail{}
	e.get(t, base).expect(t, http.StatusOK).into(t, &detail)
	if detail.Offloaded != nil {
		t.Fatal("still offloaded after Re-acquire")
	}
}

// The webhook's reacquire event: ids only, 400 without ids or with an unknown
// kind or a missing season/episode, matched:false/not_found for a title Monarr
// does not have, and idempotent: the second ask answers where it stands.
func TestPlurxWebhookReacquire(t *testing.T) {
	e := newAPIEnv(t)
	ctx := context.Background()
	id, path := e.movieOnDisk(t)
	post := func(body string) (int, map[string]any) {
		rr := e.post(t, "/api/v1/webhooks/plurx", body)
		var out map[string]any
		rr.into(t, &out)
		return rr.Code, out
	}
	// Not offloaded yet.
	if code, out := post(`{"event":"reacquire","kind":"movie","tmdb":550,"user":"paul"}`); code != 200 || out["matched"] != true || out["state"] != "on_disk" && out["state"] != "not_offloaded" {
		t.Fatalf("not offloaded: %d %v", code, out)
	}
	// Offload it, then read the state.
	var preview apigen.OffloadPreview
	e.post(t, fmt.Sprintf("/api/v1/library/%d/offload/preview", id), `{"scope":"item"}`).expect(t, http.StatusOK).into(t, &preview)
	var op apigen.OffloadOperation
	e.post(t, fmt.Sprintf("/api/v1/library/%d/offload", id), fmt.Sprintf(`{"scope":"item","requestId":"w1","fingerprint":%q}`, preview.Fingerprint)).expect(t, http.StatusAccepted).into(t, &op)
	deadline := time.Now().Add(10 * time.Second)
	for op.State != "complete" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		e.get(t, "/api/v1/offload/"+op.OperationId).expect(t, http.StatusOK).into(t, &op)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("not deleted")
	}
	if code, out := post(`{"event":"reacquire_status","kind":"movie","tmdb":550}`); code != 200 || out["matched"] != true || out["state"] != "not_offloaded" || out["mediaItemId"] != float64(id) {
		t.Fatalf("status of an offloaded title: %d %v", code, out)
	}
	if item, _ := e.db.GetMediaItemFull(ctx, id); !item.Offloaded() {
		t.Fatal("reacquire_status must not change anything")
	}
	code, out := post(`{"event":"reacquire","kind":"movie","tmdb":550,"user":"paul"}`)
	if code != 200 || out["matched"] != true || out["state"] != "searching" || out["mediaItemId"] != float64(id) {
		t.Fatalf("reacquire: %d %v", code, out)
	}
	if item, _ := e.db.GetMediaItemFull(ctx, id); item.Offloaded() || !item.Monitored {
		t.Fatalf("reacquire did not bring the title back: %+v", item)
	}
	// Idempotent: the second ask is a 200 that says where it stands.
	if code, out := post(`{"event":"reacquire","kind":"movie","tmdb":550}`); code != 200 || out["matched"] != true || out["state"] == "" || out["state"] == "not_found" {
		t.Fatalf("second reacquire: %d %v", code, out)
	}
	// The contract's refusals and non-matches.
	if code, _ := post(`{"event":"reacquire","kind":"movie"}`); code != http.StatusBadRequest {
		t.Fatalf("no ids = %d, want 400", code)
	}
	if code, _ := post(`{"event":"reacquire","kind":"nonsense","tmdb":550}`); code != http.StatusBadRequest {
		t.Fatalf("unknown kind = %d, want 400", code)
	}
	if code, _ := post(`{"event":"reacquire","kind":"episode","tmdb":550,"season":1}`); code != http.StatusBadRequest {
		t.Fatalf("episode without an episode number = %d, want 400", code)
	}
	if code, out := post(`{"event":"reacquire","kind":"movie","tmdb":999999}`); code != 200 || out["matched"] != false || out["state"] != "not_found" {
		t.Fatalf("unknown title: %d %v", code, out)
	}
	if code, out := post(`{"event":"reacquire","kind":"show","tmdb":550}`); code != 200 || out["matched"] != false || out["state"] != "not_found" {
		t.Fatalf("a movie's id is not a show: %d %v", code, out)
	}
	// An event this build does not know is accepted and dropped.
	if code, out := post(`{"event":"future","kind":"movie","tmdb":550}`); code != 200 || out["matched"] != false {
		t.Fatalf("future event: %d %v", code, out)
	}
}

// first_season is a valid monitor mode over HTTP, and the OpenAPI enum accepts it.
func TestFirstSeasonMonitorModeIsAccepted(t *testing.T) {
	e := newAPIEnv(t)
	rr := e.post(t, "/api/v1/library", `{"kind":"series","tmdbId":1399,"monitor":"first_season"}`)
	if rr.Code == http.StatusBadRequest {
		t.Fatalf("first_season refused: %s", rr.Body.String())
	}
}
