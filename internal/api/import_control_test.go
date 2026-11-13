package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apigen "github.com/pjunod/monarr/internal/api/gen"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

func TestCancelAPIReportsPendingPublicationAndRejectsStaleEpoch(t *testing.T) {
	e := newAPIEnv(t)
	acqAddClient(t, e, "qbittorrent")
	item := e.addMovie(t)
	id := acqGrab(t, e, item)
	ctx := context.Background()
	dl, err := e.db.GetDownload(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	dl.State = "importing"
	if err = e.db.UpdateDownloadHandoff(ctx, dl); err != nil {
		t.Fatal(err)
	}
	a, err := e.db.EnsureImportAttempt(ctx, id, dl.GenerationKey)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.db.SealImportAttempt(ctx, a.ImportAttemptKey, []sqlite.ImportAttemptFile{{Source: "/source/file", Kind: "media"}}, false); err != nil {
		t.Fatal(err)
	}
	epoch := a.Epoch
	p := sqlite.Placement{ID: "uncertain", DownloadID: id, ImportEpoch: &epoch, SourceGeneration: dl.GenerationKey, ItemID: item, Source: "/source/file", Target: "/library/file", Temporary: "/library/stage", SHA256: "digest", State: "prepared"}
	if err = e.db.PreparePlacement(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err = e.db.AuthorizeImportPlacement(ctx, p); err != nil {
		t.Fatal(err)
	}
	identity := map[string]string{"generationKey": dl.GenerationKey, "epoch": "0"}
	e.postImportJSON(t, "/api/v1/queue/"+acqItoa(id)+"/cancel-import", identity).expect(t, http.StatusAccepted)
	var rows []apigen.QueueItem
	e.get(t, "/api/v1/queue").expect(t, 200).into(t, &rows)
	if len(rows) != 1 || rows[0].ImportControl == nil || rows[0].ImportControl.Outcome != "pending" || rows[0].ImportControl.PendingPublications != 1 {
		t.Fatalf("pending control not visible: %+v", rows)
	}
	e.postImportJSON(t, "/api/v1/queue/"+acqItoa(id)+"/cancel-import", map[string]string{"generationKey": dl.GenerationKey, "epoch": "1"}).expect(t, http.StatusConflict)
	e.postImportJSON(t, "/api/v1/queue/"+acqItoa(id)+"/import-review", map[string]any{"generationKey": dl.GenerationKey, "epoch": "0", "action": "retain_uncertain", "ids": []string{"placement:uncertain"}, "acknowledged": false}).expect(t, http.StatusConflict)
	e.postImportJSON(t, "/api/v1/queue/"+acqItoa(id)+"/import-review", map[string]any{"generationKey": dl.GenerationKey, "epoch": "0", "action": "retain_uncertain", "ids": []string{"placement:uncertain"}, "acknowledged": true}).expect(t, 200)
	e.postImportJSON(t, "/api/v1/queue/"+acqItoa(id)+"/cancel-import", identity).expect(t, http.StatusNoContent)
}

func TestDeploymentDrainKeepsStatusAvailableAndFencesMutations(t *testing.T) {
	e := newAPIEnv(t)
	owner := strings.Repeat("c1", 32)
	var status struct {
		Phase, Generation string
		Active            int
	}
	e.postImportJSON(t, "/api/v1/system/deployment-drain", map[string]string{"ownerToken": owner}).expect(t, 200).into(t, &status)
	if status.Phase != "drained" || status.Generation == "" {
		t.Fatalf("%+v", status)
	}
	e.get(t, "/api/v1/system/deployment-drain").expect(t, 200)
	e.get(t, "/api/v1/activity").expect(t, 200)
	e.get(t, "/api/v1/queue").expect(t, 200)
	// Unknown and provider-backed reads are not allowed to silently persist
	// discovery budgets or preview state after the barrier closes.
	e.postImportJSON(t, "/api/v1/system/tasks/backlog.search/run", nil).expect(t, http.StatusServiceUnavailable)
	e.postImportJSON(t, "/api/v1/system/deployment-drain", map[string]string{"ownerToken": strings.Repeat("d1", 32)}).expect(t, http.StatusConflict)
}

func (e *apiEnv) postImportJSON(t *testing.T, path string, body any) *httptestRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return e.post(t, path, string(raw))
}

func TestImportReviewReadsImplicitAndRetainedCustodyWithoutMutation(t *testing.T) {
	e := newAPIEnv(t)
	acqAddClient(t, e, "qbittorrent")
	item := e.addMovie(t)
	id := acqGrab(t, e, item)
	ctx := context.Background()
	var review struct {
		Attempt      apigen.ImportAttemptStatus
		Publications []map[string]any
		Stages       []map[string]any
		WriterActive bool
	}
	path := "/api/v1/queue/" + acqItoa(id) + "/import-review"
	e.get(t, path).expect(t, 200).into(t, &review)
	if review.Attempt.Epoch != "0" || review.WriterActive {
		t.Fatalf("%+v", review)
	}
	dl, err := e.db.GetDownload(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	a, err := e.db.EnsureImportAttempt(ctx, id, dl.GenerationKey)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.db.RecordImportStage(ctx, a.ImportAttemptKey, "/source/caption", "/library/caption", "/library/.stage"); err != nil {
		t.Fatal(err)
	}
	e.get(t, path).expect(t, 200).into(t, &review)
	if review.Attempt.HeldStages != 1 || len(review.Stages) != 1 {
		t.Fatalf("custody not exposed: %+v", review)
	}
	e.get(t, "/api/v1/queue/999999/import-review").expect(t, 404)
	e.post(t, path, "{").expect(t, 400)
	e.postImportJSON(t, path, map[string]any{"generationKey": dl.GenerationKey, "epoch": "bad", "action": "operator_disposed", "acknowledged": true}).expect(t, 400)
	e.postImportJSON(t, path, map[string]any{"generationKey": dl.GenerationKey, "epoch": "0", "action": "operator_disposed", "ids": []string{"stagepath:/library/.stage"}, "acknowledged": true}).expect(t, 200)
	e.get(t, path).expect(t, 200).into(t, &review)
	if review.Attempt.HeldStages != 0 || len(review.Stages) != 0 {
		t.Fatalf("disposition not exposed: %+v", review)
	}
	e.post(t, "/api/v1/queue/"+acqItoa(id)+"/cancel-import", "{").expect(t, 400)
	e.postImportJSON(t, "/api/v1/queue/"+acqItoa(id)+"/cancel-import", map[string]string{"generationKey": dl.GenerationKey, "epoch": "-1"}).expect(t, 400)
}

func TestDrainAbortChecksOwnerGenerationAndTerminalShutdown(t *testing.T) {
	e := newAPIEnv(t)
	path := "/api/v1/system/deployment-drain"
	owner := strings.Repeat("91", 32)
	e.post(t, path, "{").expect(t, 400)
	e.postImportJSON(t, path, map[string]string{"ownerToken": "short"}).expect(t, 400)
	var status struct{ Phase, Generation string }
	e.postImportJSON(t, path, map[string]string{"ownerToken": owner}).expect(t, 200).into(t, &status)
	// Use the handler directly so the generation and owner are explicit.
	invoke := func(body string, expected int) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("DELETE", path, strings.NewReader(body))
		e.srv.AbortDeploymentDrain(w, r)
		if w.Code != expected {
			t.Fatalf("abort %d: %s", w.Code, w.Body.String())
		}
	}
	invoke("{", 400)
	invoke(`{"ownerToken":"`+owner+`","generation":"stale"}`, 409)
	invoke(`{"ownerToken":"`+owner+`","generation":"`+status.Generation+`"}`, 200)
	e.postImportJSON(t, path, map[string]string{"ownerToken": owner}).expect(t, 200)
	e.srv.BeginShutdown()
	invoke(`{"ownerToken":"`+owner+`","generation":"`+status.Generation+`"}`, 409)
}
