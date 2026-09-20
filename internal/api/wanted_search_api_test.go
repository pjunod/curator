package api

import (
	"encoding/json"
	"net/http"
	"testing"

	apigen "github.com/pjunod/monarr/internal/api/gen"
	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/ports"
)

func TestWantedSearchAPIValidatesScopeReusesIdenticalRunAndReportsConflict(t *testing.T) {
	e := newAPIEnv(t)
	ctx := t.Context()
	if _, err := e.db.CreateMediaItem(ctx, domain.MediaItem{
		Kind: domain.KindMovie, Title: "Wanted Movie", SortTitle: "wanted movie",
		Year: 2024, IDs: domain.ExternalIDs{TMDB: 9911}, Monitored: true,
		Path: e.root,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.AddIndexer(ctx, ports.IndexerConfig{
		Name: "idx", URL: "http://indexer.invalid", Protocol: "torrent", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	first := e.post(t, "/api/v1/wanted/searches", `{"scope":"all","targetDelayMs":0}`).expect(t, http.StatusAccepted)
	var run apigen.WantedSearchRun
	if err := json.Unmarshal(first.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	if run.Selected != 1 || run.Scope.Scope != apigen.WantedSearchScopeScopeAll {
		t.Fatalf("accepted run = %+v", run)
	}
	second := e.post(t, "/api/v1/wanted/searches", `{"scope":"all","targetDelayMs":0}`).expect(t, http.StatusAccepted)
	var reused apigen.WantedSearchRun
	_ = json.Unmarshal(second.Body.Bytes(), &reused)
	if reused.RunId != run.RunId {
		t.Fatalf("identical submission created %q, want reuse %q", reused.RunId, run.RunId)
	}
	e.post(t, "/api/v1/wanted/searches", `{"scope":"reason","reason":"missing","targetDelayMs":0}`).expect(t, http.StatusConflict)
	e.post(t, "/api/v1/wanted/searches", `{"scope":"all","reason":"missing"}`).expect(t, http.StatusBadRequest)
	e.post(t, "/api/v1/wanted/searches", `{"scope":"target","wantableId":"season:1:2"}`).expect(t, http.StatusBadRequest)
	e.post(t, "/api/v1/wanted/searches", `{"scope":"all","surprise":true}`).expect(t, http.StatusBadRequest)

	e.get(t, "/api/v1/wanted/searches/"+run.RunId).expect(t, http.StatusOK)
	e.post(t, "/api/v1/wanted/searches/"+run.RunId+"/cancel", "").expect(t, http.StatusAccepted)
	e.post(t, "/api/v1/wanted/searches/"+run.RunId+"/cancel", "").expect(t, http.StatusOK)
}
