package nzbd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pjunod/monarr/internal/ports"
)

func TestStatusPollStreamsLargeRecoveryRecordsAndInternalProofs(t *testing.T) {
	large := strings.Repeat("x", 9<<20)
	control := `{"version":1,"revision":"13","lifecycle":"running","cause":"post_failure","stage":"post_unpack_rename","instance":"same"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/jobs":
			_, _ = fmt.Fprintf(w, `{"jobs":[{"id":901,"status":"downloading","params":[["*File:proof:1",%q],["monarr-transfer","t-901"]]}]}`, large)
		case "/api/v1/history":
			_, _ = fmt.Fprintf(w, `{"entries":[{"job":902,"record":{"files":[{"segments":%q}],"total_articles":10,"success_articles":7},"params":[["*File:proof:2",%q],["*Control:v1",%q],["monarr-transfer","t-902"],["Failure:Files","retained"]],"status":"FAILURE/HEALTH"},{"job":903,"status":"SUCCESS","final_dir":"/working/complete","params":null}]}`, large, large, control)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	statuses, err := client(t, srv, "", "").Statuses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].Transfer != "t-901" {
		t.Fatalf("lost the queue row after large fields: %+v", statuses)
	}
	facts, _, _, err := client(t, srv, "", "").HistorySince(context.Background(), ports.HistoryCursor{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 2 {
		t.Fatalf("lost history rows after large fields: %+v", facts)
	}
	failed := facts[0].Terminal
	if facts[0].Transfer != "t-902" || failed.Outcome != ports.OutcomeFailed || failed.Fetched.SuccessArticles != 7 ||
		failed.Fetched.TotalArticles != 10 || !strings.Contains(failed.Message, "retained") {
		t.Fatalf("lost authoritative metadata: %+v %+v", facts[0], failed)
	}
	if facts[1].Terminal.Outcome != ports.OutcomeCompleted {
		t.Fatalf("large record hid later completion: %+v", facts[1])
	}
}

func TestStatusStreamingRejectsTruncationAndAmbiguousMetadata(t *testing.T) {
	for _, body := range []string{
		`{"jobs":[{"id":1,"record":{"files":["unfinished`,
		`{"jobs":[{"id":1}],`,
		`{"jobs":[],}`,
		`{"jobs":[],null:1}`,
		`{"jobs":[{"record":{"files":[garbage]}}]}`,
		`{"jobs":[{"record":{"files":[1,]}}]}`,
		`{"jobs":[{"params":[["ignored","bad\q"]]}]}`,
		`{"jobs":[{"ignored":{"x":true,}}]}`,
		`{"jobs":[]} {}`,
		`{"jobs":[],"jobs":[]}`,
		`{"jobs":[{"id":1,"id":2}]}`,
		`{"jobs":[{"params":[["*Control:v1","{}"],["*Control:v1","{}"]]}]}`,
		`{"jobs":[{"name":"` + strings.Repeat("a", statusFieldBudget+1) + `"}]}`,
	} {
		var page statusQueue
		if err := page.readStatusJSON(strings.NewReader(body)); err == nil {
			t.Fatalf("accepted malformed/ambiguous status (length %d)", len(body))
		}
	}
}

func TestStatusStreamingRequiresCompleteTransportEvenAfterCompleteJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte(`{"jobs":[]}`))
	}))
	defer srv.Close()
	if _, err := client(t, srv, "", "").Statuses(context.Background()); err == nil {
		t.Fatal("accepted truncated HTTP transport")
	}
}

func TestStatusStreamingRetainsOnlyPollingParametersAndArticleCounts(t *testing.T) {
	var page statusHistory
	if err := page.readStatusJSON(strings.NewReader(`{"entries":[{"job":1,"record":{"files":[1,2,3],"total_articles":9,"success_articles":4},"params":[["*File:proof:1","not needed"],["monarr-transfer","t-1"]]}]}`)); err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 1 || len(page.Entries[0].Params) != 1 || page.Entries[0].Record == nil || page.Entries[0].Record.TotalArticles != 9 {
		t.Fatalf("unexpected retained metadata: %+v", page)
	}
	raw, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "not needed") {
		t.Fatal("retained internal payload proof")
	}
}
