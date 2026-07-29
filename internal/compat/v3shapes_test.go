package compat_test

// The v3 shapes real consumers depend on, beyond the happy-path sync flows
// in compat_test.go: single-item GETs, lookup by text, the file-bearing
// series/movie (posters, sizeOnDisk, per-file quality), and the error
// answers a consumer must be able to tell apart from a success.
//
// A translation shim's failure mode is not a crash — it is answering 200
// with a shape the consumer silently misreads. So these assert the fields
// Jellyseerr, Bazarr and Prowlarr actually read, not just the status code.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/monarr-media/monarr/internal/compat"

	"github.com/monarr-media/monarr/internal/domain/mediainfo"
	"github.com/monarr-media/monarr/internal/domain/quality"
	"github.com/monarr-media/monarr/internal/ports"
)

// newEnvNoTVDB is newEnv's Sonarr half with ResolveTVDB left unwired, which
// is what a deployment without a TVDB mapping source looks like.
func newEnvNoTVDB(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	deps := compat.Deps{
		Library: e.lib, Store: e.db,
		APIKey: func(context.Context) string { return testKey },
	}
	srv := httptest.NewServer(compat.NewSonarr(deps).Handler())
	t.Cleanup(srv.Close)
	e.sonarr = srv
	return e
}

// seedSeriesWithFile adds the fake provider's series through the personality
// and attaches one episode file, so the DTO has something to report for
// sizeOnDisk, images and episodeFileId.
func seedSeriesWithFile(t *testing.T, e *env) (itemID, fileID int64) {
	t.Helper()
	ctx := context.Background()

	body := fmt.Sprintf(`{"tvdbId":700700,"qualityProfileId":1,"rootFolderPath":%q,"monitored":true}`, e.rootPath)
	resp, raw := call(t, "POST", e.sonarr.URL+"/api/v3/series", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("seed add = %d: %s", resp.StatusCode, raw)
	}
	itemID = int64(decode[map[string]any](t, raw)["id"].(float64))

	full, err := e.db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Seasons) == 0 || len(full.Seasons[0].Episodes) == 0 {
		t.Fatal("seeded series has no episodes to attach a file to")
	}
	epID := full.Seasons[0].Episodes[0].ID

	fileID, err = e.db.UpsertFile(ctx, itemID, 0, full.Path+"/Season 01/pilot.mkv", 1234567)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.db.ReplaceFileEpisodeLinks(ctx, fileID, []int64{epID}); err != nil {
		t.Fatal(err)
	}
	if err := e.db.SetFileQualityFrom(ctx, fileID,
		quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080},
		mediainfo.ProvenanceProbe, mediainfo.ConfidenceHigh); err != nil {
		t.Fatal(err)
	}
	return itemID, fileID
}

// Jellyseerr and Bazarr both fetch a single series by id after a list. A 404
// here has to be a 404 — returning 200 with a zero-valued series would make
// a consumer believe an unknown id exists.
func TestCompat2GetSeriesByID(t *testing.T) {
	e := newEnv(t)
	id, fileID := seedSeriesWithFile(t, e)
	base := e.sonarr.URL + "/api/v3"

	_, raw := call(t, "GET", fmt.Sprintf("%s/series/%d", base, id), "")
	got := decode[map[string]any](t, raw)
	if int64(got["id"].(float64)) != id {
		t.Fatalf("id = %v, want %d", got["id"], id)
	}
	if got["title"] != "Test Show" {
		t.Errorf("title = %v", got["title"])
	}
	// Ended series report status "ended"; Jellyseerr uses this to decide
	// whether to keep asking for new seasons.
	if got["status"] != "ended" || got["ended"] != true {
		t.Errorf("status/ended = %v/%v, want ended/true", got["status"], got["ended"])
	}
	if got["rootFolderPath"] != e.rootPath {
		t.Errorf("rootFolderPath = %v, want %s", got["rootFolderPath"], e.rootPath)
	}
	stats := got["statistics"].(map[string]any)
	if stats["sizeOnDisk"].(float64) != 1234567 {
		t.Errorf("sizeOnDisk = %v, want the attached file's size", stats["sizeOnDisk"])
	}
	// One of two episodes has a file: 50%, and the percentage is what the
	// consumer draws its progress bar from.
	if stats["percentOfEpisodes"].(float64) != 50 {
		t.Errorf("percentOfEpisodes = %v, want 50", stats["percentOfEpisodes"])
	}

	// The episode list carries the covering file id; Bazarr joins on it.
	_, raw = call(t, "GET", fmt.Sprintf("%s/episode?seriesId=%d", base, id), "")
	eps := decode[[]map[string]any](t, raw)
	if len(eps) != 2 {
		t.Fatalf("episodes = %d, want 2", len(eps))
	}
	linked, unlinked := 0, 0
	for _, ep := range eps {
		if int64(ep["episodeFileId"].(float64)) == fileID {
			linked++
		}
		if ep["episodeFileId"].(float64) == 0 {
			unlinked++
		}
	}
	if linked != 1 || unlinked != 1 {
		t.Errorf("episodeFileId mapping = %d linked / %d unlinked, want 1/1", linked, unlinked)
	}

	// A series id that does not exist, and a movie-shaped id asked for on
	// the series endpoint, both have to be 404 rather than a hollow DTO.
	resp, _ := call(t, "GET", base+"/series/99999", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown series = %d, want 404", resp.StatusCode)
	}
	resp, _ = call(t, "GET", base+"/series/notanumber", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("non-numeric series id = %d, want 404", resp.StatusCode)
	}
}

// Bazarr discovers subtitles by listing episode files and reading the
// quality name off each. "Unknown" is the fallback, and a probed file must
// not still say Unknown.
func TestCompat2ListEpisodeFilesCarriesQuality(t *testing.T) {
	e := newEnv(t)
	id, fileID := seedSeriesWithFile(t, e)

	_, raw := call(t, "GET", fmt.Sprintf("%s/api/v3/episodefile?seriesId=%d", e.sonarr.URL, id), "")
	files := decode[[]map[string]any](t, raw)
	if len(files) != 1 {
		t.Fatalf("episodefile = %d entries, want 1: %s", len(files), raw)
	}
	f := files[0]
	if int64(f["id"].(float64)) != fileID {
		t.Errorf("id = %v, want %d", f["id"], fileID)
	}
	// relativePath is path-minus-item-path with no leading slash; Bazarr
	// resolves sidecar subtitle names against it.
	if f["relativePath"] != "Season 01/pilot.mkv" {
		t.Errorf("relativePath = %v, want Season 01/pilot.mkv", f["relativePath"])
	}
	if f["size"].(float64) != 1234567 {
		t.Errorf("size = %v", f["size"])
	}
	name := f["quality"].(map[string]any)["quality"].(map[string]any)["name"]
	if name == "Unknown" || name == "" {
		t.Errorf("quality name = %v, want the stored quality", name)
	}

	// Unknown series id: 404 on both file endpoints, not an empty list that
	// a consumer would read as "no subtitles needed".
	resp, _ := call(t, "GET", e.sonarr.URL+"/api/v3/episodefile?seriesId=424242", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("episodefile for unknown series = %d, want 404", resp.StatusCode)
	}
	resp, _ = call(t, "GET", e.sonarr.URL+"/api/v3/episode?seriesId=424242", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("episode for unknown series = %d, want 404", resp.StatusCode)
	}
}

// Radarr's single-movie GET plus the tmdbId list filter Jellyseerr uses for
// existence checks. The filter is the interesting part: a wrong answer here
// means Jellyseerr either re-requests something we already have or reports
// a movie as present that isn't.
func TestCompat2GetMovieAndTMDBFilter(t *testing.T) {
	e := newEnv(t)
	base := e.radarr.URL + "/api/v3"

	body := fmt.Sprintf(`{"tmdbId":550,"qualityProfileId":1,"rootFolderPath":%q,"monitored":true}`, e.rootPath)
	resp, raw := call(t, "POST", base+"/movie", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("add = %d: %s", resp.StatusCode, raw)
	}
	id := int64(decode[map[string]any](t, raw)["id"].(float64))

	_, raw = call(t, "GET", fmt.Sprintf("%s/movie/%d", base, id), "")
	got := decode[map[string]any](t, raw)
	if got["tmdbId"].(float64) != 550 || got["title"] != "Fight Club" {
		t.Errorf("movie = %v", got)
	}
	// The fake provider reports Status "Released", so the v3 status must be
	// "released" and not the "announced" fallback.
	if got["status"] != "released" {
		t.Errorf("status = %v, want released", got["status"])
	}
	if got["titleSlug"] != "550" {
		t.Errorf("titleSlug = %v, want the tmdb id", got["titleSlug"])
	}

	// Matching filter returns it; non-matching returns an empty list rather
	// than everything.
	_, raw = call(t, "GET", base+"/movie?tmdbId=550", "")
	if len(decode[[]any](t, raw)) != 1 {
		t.Errorf("tmdbId=550 filter: %s", raw)
	}
	_, raw = call(t, "GET", base+"/movie?tmdbid=550", "")
	if len(decode[[]any](t, raw)) != 1 {
		t.Errorf("lowercase tmdbid filter: %s", raw)
	}
	_, raw = call(t, "GET", base+"/movie?tmdbId=999", "")
	if len(decode[[]any](t, raw)) != 0 {
		t.Errorf("tmdbId=999 should match nothing: %s", raw)
	}

	// Asking Radarr for a series id is a 404: the personalities do not leak
	// each other's kinds.
	seriesBody := fmt.Sprintf(`{"tvdbId":700700,"qualityProfileId":1,"rootFolderPath":%q}`, e.rootPath)
	_, raw = call(t, "POST", e.sonarr.URL+"/api/v3/series", seriesBody)
	seriesID := int64(decode[map[string]any](t, raw)["id"].(float64))
	resp, _ = call(t, "GET", fmt.Sprintf("%s/movie/%d", base, seriesID), "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("series id on /movie = %d, want 404", resp.StatusCode)
	}
	resp, _ = call(t, "GET", fmt.Sprintf("%s/api/v3/series/%d", e.sonarr.URL, id), "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("movie id on /series = %d, want 404", resp.StatusCode)
	}
}

// Bazarr's movie file discovery, including the "no quality recorded yet"
// fallback that must read Unknown rather than an empty string.
func TestCompat2ListMovieFiles(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	base := e.radarr.URL + "/api/v3"

	body := fmt.Sprintf(`{"tmdbId":550,"qualityProfileId":1,"rootFolderPath":%q}`, e.rootPath)
	_, raw := call(t, "POST", base+"/movie", body)
	id := int64(decode[map[string]any](t, raw)["id"].(float64))

	full, err := e.db.GetMediaItemFull(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.UpsertFile(ctx, id, 0, full.Path+"/Fight Club.mkv", 999); err != nil {
		t.Fatal(err)
	}

	_, raw = call(t, "GET", fmt.Sprintf("%s/moviefile?movieId=%d", base, id), "")
	files := decode[[]map[string]any](t, raw)
	if len(files) != 1 {
		t.Fatalf("moviefile = %s", raw)
	}
	if files[0]["relativePath"] != "Fight Club.mkv" {
		t.Errorf("relativePath = %v", files[0]["relativePath"])
	}
	name := files[0]["quality"].(map[string]any)["quality"].(map[string]any)["name"]
	if name != "Unknown" {
		t.Errorf("unprobed quality = %v, want Unknown", name)
	}
	// The lowercase spelling Bazarr sometimes sends resolves to the same
	// movie.
	_, raw = call(t, "GET", fmt.Sprintf("%s/moviefile?movieid=%d", base, id), "")
	if len(decode[[]any](t, raw)) != 1 {
		t.Errorf("lowercase movieid: %s", raw)
	}

	// hasFile flips once a file exists — Jellyseerr shows "available" off it.
	_, raw = call(t, "GET", fmt.Sprintf("%s/movie/%d", base, id), "")
	if decode[map[string]any](t, raw)["hasFile"] != true {
		t.Errorf("hasFile still false with a file attached: %s", raw)
	}

	resp, _ := call(t, "GET", base+"/moviefile?movieId=424242", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("moviefile for unknown movie = %d, want 404", resp.StatusCode)
	}
}

// Text lookup is the other half of the add flow (the tvdb:/tmdb: forms are
// covered in compat_test.go). Consumers render remotePoster and titleSlug
// straight from these results.
func TestCompat2LookupByText(t *testing.T) {
	e := newEnv(t)

	_, raw := call(t, "GET", e.sonarr.URL+"/api/v3/series/lookup?term=test+show", "")
	series := decode[[]map[string]any](t, raw)
	if len(series) != 1 {
		t.Fatalf("series lookup: %s", raw)
	}
	if series[0]["title"] != "Test Show" || series[0]["titleSlug"] != "test-show" {
		t.Errorf("series result = %v", series[0])
	}
	if series[0]["seasons"] == nil {
		t.Error("seasons must be an empty array, not null — consumers range over it")
	}

	_, raw = call(t, "GET", e.radarr.URL+"/api/v3/movie/lookup?term=fight+club", "")
	movies := decode[[]map[string]any](t, raw)
	if len(movies) != 1 {
		t.Fatalf("movie lookup: %s", raw)
	}
	if movies[0]["tmdbId"].(float64) != 550 || movies[0]["titleSlug"] != "550" {
		t.Errorf("movie result = %v", movies[0])
	}

	// "tmdb:603" is Jellyseerr's by-id form: a minimal result keyed on the
	// id, never an error, because Jellyseerr treats a non-200 as "Radarr is
	// down".
	_, raw = call(t, "GET", e.radarr.URL+"/api/v3/movie/lookup?term=tmdb:603", "")
	byID := decode[[]map[string]any](t, raw)
	if len(byID) != 1 || byID[0]["tmdbId"].(float64) != 603 {
		t.Errorf("tmdb: lookup = %s", raw)
	}

	// An unresolvable tvdb id yields an empty list, not a 500 — same reason.
	resp, raw := call(t, "GET", e.sonarr.URL+"/api/v3/series/lookup?term=tvdb:1", "")
	if resp.StatusCode != http.StatusOK || len(decode[[]any](t, raw)) != 0 {
		t.Errorf("unknown tvdb id = %d: %s", resp.StatusCode, raw)
	}
}

// The add endpoints' rejection paths. Each one has a distinct status because
// consumers branch on it: 400 is "your request is wrong", 409 is "already
// there, stop asking".
func TestCompat2AddRejections(t *testing.T) {
	e := newEnv(t)

	for _, tc := range []struct {
		name string
		url  string
		body string
		want int
	}{
		{"series: malformed JSON", e.sonarr.URL + "/api/v3/series", `{`, http.StatusBadRequest},
		{"series: no tvdbId", e.sonarr.URL + "/api/v3/series", `{"title":"x"}`, http.StatusBadRequest},
		{"series: unresolvable tvdbId", e.sonarr.URL + "/api/v3/series", `{"tvdbId":4242}`, http.StatusBadRequest},
		{"movie: malformed JSON", e.radarr.URL + "/api/v3/movie", `not json`, http.StatusBadRequest},
		{"movie: no tmdbId", e.radarr.URL + "/api/v3/movie", `{"title":"x"}`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, raw := call(t, "POST", tc.url, tc.body)
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d: %s", resp.StatusCode, tc.want, raw)
			}
			if len(raw) == 0 {
				t.Error("rejection had an empty body; consumers surface the message to the user")
			}
		})
	}

	// A rootFolderPath nobody knows is not an error — it resolves to "no
	// root", which is the pre-existing behaviour a client relies on when it
	// was configured against a path we since renamed.
	body := `{"tvdbId":700700,"qualityProfileId":1,"rootFolderPath":"/nowhere"}`
	resp, raw := call(t, "POST", e.sonarr.URL+"/api/v3/series", body)
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("unknown root path = %d, want 201: %s", resp.StatusCode, raw)
	}

	// Adding the same series twice is a conflict, matching the movie flow.
	resp, _ = call(t, "POST", e.sonarr.URL+"/api/v3/series", body)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("duplicate series add = %d, want 409", resp.StatusCode)
	}
}

// Sonarr's add and lookup both go through ResolveTVDB. When it is not wired
// the personality has to say so, not panic on a nil func.
func TestCompat2SonarrWithoutTVDBResolution(t *testing.T) {
	e := newEnvNoTVDB(t)

	resp, raw := call(t, "POST", e.sonarr.URL+"/api/v3/series", `{"tvdbId":700700}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("add without resolution = %d, want 400: %s", resp.StatusCode, raw)
	}
	// Lookup degrades to "found nothing" instead, because Jellyseerr reads
	// a non-200 as the whole server being unreachable.
	resp, raw = call(t, "GET", e.sonarr.URL+"/api/v3/series/lookup?term=tvdb:700700", "")
	if resp.StatusCode != http.StatusOK || len(decode[[]any](t, raw)) != 0 {
		t.Errorf("lookup without resolution = %d: %s", resp.StatusCode, raw)
	}
}

// Prowlarr lists the indexers it pushed to confirm the sync landed, and PUTs
// or DELETEs by id. An id that is not there must 404 rather than silently
// creating a second config.
func TestCompat2IndexerListAndMissingID(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	base := e.sonarr.URL + "/api/v3"

	// Empty first: the list has to be [] and not null.
	_, raw := call(t, "GET", base+"/indexer", "")
	if got := decode[[]map[string]any](t, raw); got == nil || len(got) != 0 {
		t.Fatalf("empty indexer list = %s", raw)
	}

	id, err := e.db.AddIndexer(ctx, ports.IndexerConfig{
		Name: "Seeded", URL: "http://idx:9117", APIKey: "k", Protocol: "torrent",
		Enabled: true, Categories: []int{5000},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, raw = call(t, "GET", base+"/indexer", "")
	list := decode[[]map[string]any](t, raw)
	if len(list) != 1 {
		t.Fatalf("indexer list = %s", raw)
	}
	got := list[0]
	if int64(got["id"].(float64)) != id || got["name"] != "Seeded" {
		t.Errorf("indexer = %v", got)
	}
	// Prowlarr keys off implementation/configContract to decide the settings
	// form; getting these wrong makes the app un-syncable.
	if got["implementation"] != "Torznab" || got["configContract"] != "TorznabSettings" {
		t.Errorf("implementation shape = %v", got)
	}
	if got["enableRss"] != true || got["enableAutomaticSearch"] != true {
		t.Errorf("enabled flags = %v", got)
	}
	fields := got["fields"].([]any)
	byName := map[string]any{}
	for _, f := range fields {
		m := f.(map[string]any)
		byName[m["name"].(string)] = m["value"]
	}
	if byName["baseUrl"] != "http://idx:9117" || byName["apiKey"] != "k" {
		t.Errorf("fields = %v", byName)
	}
	if cats, ok := byName["categories"].([]any); !ok || len(cats) != 1 {
		t.Errorf("categories field = %v", byName["categories"])
	}

	// Unknown ids.
	resp, _ := call(t, "PUT", base+"/indexer/9999", `{"name":"x","fields":[]}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("PUT unknown indexer = %d, want 404", resp.StatusCode)
	}
	// DELETE of an id that is not there is idempotent: the native store's
	// delete is a no-op on a missing row, so the handler's "indexer not
	// found" branch never fires. That is fine for Prowlarr — it retries
	// deletes — but it does mean the 404 the handler advertises is
	// unreachable through the sqlite store, so pin the behaviour that
	// actually happens rather than the one the code implies.
	resp, _ = call(t, "DELETE", base+"/indexer/9999", "")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("DELETE unknown indexer = %d, want an idempotent 200", resp.StatusCode)
	}
	if after, _ := e.db.ListIndexers(ctx); len(after) != 1 {
		t.Errorf("deleting an unknown id removed a real indexer: %+v", after)
	}
	// A malformed update body must not destroy the existing config: the
	// delete-and-recreate only starts after the body parses.
	resp, _ = call(t, "PUT", fmt.Sprintf("%s/indexer/%d", base, id), `{`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("PUT malformed body = %d, want 400", resp.StatusCode)
	}
	if after, _ := e.db.ListIndexers(ctx); len(after) != 1 {
		t.Errorf("a malformed update destroyed the indexer: %+v", after)
	}
	resp, _ = call(t, "POST", base+"/indexer", `{`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST malformed body = %d, want 400", resp.StatusCode)
	}
}

// Newznab configs come through the same endpoint and must be routed as
// usenet; anything else is clamped to torrent so grab routing has exactly
// two cases to handle.
func TestCompat2IndexerProtocolTranslation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	base := e.radarr.URL + "/api/v3"

	for _, tc := range []struct {
		name    string
		payload string
		want    string
		wantURL string
		enabled bool
	}{
		{
			name: "newznab is usenet",
			payload: `{"name":"NZB","implementation":"Newznab","protocol":"usenet","enableRss":true,
				"fields":[{"name":"baseUrl","value":"http://nzb:8080/"},{"name":"apiKey","value":"n1"}]}`,
			want: "usenet", wantURL: "http://nzb:8080", enabled: true,
		},
		{
			name: "an unrecognised protocol is clamped to torrent",
			payload: `{"name":"Odd","implementation":"Torznab","protocol":"carrier-pigeon","enableRss":false,
				"fields":[{"name":"baseUrl","value":"http://odd:9117"},{"name":"apiKey","value":"o1"}]}`,
			want: "torrent", wantURL: "http://odd:9117", enabled: false,
		},
		{
			// A non-default apiPath is folded into the URL because our
			// torznab adapter appends /api itself.
			name: "non-default apiPath folds into the URL",
			payload: `{"name":"Sub","implementation":"Torznab","protocol":"torrent",
				"fields":[{"name":"baseUrl","value":"http://sub:9117"},{"name":"apiPath","value":"/prefix/api"}]}`,
			want: "torrent", wantURL: "http://sub:9117/prefix", enabled: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, raw := call(t, "POST", base+"/indexer", tc.payload)
			if resp.StatusCode != http.StatusCreated {
				t.Fatalf("push = %d: %s", resp.StatusCode, raw)
			}
			created := decode[map[string]any](t, raw)
			if created["protocol"] != tc.want {
				t.Errorf("protocol = %v, want %s", created["protocol"], tc.want)
			}
			id := int64(created["id"].(float64))
			cfg, err := e.db.GetIndexer(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.URL != tc.wantURL {
				t.Errorf("stored URL = %q, want %q", cfg.URL, tc.wantURL)
			}
			if cfg.Enabled != tc.enabled {
				t.Errorf("enabled = %v, want %v", cfg.Enabled, tc.enabled)
			}
		})
	}
}

// The shared surface consumers poll before doing anything real. These are
// list endpoints where "null" instead of "[]" is a client-side crash.
func TestCompat2SharedListEndpointsAreArrays(t *testing.T) {
	e := newEnv(t)

	for _, path := range []string{"/api/v3/health", "/api/v3/tag"} {
		resp, raw := call(t, "GET", e.sonarr.URL+path, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s = %d", path, resp.StatusCode)
		}
		got := decode[[]any](t, raw)
		if got == nil {
			t.Errorf("%s returned null, want []", path)
		}
		if len(got) != 0 {
			t.Errorf("%s = %s, want empty", path, raw)
		}
	}

	// queue and history are paged envelopes, not bare arrays.
	for _, path := range []string{"/api/v3/queue", "/api/v3/history"} {
		_, raw := call(t, "GET", e.sonarr.URL+path, "")
		env := decode[map[string]any](t, raw)
		if _, ok := env["records"]; !ok {
			t.Errorf("%s = %s, want a paged envelope with records", path, raw)
		}
	}

	// Radarr has no language profiles; asking for them is an unknown route,
	// not a Sonarr-shaped answer.
	resp, _ := call(t, "GET", e.radarr.URL+"/api/v3/languageprofile", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("radarr languageprofile = %d, want 404", resp.StatusCode)
	}
}

// A command name we do not route must still be acknowledged: consumers poll
// the command id afterwards and treat a failure as the server being broken.
func TestCompat2UnroutedCommandStillAccepted(t *testing.T) {
	e := newEnv(t)
	before := e.searches.Load()

	resp, raw := call(t, "POST", e.sonarr.URL+"/api/v3/command", `{"name":"RefreshMonitoredDownloads"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("command = %d: %s", resp.StatusCode, raw)
	}
	if e.searches.Load() != before {
		t.Error("a non-search command triggered a backlog search")
	}
	body := decode[map[string]any](t, raw)
	if body["name"] != "RefreshMonitoredDownloads" || body["status"] == nil {
		t.Errorf("command echo = %v", body)
	}
}
