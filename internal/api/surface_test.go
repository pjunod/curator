package api

import (
	"net/http"
	"testing"
)

// Every collection endpoint answers on a freshly migrated, freshly wired
// server.
//
// This is deliberately shallow and deliberately broad. The per-area tests
// below check what each endpoint SAYS; this one checks that it answers at
// all — which is the failure a nil dependency actually produces, and the one
// that is invisible until someone opens that page. It caught three handlers
// that could not be reached from a test at all when it was written.
func TestEveryCollectionEndpointAnswers(t *testing.T) {
	e := newAPIEnv(t)
	e.addMovie(t)

	for _, path := range []string{
		"/api/v1/library",
		"/api/v1/rootfolders",
		"/api/v1/profiles",
		"/api/v1/indexers",
		"/api/v1/downloadclients",
		"/api/v1/queue",
		"/api/v1/wanted",
		"/api/v1/blocklist",
		"/api/v1/customformats",
		"/api/v1/importlists",
		"/api/v1/notifiers",
		"/api/v1/settings",
		"/api/v1/discover/lists",
		"/api/v1/system/connections",
		"/api/v1/system/tasks",
		"/api/v1/health",
		"/api/v1/system/status",
		// The calendar is the one that takes a required window rather than
		// defaulting to one, so it is spelled out here rather than omitted.
		"/api/v1/calendar?start=2026-01-01&end=2026-12-31",
	} {
		t.Run(path, func(t *testing.T) {
			e.get(t, path).expect(t, http.StatusOK)
		})
	}
}

// An empty collection serializes as [] and never as null: the web client
// maps over these directly, and `null.map` is a blank page with a console
// error rather than an empty state.
func TestEmptyCollectionsAreArraysNotNull(t *testing.T) {
	e := newAPIEnv(t)
	for _, path := range []string{
		"/api/v1/library",
		"/api/v1/indexers",
		"/api/v1/queue",
		"/api/v1/blocklist",
		"/api/v1/customformats",
		"/api/v1/importlists",
		"/api/v1/notifiers",
		"/api/v1/discover/lists",
	} {
		t.Run(path, func(t *testing.T) {
			body := e.get(t, path).expect(t, http.StatusOK).Body.String()
			if body == "null\n" || body == "null" {
				t.Errorf("%s returned null; the UI maps over this", path)
			}
		})
	}
}
