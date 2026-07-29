package compat_test

// Contract-shape drift guards: the exact JSON keys the Sonarr/Radarr v3
// personalities emit, pinned. Jellyseerr, Prowlarr, and Bazarr read these
// by NAME — renaming or dropping one breaks consumers silently, so any
// missing key fails here with its name. New keys are fine (consumers
// ignore extras); disappearing keys are not.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/pjunod/monarr/internal/domain/quality"
)

// requireKeys fails naming every pinned key absent from obj.
func requireKeys(t *testing.T, what string, obj map[string]any, keys ...string) {
	t.Helper()
	var missing []string
	for _, k := range keys {
		if _, ok := obj[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("%s lost contract keys %v — consumers read these by name.\ngot: %v", what, missing, obj)
	}
}

func TestContractSystemStatusShape(t *testing.T) {
	e := newEnv(t)
	for _, base := range []string{e.sonarr.URL, e.radarr.URL} {
		_, raw := call(t, "GET", base+"/api/v3/system/status", "")
		requireKeys(t, base+" system/status", decode[map[string]any](t, raw),
			"appName", "instanceName", "version", "buildTime", "isProduction",
			"urlBase", "runtimeVersion", "authentication")
	}
}

func TestContractSharedResourceShapes(t *testing.T) {
	e := newEnv(t)
	base := e.sonarr.URL + "/api/v3"

	_, raw := call(t, "GET", base+"/qualityprofile", "")
	profiles := decode[[]map[string]any](t, raw)
	if len(profiles) == 0 {
		t.Fatal("no profiles")
	}
	requireKeys(t, "qualityprofile", profiles[0], "id", "name", "upgradeAllowed", "cutoff", "items")
	items := profiles[0]["items"].([]any)
	if len(items) == 0 {
		t.Fatal("profile has no items")
	}
	requireKeys(t, "qualityprofile item", items[0].(map[string]any), "quality", "allowed")
	requireKeys(t, "qualityprofile item quality", items[0].(map[string]any)["quality"].(map[string]any), "id", "name")

	_, raw = call(t, "GET", base+"/rootfolder", "")
	roots := decode[[]map[string]any](t, raw)
	requireKeys(t, "rootfolder", roots[0], "id", "path", "accessible", "freeSpace", "unmappedFolders")

	_, raw = call(t, "GET", base+"/languageprofile", "")
	langs := decode[[]map[string]any](t, raw)
	requireKeys(t, "languageprofile", langs[0], "id", "name", "upgradeAllowed")
}

func TestContractRadarrMovieShapes(t *testing.T) {
	e := newEnv(t)
	base := e.radarr.URL + "/api/v3"

	addBody := fmt.Sprintf(`{"tmdbId":550,"title":"Fight Club","qualityProfileId":2,"rootFolderPath":%q,"monitored":true}`, e.rootPath)
	resp, raw := call(t, "POST", base+"/movie", addBody)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("add = %d: %s", resp.StatusCode, raw)
	}
	movie := decode[map[string]any](t, raw)
	movieKeys := []string{
		"id", "title", "sortTitle", "sizeOnDisk", "status", "overview", "year",
		"hasFile", "path", "qualityProfileId", "monitored", "minimumAvailability",
		"isAvailable", "runtime", "cleanTitle", "imdbId", "tmdbId", "titleSlug",
		"rootFolderPath", "genres", "tags", "added", "images",
	}
	requireKeys(t, "movie (add response)", movie, movieKeys...)

	// The list emits the same shape (Jellyseerr existence checks read it).
	_, raw = call(t, "GET", base+"/movie", "")
	listed := decode[[]map[string]any](t, raw)
	if len(listed) != 1 {
		t.Fatalf("movies = %s", raw)
	}
	requireKeys(t, "movie (list)", listed[0], movieKeys...)

	// Lookup shape (tmdb: term) — Jellyseerr matches on tmdbId.
	_, raw = call(t, "GET", base+"/movie/lookup?term=tmdb:550", "")
	lookup := decode[[]map[string]any](t, raw)
	if len(lookup) == 0 {
		t.Fatal("lookup empty")
	}
	requireKeys(t, "movie lookup", lookup[0], "tmdbId", "title", "titleSlug", "year")

	// Movie files (Bazarr): give the movie a file, then pin the shape.
	itemID := int64(movie["id"].(float64))
	moviePath := movie["path"].(string)
	if err := os.MkdirAll(moviePath, 0o755); err != nil {
		t.Fatal(err)
	}
	fpath := filepath.Join(moviePath, "Fight Club (1999) [WEBDL-1080p].mkv")
	if err := os.WriteFile(fpath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	fid, err := e.db.UpsertFile(context.Background(), itemID, 0, fpath, 1)
	if err != nil {
		t.Fatal(err)
	}
	_ = e.db.SetFileQuality(context.Background(), fid, quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080})

	_, raw = call(t, "GET", fmt.Sprintf("%s/moviefile?movieId=%d", base, itemID), "")
	files := decode[[]map[string]any](t, raw)
	if len(files) != 1 {
		t.Fatalf("moviefiles = %s", raw)
	}
	requireKeys(t, "moviefile", files[0], "id", "movieId", "relativePath", "path", "size", "quality")
	requireKeys(t, "moviefile quality", files[0]["quality"].(map[string]any), "quality")
}

func TestContractSonarrSeriesShapes(t *testing.T) {
	e := newEnv(t)
	base := e.sonarr.URL + "/api/v3"

	addBody := fmt.Sprintf(`{"tvdbId":700700,"title":"Test Show","qualityProfileId":1,"languageProfileId":1,"rootFolderPath":%q,"monitored":true,"seasons":[{"seasonNumber":1,"monitored":true}]}`, e.rootPath)
	resp, raw := call(t, "POST", base+"/series", addBody)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("add = %d: %s", resp.StatusCode, raw)
	}
	series := decode[map[string]any](t, raw)
	seriesKeys := []string{
		"id", "title", "sortTitle", "status", "ended", "overview", "seasons",
		"year", "path", "qualityProfileId", "languageProfileId", "seasonFolder",
		"monitored", "runtime", "tvdbId", "seriesType", "imdbId", "titleSlug",
		"rootFolderPath", "genres", "tags", "added", "firstAired", "images", "statistics",
	}
	requireKeys(t, "series (add response)", series, seriesKeys...)
	requireKeys(t, "series statistics", series["statistics"].(map[string]any),
		"seasonCount", "episodeCount", "episodeFileCount", "totalEpisodeCount",
		"sizeOnDisk", "percentOfEpisodes")
	seasons := series["seasons"].([]any)
	if len(seasons) == 0 {
		t.Fatal("no seasons")
	}
	season := seasons[0].(map[string]any)
	requireKeys(t, "season", season, "seasonNumber", "monitored", "statistics")
	requireKeys(t, "season statistics", season["statistics"].(map[string]any),
		"episodeCount", "episodeFileCount", "totalEpisodeCount", "percentOfEpisodes")

	// Episodes (Bazarr enumerates every field here by name).
	id := int64(series["id"].(float64))
	_, raw = call(t, "GET", fmt.Sprintf("%s/episode?seriesId=%d", base, id), "")
	episodes := decode[[]map[string]any](t, raw)
	if len(episodes) == 0 {
		t.Fatal("no episodes")
	}
	requireKeys(t, "episode", episodes[0],
		"id", "seriesId", "seasonNumber", "episodeNumber", "title", "airDate",
		"hasFile", "monitored", "episodeFileId", "absoluteEpisodeNumber")

	// Episode files: link one, pin the shape.
	seriesPath := series["path"].(string)
	if err := os.MkdirAll(seriesPath, 0o755); err != nil {
		t.Fatal(err)
	}
	fpath := filepath.Join(seriesPath, "Test Show - S01E01 - Pilot [HDTV-720p].mkv")
	if err := os.WriteFile(fpath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	fid, err := e.db.UpsertFile(ctx, id, 0, fpath, 1)
	if err != nil {
		t.Fatal(err)
	}
	epID, err := e.db.GetEpisodeID(ctx, id, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.db.ReplaceFileEpisodeLinks(ctx, fid, []int64{epID}); err != nil {
		t.Fatal(err)
	}
	_ = e.db.SetFileQuality(ctx, fid, quality.Quality{Source: quality.SourceHDTV, Resolution: 720})

	_, raw = call(t, "GET", fmt.Sprintf("%s/episodefile?seriesId=%d", base, id), "")
	files := decode[[]map[string]any](t, raw)
	if len(files) != 1 {
		t.Fatalf("episodefiles = %s", raw)
	}
	requireKeys(t, "episodefile", files[0], "id", "seriesId", "relativePath", "path", "size", "quality")

	// Series lookup by tvdb term (the Jellyseerr entry point).
	_, raw = call(t, "GET", base+"/series/lookup?term=tvdb:700700", "")
	lookup := decode[[]map[string]any](t, raw)
	if len(lookup) == 0 {
		t.Fatal("lookup empty")
	}
	requireKeys(t, "series lookup", lookup[0], "tvdbId", "title", "titleSlug", "year", "seasons")
}

func TestContractCommandAndPaging(t *testing.T) {
	e := newEnv(t)
	base := e.radarr.URL + "/api/v3"

	// Command acknowledgements: consumers poll id/status by name.
	resp, raw := call(t, "POST", base+"/command", `{"name":"RefreshMovie"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("command = %d", resp.StatusCode)
	}
	requireKeys(t, "command", decode[map[string]any](t, raw), "id", "name", "commandName", "status")

	// History paging envelope (Bazarr reads records/totalRecords).
	_, raw = call(t, "GET", base+"/history?page=1", "")
	requireKeys(t, "history", decode[map[string]any](t, raw),
		"page", "pageSize", "sortKey", "sortDirection", "totalRecords", "records")
}
