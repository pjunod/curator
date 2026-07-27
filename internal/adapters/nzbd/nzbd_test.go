package nzbd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/monarr-media/monarr/internal/ports"
)

// The payloads below were captured from a running nzbd on 2026-07-26, not
// written from its docs — every field name, every null, and the shape of
// `status` (a bare string for simple states, an object while
// post-processing runs) is what the daemon actually emits. A fixture
// invented from a spec tests the spec; this tests the wire.
const (
	queueJSON = `{"jobs":[
      {"assigned_node":null,"category":"tv","critical_health":850,"done_articles":0,
       "downloaded_bytes":250,"dupe_key":"","dupe_score":0,"failed_articles":0,
       "failed_bytes":0,"files_done":0,"files_total":1,"health":1000,"id":1,
       "name":"Show.S01E01","params":[["monarr-transfer","t-42-a3f9c1"]],
       "pp_done":false,"priority":0,"rate_bps":0,"remaining_bytes":750,
       "retried_articles":0,"size_bytes":1000,"status":"queued","total_articles":1},
      {"id":2,"name":"Movie.2024","status":{"post":{"stage":"par_verify"}},
       "size_bytes":100,"downloaded_bytes":100,"params":[]},
      {"id":3,"name":"Other.2024","status":"post_queued",
       "size_bytes":100,"downloaded_bytes":100,"params":[]}
    ]}`

	historyJSON = `{"entries":[
      {"can_requeue":false,"category":"tv","completed_at_unix":1785099832,"dupe_key":"",
       "dupe_score":0,"final_dir":"/downloads/complete/Show.S01E00","first_seen_at_unix":null,
       "health":1000,"hidden":false,"job":10,"last_seen_at_unix":null,"name":"Show.S01E00",
       "params":[["monarr-transfer","t-40-aaaaaa"]],"picked_up_by":null,"removed_at_unix":null,
       "seen_count":0,"seq":7,"size":1000,"status":"SUCCESS"},
      {"job":11,"name":"Bad.Release","status":"FAILURE/HEALTH","final_dir":null,"seq":8},
      {"job":12,"name":"Unpacked.Badly","status":"UNPACK_FAILURE",
       "final_dir":"/downloads/complete/Unpacked.Badly","seq":9},
      {"job":13,"name":"Removed.By.Hand","status":"DELETED","final_dir":null,"seq":10}
    ]}`

	statusJSON = `{"version":"0.1.0+g1bddcf0","built":"2026-07-26 20:23 UTC","up_since_unix":1785099779,
      "download_rate_bps":0,"remaining_bytes":0,"session_downloaded_bytes":0,
      "download_paused":false,"disk_low":false,"quota_reached":false,"blocked_servers":[],
      "health_abort":false,"speed_limit_bps":null,"jobs_queued":0,"jobs_downloading":0,
      "jobs_finished":0,"servers":[]}`
)

// recorder captures what the adapter sent, so the assertions can be about
// the request Monarr makes and not only the reply it accepts.
type recorder struct {
	method string
	path   string
	query  url.Values
	header http.Header
}

func fake(t *testing.T, rec *recorder, handler func(w http.ResponseWriter, r *http.Request) bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rec != nil {
			rec.method, rec.path, rec.query, rec.header = r.Method, r.URL.Path, r.URL.Query(), r.Header.Clone()
		}
		if handler != nil && handler(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/jobs" && r.Method == http.MethodPost:
			_, _ = w.Write([]byte(`{"id":1}`))
		case r.URL.Path == "/api/v1/jobs":
			_, _ = w.Write([]byte(queueJSON))
		case r.URL.Path == "/api/v1/history":
			_, _ = w.Write([]byte(historyJSON))
		case r.URL.Path == "/api/v1/status":
			_, _ = w.Write([]byte(statusJSON))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no such job"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func client(t *testing.T, srv *httptest.Server, user, pass string) *Client {
	t.Helper()
	return New(ports.ClientConfig{Type: "nzbd", Name: "nzbd", URL: srv.URL, Username: user, Password: pass})
}

// The transfer id is the whole reason this adapter speaks the native API:
// it must reach nzbd at admit time, encoded exactly, or the trace this
// integration exists to produce has a hole at its first hop.
func TestAddCarriesTheTransferID(t *testing.T) {
	var rec recorder
	srv := fake(t, &rec, nil)

	h, err := client(t, srv, "", "tok").AddTagged(context.Background(),
		"https://indexer/x.nzb", "tv", "t-42-a3f9c1")
	if err != nil {
		t.Fatalf("AddTagged: %v", err)
	}
	if h != "1" {
		t.Fatalf("handle = %q, want the job id from the response", h)
	}
	if rec.method != http.MethodPost || rec.path != "/api/v1/jobs" {
		t.Fatalf("sent %s %s", rec.method, rec.path)
	}
	if got := rec.query.Get("url"); got != "https://indexer/x.nzb" {
		t.Errorf("url = %q", got)
	}
	if got := rec.query.Get("category"); got != "tv" {
		t.Errorf("category = %q", got)
	}
	var params map[string]string
	if err := json.Unmarshal([]byte(rec.query.Get("params")), &params); err != nil {
		t.Fatalf("params is not a JSON object of strings: %q (%v)", rec.query.Get("params"), err)
	}
	if params[TransferParam] != "t-42-a3f9c1" {
		t.Errorf("params = %v, want %s set", params, TransferParam)
	}
	// nzbd's client registry keys on this; without it a healthy native
	// consumer is invisible in nzbd's UI and looks like nothing connected.
	if got := rec.header.Get("X-Nzbd-Client"); !strings.HasPrefix(got, "monarr/") {
		t.Errorf("X-Nzbd-Client = %q, want monarr/<version>", got)
	}
}

// An untagged add must not send an empty or null params object — nzbd
// would have to parse it, and "no params" is not the same request as
// "params: nothing".
func TestAddWithoutATransferSendsNoParams(t *testing.T) {
	var rec recorder
	srv := fake(t, &rec, nil)
	if _, err := client(t, srv, "", "tok").Add(context.Background(), "https://i/x.nzb", "tv"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, present := rec.query["params"]; present {
		t.Errorf("params sent when there is no transfer id: %q", rec.query.Get("params"))
	}
}

func TestAuthorizationPrefersATokenAndFallsBackToBasic(t *testing.T) {
	var rec recorder
	srv := fake(t, &rec, nil)

	if _, err := client(t, srv, "", "sekret").Add(context.Background(), "u", ""); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if got := rec.header.Get("Authorization"); got != "Bearer sekret" {
		t.Errorf("empty username should mean token auth; got %q", got)
	}

	if _, err := client(t, srv, "paul", "pw").Add(context.Background(), "u", ""); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if got := rec.header.Get("Authorization"); !strings.HasPrefix(got, "Basic ") {
		t.Errorf("a username should mean basic auth; got %q", got)
	}

	// No credentials configured: send none rather than an empty Basic
	// header, which nzbd would reject as a failed attempt.
	if _, err := client(t, srv, "", "").Add(context.Background(), "u", ""); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if got := rec.header.Get("Authorization"); got != "" {
		t.Errorf("unauthenticated client sent %q", got)
	}
}

func TestStatusesMapsQueueAndHistory(t *testing.T) {
	srv := fake(t, nil, nil)
	got, err := client(t, srv, "", "tok").Statuses(context.Background())
	if err != nil {
		t.Fatalf("Statuses: %v", err)
	}
	by := map[ports.Handle]ports.DownloadStatus{}
	for _, s := range got {
		by[s.Handle] = s
	}
	if len(by) != 7 {
		t.Fatalf("got %d statuses, want 3 queued + 4 history", len(by))
	}

	// A queued job, with progress from the byte counters.
	if s := by["1"]; s.State != ports.StateQueued || s.Progress != 0.25 {
		t.Errorf("job 1 = %+v, want queued at 0.25", s)
	}

	// Post-processing is NOT completion. A job mid-unpack whose files are
	// reported importable is how an import finds a half-unpacked folder,
	// so both PP shapes must stay 'downloading' — and say which stage, so
	// "stuck" reads as "verifying since four minutes ago".
	if s := by["2"]; s.State != ports.StateDownloading || !strings.Contains(s.Message, "par verify") {
		t.Errorf("job 2 (mid-PP) = %+v, want downloading naming the stage", s)
	}
	if s := by["3"]; s.State != ports.StateDownloading {
		t.Errorf("job 3 (post_queued) = %+v, want downloading", s)
	}

	// History: success carries the final directory, which is the whole
	// point — it is where the import will look.
	if s := by["10"]; s.State != ports.StateCompleted || s.SavePath != "/downloads/complete/Show.S01E00" {
		t.Errorf("job 10 = %+v, want completed with its final_dir", s)
	}
	if s := by["11"]; s.State != ports.StateFailed || s.Message != "FAILURE/HEALTH" {
		t.Errorf("job 11 = %+v, want failed carrying nzbd's own reason", s)
	}
	if s := by["12"]; s.State != ports.StateFailed || s.Message != "UNPACK_FAILURE" {
		t.Errorf("job 12 = %+v, want failed carrying nzbd's own reason", s)
	}
	// Deleted-by-hand is not a failure at all.
	//
	// It was reported as one (blameless, so at least not blocklisted) until
	// the blameless flag turned out to guard only half the consequence: the
	// failure path also re-searches, so deleting a job in nzbd made Monarr
	// grab another copy of the same release seconds later. StateRemoved is
	// the state that carries the operator's actual meaning — stop, and do
	// not go and find me another one.
	if s := by["13"]; s.State != ports.StateRemoved || !strings.Contains(s.Message, "removed in nzbd") {
		t.Errorf("job 13 = %+v, want removed, saying a human removed it", s)
	}
	if s := by["13"]; s.State == ports.StateFailed {
		t.Error("a deletion reported as a failure will be re-searched and replaced")
	}
}

// `final_dir` is null for anything that never produced a directory. A
// decoder that chokes on it would break the whole poll, not one row.
func TestNullFinalDirIsNotAnError(t *testing.T) {
	srv := fake(t, nil, nil)
	got, err := client(t, srv, "", "t").Statuses(context.Background())
	if err != nil {
		t.Fatalf("Statuses: %v", err)
	}
	for _, s := range got {
		if s.Handle == "11" && s.SavePath != "" {
			t.Errorf("null final_dir became %q", s.SavePath)
		}
	}
}

func TestRemoveFallsBackFromQueueToHistory(t *testing.T) {
	var paths []string
	srv := fake(t, nil, func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.Contains(r.URL.Path, "/actions/") {
			return false
		}
		paths = append(paths, r.URL.Path)
		// The job already retired out of the queue — exactly what the real
		// daemon answers for a finished download.
		if strings.HasPrefix(r.URL.Path, "/api/v1/jobs/") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no such job"}`))
			return true
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
		return true
	})
	if err := client(t, srv, "", "t").Remove(context.Background(), "7", false); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	want := []string{"/api/v1/jobs/7/actions/delete", "/api/v1/history/7/actions/delete"}
	if fmt.Sprint(paths) != fmt.Sprint(want) {
		t.Errorf("tried %v, want the queue then history", paths)
	}

	// deleteData reaches both routes as the destructive action.
	paths = nil
	_ = client(t, srv, "", "t").Remove(context.Background(), "7", true)
	for _, p := range paths {
		if !strings.HasSuffix(p, "/delete-files") {
			t.Errorf("deleteData sent %q, want delete-files", p)
		}
	}
}

// When neither route works, the error has to name both attempts: "no such
// job" twice and "connection refused" twice are very different problems,
// and reporting only the second hides which one happened.
func TestRemoveReportsBothFailures(t *testing.T) {
	srv := fake(t, nil, func(w http.ResponseWriter, r *http.Request) bool {
		if strings.Contains(r.URL.Path, "/actions/") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no such job"}`))
			return true
		}
		return false
	})
	err := client(t, srv, "", "t").Remove(context.Background(), "7", false)
	if err == nil {
		t.Fatal("want an error when both routes fail")
	}
	if !strings.Contains(err.Error(), "queue:") || !strings.Contains(err.Error(), "history:") {
		t.Errorf("error names only one attempt: %v", err)
	}
}

func TestRemoveRejectsANonNumericHandle(t *testing.T) {
	srv := fake(t, nil, nil)
	if err := client(t, srv, "", "t").Remove(context.Background(), "abc", false); err == nil {
		t.Fatal("want an error for a handle that is not a job id")
	}
}

func TestTest(t *testing.T) {
	srv := fake(t, nil, nil)
	if err := client(t, srv, "", "tok").Test(context.Background()); err != nil {
		t.Fatalf("Test against a healthy daemon: %v", err)
	}
}

// A 200 from something that is not nzbd — a reverse proxy's index page,
// another app on that port — must fail the test, not pass it and then
// fail mysteriously at the first grab.
func TestTestRejectsAnImpostor(t *testing.T) {
	srv := fake(t, nil, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/api/v1/status" {
			_, _ = w.Write([]byte(`{"ok":true}`))
			return true
		}
		return false
	})
	err := client(t, srv, "", "tok").Test(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not like nzbd") {
		t.Fatalf("err = %v, want a clear 'this is not nzbd'", err)
	}
}

func TestUnauthorizedSaysSo(t *testing.T) {
	srv := fake(t, nil, func(w http.ResponseWriter, r *http.Request) bool {
		w.WriteHeader(http.StatusUnauthorized)
		return true
	})
	err := client(t, srv, "", "wrong").Test(context.Background())
	if err == nil || !strings.Contains(err.Error(), "authentication") {
		t.Fatalf("err = %v, want an authentication error", err)
	}
}

// nzbd answers errors as {"error": "..."}. Surfacing its words beats a
// status code the operator then has to go look up.
func TestErrorBodyIsSurfaced(t *testing.T) {
	srv := fake(t, nil, func(w http.ResponseWriter, r *http.Request) bool {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":"param key \"*PP:done\" is reserved"}`))
		return true
	})
	_, err := client(t, srv, "", "t").Add(context.Background(), "u", "")
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("err = %v, want nzbd's own message", err)
	}
}

// The adapter must satisfy both the port and the optional capability, or
// Grab's type assertion silently falls back to an untagged add and the
// transfer id vanishes with no error anywhere.
var (
	_ ports.DownloadClient = (*Client)(nil)
	_ ports.TaggedAdder    = (*Client)(nil)
)
