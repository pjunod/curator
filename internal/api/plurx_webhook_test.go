package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

func watchServer(t *testing.T) (*Server, *sqlite.DB) {
	t.Helper()
	db, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return &Server{deps: Deps{Store: db, Log: slog.New(slog.DiscardHandler)}}, db
}

func postWatched(t *testing.T, srv *Server, body map[string]any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", "/api/v1/webhooks/plurx", bytes.NewReader(raw))
	rr := httptest.NewRecorder()
	srv.PlurxWebhook(rr, req)
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out
}

func TestAWatchedNotificationIsMatchedByIDAndRecorded(t *testing.T) {
	srv, db := watchServer(t)
	ctx := t.Context()
	item, err := db.CreateMediaItem(ctx, domain.MediaItem{
		Kind: domain.KindSeries, Title: "Severance", Year: 2022, Monitored: true,
		IDs: domain.ExternalIDs{TMDB: 95396},
	})
	if err != nil {
		t.Fatal(err)
	}

	code, out := postWatched(t, srv, map[string]any{
		"event": "watched", "kind": "episode", "tmdb": 95396,
		"season": 1, "episode": 3, "watched_at": time.Now().Unix(), "user": "paul",
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d: %v", code, out)
	}
	if out["matched"] != true {
		t.Fatalf("matched = %v, want true: %v", out["matched"], out)
	}

	rows, err := db.ListPlurxWatched(ctx, item, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("recorded %d rows", len(rows))
	}
	if rows[0].Username != "paul" || rows[0].Season != 1 || rows[0].Episode != 3 {
		t.Errorf("row = %+v", rows[0])
	}
}

// A notification for something Monarr does not manage is not a failure. It
// must not 404 either: plurx retries failures, and there is nothing to retry
// about a film Monarr was never asked to look after.
func TestAnUnknownItemIsAcceptedAndIgnored(t *testing.T) {
	srv, _ := watchServer(t)
	code, out := postWatched(t, srv, map[string]any{
		"event": "watched", "kind": "movie", "tmdb": 999999,
		"watched_at": time.Now().Unix(), "user": "paul",
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d — an unknown item must not read as a delivery failure: %v", code, out)
	}
	if out["matched"] != false {
		t.Errorf("matched = %v, want false", out["matched"])
	}
}

// Matching on a title is the mistake this whole integration removes. Refused
// outright rather than falling back to one.
func TestAPayloadWithNoIDsIsRefused(t *testing.T) {
	srv, _ := watchServer(t)
	code, _ := postWatched(t, srv, map[string]any{
		"event": "watched", "kind": "movie", "watched_at": time.Now().Unix(),
	})
	if code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", code)
	}
}

// Watching something twice is one fact with a newer date, not two rows —
// which also bounds the table by library size rather than by how much
// television gets watched.
func TestRewatchingReplacesRatherThanAccumulates(t *testing.T) {
	srv, db := watchServer(t)
	ctx := t.Context()
	item, err := db.CreateMediaItem(ctx, domain.MediaItem{
		Kind: domain.KindMovie, Title: "Heat", Year: 1995, Monitored: true,
		IDs: domain.ExternalIDs{TMDB: 949},
	})
	if err != nil {
		t.Fatal(err)
	}
	first := time.Now().Add(-48 * time.Hour).Unix()
	for _, at := range []int64{first, time.Now().Unix()} {
		if code, out := postWatched(t, srv, map[string]any{
			"event": "watched", "kind": "movie", "tmdb": 949,
			"watched_at": at, "user": "paul",
		}); code != http.StatusOK {
			t.Fatalf("status = %d: %v", code, out)
		}
	}
	rows, err := db.ListPlurxWatched(ctx, item, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 (same person, same item)", len(rows))
	}
	if rows[0].WatchedAt.Unix() == first {
		t.Error("the newer watch did not replace the older one")
	}

	// Two people watching the same film IS two facts.
	postWatched(t, srv, map[string]any{
		"event": "watched", "kind": "movie", "tmdb": 949,
		"watched_at": time.Now().Unix(), "user": "someone-else",
	})
	rows, _ = db.ListPlurxWatched(ctx, item, 10)
	if len(rows) != 2 {
		t.Errorf("rows = %d, want 2 — per-user is the point of the decision", len(rows))
	}
}

// The signal has to reach the thing it was collected for.
func TestActivelyWatchedReportsWhatWasWatchedInTheWindow(t *testing.T) {
	srv, db := watchServer(t)
	ctx := t.Context()
	recent, err := db.CreateMediaItem(ctx, domain.MediaItem{
		Kind: domain.KindMovie, Title: "Heat", Year: 1995, IDs: domain.ExternalIDs{TMDB: 949},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateMediaItem(ctx, domain.MediaItem{
		Kind: domain.KindMovie, Title: "Alien", Year: 1979, IDs: domain.ExternalIDs{TMDB: 348},
	}); err != nil {
		t.Fatal(err)
	}
	postWatched(t, srv, map[string]any{
		"event": "watched", "kind": "movie", "tmdb": 949,
		"watched_at": time.Now().Unix(), "user": "paul",
	})
	postWatched(t, srv, map[string]any{
		"event": "watched", "kind": "movie", "tmdb": 348,
		"watched_at": time.Now().Add(-90 * 24 * time.Hour).Unix(), "user": "paul",
	})

	active, err := db.ActivelyWatched(ctx, time.Now().Add(-30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := active[recent]; !ok {
		t.Error("something watched today is not counted as active")
	}
	if len(active) != 1 {
		t.Errorf("active = %v — a film watched three months ago is not what somebody is following", active)
	}
}
