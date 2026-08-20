package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pjunod/monarr/internal/ports"
)

func plurxNotifier(url string) ports.Notifier {
	return New(ports.NotifierConfig{Type: "plurx", Settings: map[string]string{
		"url": url, "apiKey": "plx_secret",
	}})
}

// movieImport builds an import notification for the given DIRECTORIES —
// what plurx is actually asked to index.
func movieImport(dirs ...string) ports.Notification {
	return ports.Notification{
		Event: "import", Title: "Import completed", Body: "Heat.1995.1080p",
		Import: &ports.ImportInfo{
			MediaItemID: 7, DownloadID: 42, Kind: "movie", Title: "Heat",
			Dirs: dirs, TmdbID: 949, ImdbID: "tt0113277",
			Transfer: "t-42-a3f9c1",
		},
	}
}

func TestPlurxSendsOneScanPerImportedDirectory(t *testing.T) {
	var got []map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/scan" {
			t.Errorf("posted to %s, not /api/v1/scan", r.URL.Path)
		}
		auth = r.Header.Get("Authorization")
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		got = append(got, body)
		_, _ = w.Write([]byte(`{"status":"scanned"}`))
	}))
	defer srv.Close()

	err := plurxNotifier(srv.URL).Send(context.Background(),
		movieImport("/media/movies/Heat (1995)"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("sent %d requests, want 1", len(got))
	}
	if auth != "Bearer plx_secret" {
		t.Errorf("Authorization = %q", auth)
	}
	req := got[0]
	if req["path"] != "/media/movies/Heat (1995)" {
		t.Errorf("path = %v", req["path"])
	}
	// The ids are the entire point: without them plurx falls back to
	// matching the folder name, which is how a remake gets the wrong poster.
	ids, _ := req["ids"].(map[string]any)
	if ids == nil || ids["tmdb"] != float64(949) || ids["imdb"] != "tt0113277" {
		t.Errorf("ids = %v", req["ids"])
	}
	if req["hint"] != "movie" {
		t.Errorf("hint = %v", req["hint"])
	}
	if req["correlation_id"] != "t-42-a3f9c1" {
		t.Errorf("correlation_id = %v — without it the transfer cannot be traced across apps", req["correlation_id"])
	}
	if req["source"] != "monarr" {
		t.Errorf("source = %v", req["source"])
	}
}

// A series import must put the SHOW's id under `series`, not under `ids`.
// An episode's own TMDB id is not what identifies the series it belongs to,
// and stamping the show's id onto the episode row is a different bug that
// looks like it worked.
func TestPlurxPutsASeriesIDWhereASeriesIDGoes(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		_, _ = w.Write([]byte(`{"status":"scanned"}`))
	}))
	defer srv.Close()

	n := movieImport("/media/tv/Severance/Season 01")
	n.Import.Kind = "series"
	n.Import.TmdbID = 95396
	if err := plurxNotifier(srv.URL).Send(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	if got["ids"] != nil {
		t.Errorf("ids = %v — a series id must not be stamped on the episode", got["ids"])
	}
	series, _ := got["series"].(map[string]any)
	if series == nil || series["tmdb"] != float64(95396) {
		t.Errorf("series = %v", got["series"])
	}
	if got["hint"] != "episode" {
		t.Errorf("hint = %v", got["hint"])
	}
}

// A season pack is one event, one folder, and several files. plurx indexes
// folders, so a dozen requests naming the same folder would be a dozen
// chances for one to fail and eleven scans of work already done.
func TestPlurxAnnouncesASeasonPackAsOneDirectory(t *testing.T) {
	var count int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&count, 1)
		w.WriteHeader(http.StatusAccepted) // busy: queued, which is a success
		_, _ = w.Write([]byte(`{"status":"queued","request_id":"sr-1"}`))
	}))
	defer srv.Close()

	// Three files, ONE directory: exactly one request.
	if err := plurxNotifier(srv.URL).Send(context.Background(),
		movieImport("/tv/Severance/Season 01")); err != nil {
		t.Fatalf("202 must count as delivered — plurx queued it: %v", err)
	}
	if count != 1 {
		t.Errorf("sent %d requests for one directory — a season pack is one folder", count)
	}
}

// Retrying is the delivery queue's job — it can do it across a restart,
// which a loop inside one Send cannot. What the adapter owns is saying
// WHETHER retrying could help, because that knowledge lives with the status
// codes. Getting it backwards is expensive both ways: a permanent failure
// retried three times just delays the message somebody needs to read, and a
// transient one marked permanent drops a scan on the floor.
func TestPlurxSaysWhetherAFailureIsWorthRetrying(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		permanent bool
	}{
		{"a 502 is the server having a moment", http.StatusBadGateway, "", false},
		{"a 403 is a key that will not grow a scope", http.StatusForbidden, "", true},
		{"a 401 is a credential that will not come back", http.StatusUnauthorized, "", true},
		{"a 422 is a path that will not move", http.StatusUnprocessableEntity,
			`{"error":"path is not under any library root","roots":[]}`, true},
		{"a 404 is a plurx too old to have the route", http.StatusNotFound, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var attempts int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				atomic.AddInt32(&attempts, 1)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			err := plurxNotifier(srv.URL).Send(context.Background(), movieImport("/m"))
			if err == nil {
				t.Fatal("expected an error")
			}
			if attempts != 1 {
				t.Errorf("attempts = %d — the adapter must not retry; the queue does", attempts)
			}
			if got := errors.Is(err, ports.ErrNotifyPermanent); got != tc.permanent {
				t.Errorf("permanent = %v, want %v (%v)", got, tc.permanent, err)
			}
		})
	}
}

// A connection that never opened is the most retryable failure there is —
// it is what a restarting plurx looks like.
func TestPlurxTreatsAnUnreachableServerAsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing listening now

	err := plurxNotifier(url).Send(context.Background(), movieImport("/m"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, ports.ErrNotifyPermanent) {
		t.Errorf("an unreachable server must stay retryable: %v", err)
	}
}

// A mixed batch stays retryable. The transient half deserves another go, and
// a targeted scan is idempotent, so redoing the settled half costs a no-op.
func TestPlurxKeepsAMixedBatchRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), "/m/gone") {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"error":"path is not under any library root","roots":[]}`))
			return
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	err := plurxNotifier(srv.URL).Send(context.Background(), movieImport("/m/gone", "/m/blip"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, ports.ErrNotifyPermanent) {
		t.Errorf("one transient failure makes the batch worth retrying: %v", err)
	}
}

// The 422 is the one people will actually hit: two containers, two different
// mounts. plurx answers with the roots it does know, and that answer has to
// survive the trip into Monarr's log — "422" on its own helps nobody.
func TestPlurxCarriesThePathMappingErrorThrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":"path is not under any library root","roots":["/media/movies"]}`))
	}))
	defer srv.Close()

	err := plurxNotifier(srv.URL).Send(context.Background(), movieImport("/data/media/movies/Heat (1995)"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "/media/movies") {
		t.Errorf("plurx's roots must reach the log, got: %v", err)
	}
	if !strings.Contains(err.Error(), "/data/media/movies/Heat (1995)") {
		t.Errorf("the rejected path must be named too, got: %v", err)
	}
}

// Partial failure has to read as partial. "plurx is down" and "one file of
// six was rejected" call for completely different actions.
func TestPlurxSaysHowMuchOfAnImportLanded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), "/m/bad") {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"error":"path is not under any library root","roots":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"scanned"}`))
	}))
	defer srv.Close()

	err := plurxNotifier(srv.URL).Send(context.Background(),
		movieImport("/m/ok1", "/m/bad", "/m/ok2"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "1 of 3") {
		t.Errorf("the error must quantify the damage, got: %v", err)
	}
}

// A grab or a health event carries no paths. That is not a delivery failure,
// and treating it as one would light the health page up over nothing.
func TestPlurxIgnoresEventsWithNothingToIndex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("plurx was called for an event with no paths")
	}))
	defer srv.Close()

	n := plurxNotifier(srv.URL)
	if err := n.Send(context.Background(), ports.Notification{Event: "grab", Body: "x"}); err != nil {
		t.Errorf("a pathless event is not a failure: %v", err)
	}
	if err := n.Send(context.Background(), movieImport()); err != nil {
		t.Errorf("an import that placed nothing is not a failure: %v", err)
	}
}

// Test proves URL, key and scope without starting a scan of anything real —
// so it stays a button people are willing to press.
func TestPlurxTestPassesOnThePathRejectionAndFailsOnBadCredentials(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":"path is not under any library root","roots":["/media"]}`))
	}))
	defer srv.Close()
	if err := plurxNotifier(srv.URL).Test(context.Background()); err != nil {
		t.Errorf("a 422 means the server answered and the key was accepted: %v", err)
	}
	if body["ids"] != nil || body["correlation_id"] != nil {
		t.Errorf("the test request must be inert, got %v", body)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer bad.Close()
	err := plurxNotifier(bad.URL).Test(context.Background())
	if err == nil {
		t.Fatal("a 401 must fail the test")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("the error must say what happened, got: %v", err)
	}
}

func TestPlurxWithoutConfigurationFailsLoudly(t *testing.T) {
	n := New(ports.NotifierConfig{Type: "plurx", Settings: map[string]string{"url": "http://x"}})
	if err := n.Send(context.Background(), movieImport("/m")); err == nil {
		t.Error("a plurx notifier with no key must not silently do nothing")
	}
}

// What plurx made of the files is the only place the chain "grabbed →
// downloaded → imported → indexed as item 1201" is ever joined up. It has to
// come back, not just "delivered".
func TestPlurxReportsWhatPlurxMadeOfTheFiles(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) == 1 {
			_, _ = w.Write([]byte(`{"status":"scanned","library_id":3,
				"items":[{"item_id":1201,"file_id":88}]}`))
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"queued","request_id":"sr-9f2"}`))
	}))
	defer srv.Close()

	target := plurxNotifier(srv.URL)
	if err := target.Send(context.Background(), movieImport("/m/a", "/m/b")); err != nil {
		t.Fatal(err)
	}
	r, ok := target.(ports.DeliveryReporter)
	if !ok {
		t.Fatal("the plurx notifier must report its delivery")
	}
	got := r.Delivery()
	if !strings.Contains(got, "1201") {
		t.Errorf("plurx's item id must reach the trace, got %q", got)
	}
	if !strings.Contains(got, "sr-9f2") {
		t.Errorf("a queued request id is what somebody polls; got %q", got)
	}
}

// A partial failure still has a successful half, and losing it would throw
// away the record of the files that DID land.
func TestPlurxKeepsTheResultOfThePartThatWorked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), "/m/bad") {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"error":"path is not under any library root","roots":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"scanned","items":[{"item_id":77}]}`))
	}))
	defer srv.Close()

	target := plurxNotifier(srv.URL)
	if err := target.Send(context.Background(), movieImport("/m/ok", "/m/bad")); err == nil {
		t.Fatal("expected the partial failure to be reported")
	}
	got := target.(ports.DeliveryReporter).Delivery()
	if !strings.Contains(got, "77") {
		t.Errorf("the successful half was thrown away: %q", got)
	}
}

// A book is path-identified: the Books library decides whether the imported
// files are text or audio and derives author/title identity from the shelf.
// Curator must still close the handoff immediately instead of leaving the
// periodic Cinema scan to discover the edition hours later.
func TestPlurxSendsBookImportsToTheBooksLibrary(t *testing.T) {
	var got map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		_, _ = w.Write([]byte(`{"status":"scanned","items":[{"item_id":1401}]}`))
	}))
	defer srv.Close()

	n := movieImport("/media/books/Some Author/Some Title")
	n.Import.Kind = "book"
	n.Import.TmdbID = 0
	n.Import.ImdbID = ""
	target := plurxNotifier(srv.URL)
	if err := target.Send(context.Background(), n); err != nil {
		t.Fatalf("book targeted scan: %v", err)
	}
	if auth != "Bearer plx_secret" {
		t.Errorf("Authorization = %q", auth)
	}
	if got["path"] != "/media/books/Some Author/Some Title" {
		t.Errorf("path = %v", got["path"])
	}
	if got["hint"] != "book" {
		t.Errorf("hint = %v, want book", got["hint"])
	}
	if got["ids"] != nil || got["series"] != nil {
		t.Errorf("book request invented provider ids: %v", got)
	}
	if got["correlation_id"] != "t-42-a3f9c1" {
		t.Errorf("correlation_id = %v", got["correlation_id"])
	}
	delivery := target.(ports.DeliveryReporter).Delivery()
	if !strings.Contains(delivery, "1401") {
		t.Errorf("Cinema's book item id must close the transfer trace, got %q", delivery)
	}
}
