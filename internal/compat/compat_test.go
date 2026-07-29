package compat_test

// Fake-consumer conformance tests: replay the request sequences Jellyseerr,
// Prowlarr, and Bazarr actually issue against real Sonarr/Radarr v3 APIs,
// and assert the personalities answer in shapes those consumers accept.
// The docker-compose harness in test/conformance runs the same flows
// against the real applications.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/monarr-media/monarr/internal/app/library"
	"github.com/monarr-media/monarr/internal/compat"
	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/infra/bus"
	"github.com/monarr-media/monarr/internal/infra/sqlite"
	"github.com/monarr-media/monarr/internal/ports"
)

const testKey = "conformance-key"

type fakeProvider struct{}

func (fakeProvider) SearchMovies(ctx context.Context, q string) ([]ports.SearchResult, error) {
	return []ports.SearchResult{{Kind: domain.KindMovie, TMDBID: 550, Title: "Fight Club", Year: 1999}}, nil
}

func (fakeProvider) SearchSeries(ctx context.Context, q string) ([]ports.SearchResult, error) {
	return []ports.SearchResult{{Kind: domain.KindSeries, TMDBID: 100, Title: "Test Show", Year: 2020}}, nil
}

func (fakeProvider) GetMovie(ctx context.Context, id int64) (domain.MediaItem, error) {
	return domain.MediaItem{
		Kind: domain.KindMovie, Title: "Fight Club", SortTitle: "fight club",
		Year: 1999, IDs: domain.ExternalIDs{TMDB: id, IMDB: "tt0137523"},
		Status: "Released", Runtime: 139,
	}, nil
}

func (fakeProvider) GetSeries(ctx context.Context, id int64) (domain.MediaItem, error) {
	return domain.MediaItem{
		Kind: domain.KindSeries, Title: "Test Show", SortTitle: "test show",
		Year: 2020, IDs: domain.ExternalIDs{TMDB: id, TVDB: 700700}, Ended: true,
		Seasons: []domain.Season{{
			Number: 1, Monitored: true,
			Episodes: []domain.Episode{
				{SeasonNumber: 1, EpisodeNumber: 1, Title: "Pilot", AirDate: "2020-01-01", Monitored: true},
				{SeasonNumber: 1, EpisodeNumber: 2, Title: "Finale", AirDate: "2020-01-08", Monitored: true},
			},
		}},
	}, nil
}

type env struct {
	db       *sqlite.DB
	lib      *library.Service
	sonarr   *httptest.Server
	radarr   *httptest.Server
	searches *atomic.Int64
	rootPath string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	b := bus.New(nil)
	t.Cleanup(b.Close)
	lib := library.New(db, fakeProvider{}, b, nil)

	rootPath := t.TempDir()
	if _, err := lib.AddRootFolder(ctx, rootPath, domain.KindMixed); err != nil {
		t.Fatal(err)
	}

	var searches atomic.Int64
	deps := compat.Deps{
		Library: lib, Store: db,
		APIKey: func(context.Context) string { return testKey },
		ResolveTVDB: func(ctx context.Context, tvdbID int64) (domain.MediaItem, error) {
			if tvdbID != 700700 {
				return domain.MediaItem{}, fmt.Errorf("unknown tvdb id %d", tvdbID)
			}
			return fakeProvider{}.GetSeries(ctx, 100)
		},
		TriggerSearch: func() { searches.Add(1) },
	}
	sonarr := httptest.NewServer(compat.NewSonarr(deps).Handler())
	t.Cleanup(sonarr.Close)
	radarr := httptest.NewServer(compat.NewRadarr(deps).Handler())
	t.Cleanup(radarr.Close)
	return &env{db: db, lib: lib, sonarr: sonarr, radarr: radarr, searches: &searches, rootPath: rootPath}
}

// call performs a request the way real consumers do (X-Api-Key header).
func call(t *testing.T, method, url string, body string) (*http.Response, []byte) {
	t.Helper()
	var req *http.Request
	var err error
	if body != "" {
		req, err = http.NewRequest(method, url, strings.NewReader(body))
	} else {
		req, err = http.NewRequest(method, url, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Api-Key", testKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, raw
}

func decode[T any](t *testing.T, raw []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("bad JSON: %v\n%s", err, raw)
	}
	return v
}

func TestAuthAndCaseInsensitivity(t *testing.T) {
	e := newEnv(t)

	// No key → 401, exactly like the real apps.
	resp, err := http.Get(e.sonarr.URL + "/api/v3/system/status")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no key = %d, want 401", resp.StatusCode)
	}
	// ?apikey= query form also accepted.
	resp, err = http.Get(e.sonarr.URL + "/api/v3/system/status?apikey=" + testKey)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("query key = %d, want 200", resp.StatusCode)
	}
	// Mixed-case path (Bazarr does this).
	resp, raw := call(t, "GET", e.sonarr.URL+"/API/V3/System/Status", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mixed case = %d", resp.StatusCode)
	}
	status := decode[map[string]any](t, raw)
	if status["appName"] != "Sonarr" || status["version"] != compat.SonarrVersion {
		t.Errorf("status = %v", status)
	}
}

func TestJellyseerrRadarrFlow(t *testing.T) {
	e := newEnv(t)
	base := e.radarr.URL + "/api/v3"

	// 1. Connection test: status + profiles + root folders.
	_, raw := call(t, "GET", base+"/system/status", "")
	if decode[map[string]any](t, raw)["appName"] != "Radarr" {
		t.Fatalf("appName: %s", raw)
	}
	_, raw = call(t, "GET", base+"/qualityprofile", "")
	profiles := decode[[]map[string]any](t, raw)
	if len(profiles) != 5 {
		t.Fatalf("profiles = %d", len(profiles))
	}
	_, raw = call(t, "GET", base+"/rootfolder", "")
	roots := decode[[]map[string]any](t, raw)
	if len(roots) != 1 || roots[0]["path"] != e.rootPath {
		t.Fatalf("roots = %s", raw)
	}

	// 2. Library snapshot (empty), then add movie by tmdbId.
	_, raw = call(t, "GET", base+"/movie", "")
	if len(decode[[]any](t, raw)) != 0 {
		t.Fatalf("initial movies: %s", raw)
	}
	addBody := fmt.Sprintf(`{"tmdbId":550,"title":"Fight Club","qualityProfileId":2,"rootFolderPath":%q,"monitored":true,"addOptions":{"searchForMovie":true}}`, e.rootPath)
	resp, raw := call(t, "POST", base+"/movie", addBody)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("add = %d: %s", resp.StatusCode, raw)
	}
	added := decode[map[string]any](t, raw)
	if added["tmdbId"].(float64) != 550 || added["monitored"] != true || added["hasFile"] != false {
		t.Errorf("added = %v", added)
	}
	if added["qualityProfileId"].(float64) != 2 {
		t.Errorf("profile = %v", added["qualityProfileId"])
	}

	// 3. Existence checks: list has it; duplicate add conflicts.
	_, raw = call(t, "GET", base+"/movie", "")
	if len(decode[[]any](t, raw)) != 1 {
		t.Fatalf("movies after add: %s", raw)
	}
	resp, _ = call(t, "POST", base+"/movie", addBody)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("dup add = %d, want 409", resp.StatusCode)
	}

	// 4. Search command acknowledged and routed.
	resp, _ = call(t, "POST", base+"/command", `{"name":"MoviesSearch","movieIds":[1]}`)
	if resp.StatusCode != http.StatusCreated || e.searches.Load() != 1 {
		t.Errorf("command = %d, searches = %d", resp.StatusCode, e.searches.Load())
	}
}

func TestJellyseerrSonarrFlow(t *testing.T) {
	e := newEnv(t)
	base := e.sonarr.URL + "/api/v3"

	// Jellyseerr looks the series up by tvdb id first…
	_, raw := call(t, "GET", base+"/series/lookup?term=tvdb:700700", "")
	found := decode[[]map[string]any](t, raw)
	if len(found) != 1 || found[0]["tvdbId"].(float64) != 700700 {
		t.Fatalf("lookup: %s", raw)
	}
	if found[0]["titleSlug"] != "test-show" {
		t.Errorf("slug = %v", found[0]["titleSlug"])
	}

	// …checks the language profiles exist…
	_, raw = call(t, "GET", base+"/languageprofile", "")
	if len(decode[[]any](t, raw)) != 1 {
		t.Fatalf("languageprofile: %s", raw)
	}

	// …then posts the add.
	addBody := fmt.Sprintf(`{"tvdbId":700700,"title":"Test Show","qualityProfileId":1,"languageProfileId":1,"rootFolderPath":%q,"monitored":true,"seasons":[{"seasonNumber":1,"monitored":true}]}`, e.rootPath)
	resp, raw := call(t, "POST", base+"/series", addBody)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("add = %d: %s", resp.StatusCode, raw)
	}
	added := decode[map[string]any](t, raw)
	seasons := added["seasons"].([]any)
	if len(seasons) != 1 {
		t.Fatalf("seasons = %v", seasons)
	}
	stats := added["statistics"].(map[string]any)
	if stats["episodeCount"].(float64) != 2 {
		t.Errorf("stats = %v", stats)
	}

	// Bazarr then enumerates series, episodes, and files.
	_, raw = call(t, "GET", base+"/series", "")
	series := decode[[]map[string]any](t, raw)
	if len(series) != 1 {
		t.Fatalf("series list: %s", raw)
	}
	id := int64(series[0]["id"].(float64))
	_, raw = call(t, "GET", fmt.Sprintf("%s/episode?seriesId=%d", base, id), "")
	episodes := decode[[]map[string]any](t, raw)
	if len(episodes) != 2 || episodes[0]["hasFile"] != false {
		t.Fatalf("episodes: %s", raw)
	}
	_, raw = call(t, "GET", fmt.Sprintf("%s/episodefile?seriesId=%d", base, id), "")
	if files := decode[[]any](t, raw); len(files) != 0 {
		t.Fatalf("files should be empty: %s", raw)
	}
}

func TestProwlarrIndexerSync(t *testing.T) {
	e := newEnv(t)
	base := e.sonarr.URL + "/api/v3"

	// Prowlarr discovers the Torznab schema…
	_, raw := call(t, "GET", base+"/indexer/schema", "")
	schema := decode[[]map[string]any](t, raw)
	hasTorznab := false
	for _, s := range schema {
		if s["implementation"] == "Torznab" {
			hasTorznab = true
		}
	}
	if !hasTorznab {
		t.Fatalf("schema lacks Torznab: %s", raw)
	}

	// …then pushes an indexer with field-styled settings.
	payload := `{"name":"Prowlarr Indexer (Prowlarr)","implementation":"Torznab","configContract":"TorznabSettings","protocol":"torrent","enableRss":true,
		"fields":[{"name":"baseUrl","value":"http://prowlarr:9696/1/"},{"name":"apiPath","value":"/api"},{"name":"apiKey","value":"pk-123"},{"name":"categories","value":[2000,5000]}]}`
	resp, raw := call(t, "POST", base+"/indexer", payload)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("push = %d: %s", resp.StatusCode, raw)
	}
	created := decode[map[string]any](t, raw)
	id := int64(created["id"].(float64))

	// The indexer landed in the native store, translated.
	native, err := e.db.ListIndexers(context.Background())
	if err != nil || len(native) != 1 {
		t.Fatalf("native indexers = %+v err %v", native, err)
	}
	if native[0].URL != "http://prowlarr:9696/1" || native[0].APIKey != "pk-123" ||
		len(native[0].Categories) != 2 || !native[0].Enabled {
		t.Errorf("translated = %+v", native[0])
	}

	// Second sync of the same indexer: no duplicate.
	resp, _ = call(t, "POST", base+"/indexer", payload)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("re-push = %d", resp.StatusCode)
	}
	native, _ = e.db.ListIndexers(context.Background())
	if len(native) != 1 {
		t.Fatalf("duplicated on re-sync: %+v", native)
	}

	// Update (Prowlarr PUTs on settings changes), then delete.
	upd := strings.Replace(payload, "pk-123", "pk-456", 1)
	resp, _ = call(t, "PUT", fmt.Sprintf("%s/indexer/%d", base, id), upd)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("update = %d", resp.StatusCode)
	}
	native, _ = e.db.ListIndexers(context.Background())
	if len(native) != 1 || native[0].APIKey != "pk-456" {
		t.Fatalf("after update = %+v", native)
	}
	resp, _ = call(t, "DELETE", fmt.Sprintf("%s/indexer/%d", base, native[0].ID), "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete = %d", resp.StatusCode)
	}
	native, _ = e.db.ListIndexers(context.Background())
	if len(native) != 0 {
		t.Fatalf("after delete = %+v", native)
	}
}

func TestUnknownRequestsLoggedNot500(t *testing.T) {
	e := newEnv(t)
	resp, raw := call(t, "GET", e.sonarr.URL+"/api/v3/importlist", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown = %d: %s", resp.StatusCode, raw)
	}
	msg := decode[map[string]any](t, raw)
	if !strings.Contains(msg["message"].(string), "monarr") {
		t.Errorf("message = %v", msg)
	}
}
