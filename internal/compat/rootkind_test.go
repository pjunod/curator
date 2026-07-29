package compat_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/pjunod/monarr/internal/domain"
)

// Before ADR 0009 both personalities were shown every root, so a Radarr
// client could pick the TV root as a movie destination and Monarr would
// accept the write. Both halves are checked here: the list is filtered, and
// the write is refused even when the client supplies the path anyway.
func TestRootFoldersAreFilteredByPersonality(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()

	movieRoot := filepath.Join(t.TempDir(), "movies")
	seriesRoot := filepath.Join(t.TempDir(), "tv")
	for path, kind := range map[string]domain.RootKind{
		movieRoot:  domain.RootKindOf(domain.KindMovie),
		seriesRoot: domain.RootKindOf(domain.KindSeries),
	} {
		if err := mkdir(path); err != nil {
			t.Fatal(err)
		}
		if _, err := env.db.AddRootFolder(ctx, path, kind); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct {
		name     string
		server   string
		wants    string
		excludes string
	}{
		{"radarr sees movie roots", env.radarr.URL, movieRoot, seriesRoot},
		{"sonarr sees series roots", env.sonarr.URL, seriesRoot, movieRoot},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, raw := call(t, "GET", tc.server+"/api/v3/rootfolder", "")
			roots := decode[[]map[string]any](t, raw)
			var paths []string
			for _, rf := range roots {
				paths = append(paths, fmt.Sprint(rf["path"]))
			}
			if !contains(paths, tc.wants) {
				t.Errorf("want %s in %v", tc.wants, paths)
			}
			if contains(paths, tc.excludes) {
				t.Errorf("%s must not be offered: %v", tc.excludes, paths)
			}
			// The mixed root from newEnv restricts nothing, so both see it.
			if !contains(paths, env.rootPath) {
				t.Errorf("a mixed root must be offered to both, missing from %v", paths)
			}
		})
	}
}

// Filtering the list is the affordance; refusing the write is the guarantee.
func TestAddRefusesRootOfTheWrongKind(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()

	seriesRoot := filepath.Join(t.TempDir(), "tv")
	if err := mkdir(seriesRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.AddRootFolder(ctx, seriesRoot, domain.RootKindOf(domain.KindSeries)); err != nil {
		t.Fatal(err)
	}

	body := fmt.Sprintf(`{"tmdbId":550,"qualityProfileId":1,"rootFolderPath":%q,"monitored":true}`, seriesRoot)
	resp, raw := call(t, "POST", env.radarr.URL+"/api/v3/movie", body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 filing a movie into a series root: %s", resp.StatusCode, raw)
	}
}

// A mixed root still accepts either personality, so nothing an existing
// install does today starts failing.
func TestAddIntoMixedRootStillWorks(t *testing.T) {
	env := newEnv(t)
	body := fmt.Sprintf(`{"tmdbId":550,"qualityProfileId":1,"rootFolderPath":%q,"monitored":true}`, env.rootPath)
	resp, raw := call(t, "POST", env.radarr.URL+"/api/v3/movie", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 into a mixed root: %s", resp.StatusCode, raw)
	}
}

func mkdir(p string) error { return os.MkdirAll(p, 0o755) }

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
