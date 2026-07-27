package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/monarr-media/monarr/internal/ports"
)

func plurxNotifier(url string) ports.Notifier {
	return New(ports.NotifierConfig{Type: "plurx", Settings: map[string]string{
		"url": url, "apiKey": "plx_secret",
	}})
}

func movieImport(paths ...string) ports.Notification {
	return ports.Notification{
		Event: "import", Title: "Import completed", Body: "Heat.1995.1080p",
		Import: &ports.ImportInfo{
			MediaItemID: 7, Paths: paths, TMDBID: 949, IMDBID: "tt0113277",
			Transfer: "t-42-a3f9c1",
		},
	}
}

func TestPlurxSendsOneScanPerImportedPath(t *testing.T) {
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
		movieImport("/media/movies/Heat (1995)/Heat (1995) [Bluray-1080p].mkv"))
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
	if req["path"] != "/media/movies/Heat (1995)/Heat (1995) [Bluray-1080p].mkv" {
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

	n := movieImport("/media/tv/Severance/Season 01/S01E01.mkv")
	n.Import.Episode = true
	n.Import.TMDBID = 95396
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

// A season pack is one event and several files. Every one of them has to be
// announced, or the season is half-indexed and nothing says so.
func TestPlurxAnnouncesEveryFileOfASeasonPack(t *testing.T) {
	var count int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&count, 1)
		w.WriteHeader(http.StatusAccepted) // busy: queued, which is a success
		_, _ = w.Write([]byte(`{"status":"queued","request_id":"sr-1"}`))
	}))
	defer srv.Close()

	paths := []string{"/tv/S01E01.mkv", "/tv/S01E02.mkv", "/tv/S01E03.mkv"}
	if err := plurxNotifier(srv.URL).Send(context.Background(), movieImport(paths...)); err != nil {
		t.Fatalf("202 must count as delivered — plurx queued it: %v", err)
	}
	if int(count) != len(paths) {
		t.Errorf("sent %d requests for %d files", count, len(paths))
	}
}

func TestPlurxRetriesWhatRetryingCanFixAndNotWhatItCannot(t *testing.T) {
	t.Run("a 5xx is retried", func(t *testing.T) {
		var attempts int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if atomic.AddInt32(&attempts, 1) < 3 {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			_, _ = w.Write([]byte(`{"status":"scanned"}`))
		}))
		defer srv.Close()
		if err := plurxNotifier(srv.URL).Send(context.Background(), movieImport("/m/a.mkv")); err != nil {
			t.Fatalf("a transient 502 should have been ridden out: %v", err)
		}
		if attempts != 3 {
			t.Errorf("attempts = %d, want 3", attempts)
		}
	})

	t.Run("a 403 is not", func(t *testing.T) {
		var attempts int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			atomic.AddInt32(&attempts, 1)
			w.WriteHeader(http.StatusForbidden)
		}))
		defer srv.Close()
		err := plurxNotifier(srv.URL).Send(context.Background(), movieImport("/m/a.mkv"))
		if err == nil {
			t.Fatal("a 403 must be reported, not swallowed")
		}
		if attempts != 1 {
			t.Errorf("attempts = %d — a key without the scope will not grow one", attempts)
		}
		if !strings.Contains(err.Error(), "scan:trigger") {
			t.Errorf("the error must say what is missing, got: %v", err)
		}
	})
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

	err := plurxNotifier(srv.URL).Send(context.Background(), movieImport("/data/media/movies/Heat.mkv"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "/media/movies") {
		t.Errorf("plurx's roots must reach the log, got: %v", err)
	}
	if !strings.Contains(err.Error(), "/data/media/movies/Heat.mkv") {
		t.Errorf("the rejected path must be named too, got: %v", err)
	}
}

// Partial failure has to read as partial. "plurx is down" and "one file of
// six was rejected" call for completely different actions.
func TestPlurxSaysHowMuchOfAnImportLanded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), "bad.mkv") {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"error":"path is not under any library root","roots":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"scanned"}`))
	}))
	defer srv.Close()

	err := plurxNotifier(srv.URL).Send(context.Background(),
		movieImport("/m/ok1.mkv", "/m/bad.mkv", "/m/ok2.mkv"))
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
	if err := n.Send(context.Background(), movieImport("/m/a.mkv")); err == nil {
		t.Error("a plurx notifier with no key must not silently do nothing")
	}
}
