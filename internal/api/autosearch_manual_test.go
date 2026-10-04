package api

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	apigen "github.com/pjunod/monarr/internal/api/gen"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

// The Auto search button is a person waiting on an answer. With the automatic
// search allowance spent — what a busy backlog leaves behind — the endpoint
// must still search, charged to the interactive allowance. Before this it
// answered seen=0 without asking any indexer, and the UI read that as "No
// releases came back from any indexer".
func TestAutoSearchEndpointIsChargedAsManual(t *testing.T) {
	e := newAPIEnv(t)
	item := e.addMovie(t)
	acqAddIndexer(t, e)
	acqAddClient(t, e, "qbittorrent")
	ctx := context.Background()

	indexers, err := e.db.ListIndexers(ctx)
	if err != nil || len(indexers) != 1 {
		t.Fatalf("indexers = %v, err = %v", indexers, err)
	}
	var deferred *sqlite.BudgetDeferred
	for i := 0; !errors.As(err, &deferred); i++ {
		if err != nil || i > 1000 {
			t.Fatalf("spending the automatic allowance: i=%d err=%v", i, err)
		}
		err = e.db.ReserveIndexerRequest(ctx, indexers[0].ID, "search", true, time.Now())
	}
	var before int
	if err := e.db.R.QueryRowContext(ctx, `SELECT count(*) FROM indexer_request_usage WHERE automatic=1`).Scan(&before); err != nil {
		t.Fatal(err)
	}

	var out apigen.AutoSearchResult
	e.post(t, "/api/v1/library/"+acqItoa(item)+"/autosearch", "").
		expect(t, http.StatusOK).into(t, &out)
	if out.Grabbed != 1 || len(out.Targets) != 1 || out.Targets[0].Incomplete != nil {
		t.Fatalf("outcome = %+v, want a grab with every indexer searched", out)
	}
	var after, interactive int
	if err := e.db.R.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE automatic=1), count(*) FILTER (WHERE automatic=0 AND bucket IN ('interactive','headroom','search')) FROM indexer_request_usage`).Scan(&after, &interactive); err != nil {
		t.Fatal(err)
	}
	if after != before || interactive == 0 {
		t.Fatalf("automatic rows %d -> %d, manual rows %d; want the press charged as manual only", before, after, interactive)
	}
}
