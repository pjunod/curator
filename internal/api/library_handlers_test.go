package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/app/health"
	"github.com/pjunod/monarr/internal/app/library"
	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/bus"
	"github.com/pjunod/monarr/internal/infra/scheduler"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

type stubProvider struct{ configured bool }

func (p stubProvider) SearchMovies(ctx context.Context, q string) ([]ports.SearchResult, error) {
	if !p.configured {
		return nil, ports.ErrProviderNotConfigured
	}
	return []ports.SearchResult{
		{Kind: domain.KindMovie, TMDBID: 550, Title: "Fight Club", Year: 1999, PosterPath: "/fc.jpg"},
	}, nil
}

func (p stubProvider) SearchSeries(ctx context.Context, q string) ([]ports.SearchResult, error) {
	return nil, nil
}

func (p stubProvider) GetMovie(ctx context.Context, id int64) (domain.MediaItem, error) {
	if !p.configured {
		return domain.MediaItem{}, ports.ErrProviderNotConfigured
	}
	return domain.MediaItem{
		Kind: domain.KindMovie, Title: "Fight Club", SortTitle: "fight club",
		Year: 1999, IDs: domain.ExternalIDs{TMDB: id, IMDB: "tt0137523"},
		Genres: []string{"Drama"}, Runtime: 139,
	}, nil
}

func (p stubProvider) GetSeries(ctx context.Context, id int64) (domain.MediaItem, error) {
	return domain.MediaItem{}, nil
}

func newLibraryServer(t *testing.T, provider ports.MetadataProvider) (http.Handler, *sqlite.DB) {
	t.Helper()
	db, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	b := bus.New(nil)
	t.Cleanup(b.Close)

	lib := library.New(db, provider, b, nil)

	sched := scheduler.New(nil, b, nil)
	if err := sched.Register(scheduler.Task{
		Name:     ScanTaskName,
		Interval: time.Hour,
		Fn: func(ctx context.Context) error {
			_, err := lib.Scan(ctx)
			return err
		},
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := sched.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); sched.Wait() })

	srv := New(Deps{
		Bus: b, Health: health.NewRegistry(nil), Scheduler: sched,
		DB: fakeDB{v: 2}, Library: lib, Settings: db,
		Version: "test", DataDir: t.TempDir(),
	})
	return srv.Handler(), db
}

func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestSearchUnconfiguredProviderIs503(t *testing.T) {
	h, _ := newLibraryServer(t, stubProvider{configured: false})
	rr := do(t, h, "GET", "/api/v1/metadata/search?kind=movie&query=fight", "")
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, body %s", rr.Code, rr.Body.String())
	}
}

func TestAddListGetDeleteFlow(t *testing.T) {
	h, _ := newLibraryServer(t, stubProvider{configured: true})

	// Search shows the candidate, not yet in library.
	rr := do(t, h, "GET", "/api/v1/metadata/search?kind=movie&query=fight", "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"inLibrary":false`) {
		t.Fatalf("search: %d %s", rr.Code, rr.Body.String())
	}

	// Add.
	rr = do(t, h, "POST", "/api/v1/library", `{"kind":"movie","tmdbId":550}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("add: %d %s", rr.Code, rr.Body.String())
	}
	var added struct {
		Id    int64  `json:"id"`
		Title string `json:"title"`
		Ids   struct {
			Imdb string `json:"imdb"`
		} `json:"ids"`
	}
	json.Unmarshal(rr.Body.Bytes(), &added)
	if added.Title != "Fight Club" || added.Ids.Imdb != "tt0137523" {
		t.Errorf("added = %+v", added)
	}

	// Duplicate → 409.
	rr = do(t, h, "POST", "/api/v1/library", `{"kind":"movie","tmdbId":550}`)
	if rr.Code != http.StatusConflict {
		t.Errorf("dup add: %d", rr.Code)
	}

	// Search now flags inLibrary.
	rr = do(t, h, "GET", "/api/v1/metadata/search?kind=movie&query=fight", "")
	if !strings.Contains(rr.Body.String(), `"inLibrary":true`) {
		t.Errorf("search after add: %s", rr.Body.String())
	}

	// List + kind filter.
	rr = do(t, h, "GET", "/api/v1/library?kind=movie", "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "Fight Club") {
		t.Errorf("list: %d %s", rr.Code, rr.Body.String())
	}
	rr = do(t, h, "GET", "/api/v1/library?kind=series", "")
	if rr.Body.String() == "" || strings.Contains(rr.Body.String(), "Fight Club") {
		t.Errorf("series filter should be empty: %s", rr.Body.String())
	}

	// Detail.
	rr = do(t, h, "GET", "/api/v1/library/1", "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"runtime":139`) {
		t.Errorf("detail: %d %s", rr.Code, rr.Body.String())
	}

	// Delete, then 404.
	rr = do(t, h, "DELETE", "/api/v1/library/1", "")
	if rr.Code != http.StatusNoContent {
		t.Errorf("delete: %d", rr.Code)
	}
	rr = do(t, h, "GET", "/api/v1/library/1", "")
	if rr.Code != http.StatusNotFound {
		t.Errorf("get after delete: %d", rr.Code)
	}
}

func TestBadKindIs400(t *testing.T) {
	h, _ := newLibraryServer(t, stubProvider{configured: true})
	rr := do(t, h, "POST", "/api/v1/library", `{"kind":"music","tmdbId":1}`)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("unknown kind: %d", rr.Code)
	}
	// Books are a real kind (Phase 2.5) — but with no book provider wired
	// the add reports "provider not configured", not "bad request".
	rr = do(t, h, "POST", "/api/v1/library", `{"kind":"book","olid":"OL1W"}`)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("book add without provider: %d", rr.Code)
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	h, _ := newLibraryServer(t, stubProvider{configured: true})

	rr := do(t, h, "GET", "/api/v1/settings", "")
	if !strings.Contains(rr.Body.String(), `"tmdbApiKeyConfigured":false`) {
		t.Fatalf("initial settings: %s", rr.Body.String())
	}

	rr = do(t, h, "PUT", "/api/v1/settings", `{"tmdbApiKey":"secret-key-1234"}`)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("put: %d", rr.Code)
	}

	rr = do(t, h, "GET", "/api/v1/settings", "")
	body := rr.Body.String()
	if !strings.Contains(body, `"tmdbApiKeyConfigured":true`) || !strings.Contains(body, "…1234") {
		t.Errorf("settings after set: %s", body)
	}
	if strings.Contains(body, "secret-key") {
		t.Errorf("full key must never be returned: %s", body)
	}
}

func TestRootFoldersAPI(t *testing.T) {
	h, _ := newLibraryServer(t, stubProvider{configured: true})
	dir := t.TempDir()

	rr := do(t, h, "POST", "/api/v1/rootfolders", `{"path":"`+dir+`"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("add root: %d %s", rr.Code, rr.Body.String())
	}
	rr = do(t, h, "POST", "/api/v1/rootfolders", `{"path":"not-absolute"}`)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("bad root: %d", rr.Code)
	}
	rr = do(t, h, "GET", "/api/v1/rootfolders", "")
	if !strings.Contains(rr.Body.String(), `"accessible":true`) {
		t.Errorf("list roots: %s", rr.Body.String())
	}
	rr = do(t, h, "DELETE", "/api/v1/rootfolders/1", "")
	if rr.Code != http.StatusNoContent {
		t.Errorf("delete root: %d", rr.Code)
	}
	rr = do(t, h, "DELETE", "/api/v1/rootfolders/99", "")
	if rr.Code != http.StatusNotFound {
		t.Errorf("delete missing root: %d", rr.Code)
	}
}

func TestScanEndpoints(t *testing.T) {
	h, _ := newLibraryServer(t, stubProvider{configured: true})

	rr := do(t, h, "GET", "/api/v1/library/scan/report", "")
	if rr.Code != http.StatusNotFound {
		t.Errorf("report before scan: %d", rr.Code)
	}
	rr = do(t, h, "POST", "/api/v1/library/scan", "")
	if rr.Code != http.StatusAccepted {
		t.Fatalf("trigger scan: %d", rr.Code)
	}
	// The scan runs async via the scheduler; poll briefly for the report.
	deadline := time.Now().Add(3 * time.Second)
	for {
		rr = do(t, h, "GET", "/api/v1/library/scan/report", "")
		if rr.Code == http.StatusOK || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "scannedAt") {
		t.Errorf("report after scan: %d %s", rr.Code, rr.Body.String())
	}
}
