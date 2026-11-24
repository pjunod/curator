package api

import (
	"net/http"
	"testing"

	apigen "github.com/pjunod/monarr/internal/api/gen"
)

// GET /library/{id}/status after an auto search: the search is recorded and
// the grab shows as downloading; the generated schema decodes the body.
func TestItemStatusEndpoint(t *testing.T) {
	e := newAPIEnv(t)
	item := e.addMovie(t)
	e.get(t, "/api/v1/library/9999/status").expect(t, http.StatusNotFound)

	var before apigen.ItemStatus
	e.get(t, "/api/v1/library/"+acqItoa(item)+"/status").expect(t, http.StatusOK).into(t, &before)
	if before.Active || before.Stages == nil || len(*before.Stages) != 6 || (*before.Stages)[0].State != apigen.StatusStageStateTodo {
		t.Fatalf("before any search = %+v", before)
	}

	acqAddIndexer(t, e)
	acqAddClient(t, e, "qbittorrent")
	e.post(t, "/api/v1/library/"+acqItoa(item)+"/autosearch", "").expect(t, http.StatusOK)

	var after apigen.ItemStatus
	e.get(t, "/api/v1/library/"+acqItoa(item)+"/status").expect(t, http.StatusOK).into(t, &after)
	st := *after.Stages
	if !after.Active || after.Download == nil || st[0].State != apigen.StatusStageStateDone || st[1].Key != apigen.StatusStageKeyDownloading || st[1].State != apigen.StatusStageStateActive {
		t.Fatalf("after the grab = %+v stages %+v", after, st)
	}

	// Nothing on disk to confirm: a 409 that says so.
	e.post(t, "/api/v1/library/"+acqItoa(item)+"/status/confirm", "").expect(t, http.StatusConflict)
}
