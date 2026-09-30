package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/app/recommendation"
	"github.com/pjunod/monarr/internal/domain"
	rules "github.com/pjunod/monarr/internal/domain/recommendation"
	"github.com/pjunod/monarr/internal/ports"
)

type recommendationAPISource struct {
	seedErr       error
	configuredErr error
	snapshots     atomic.Int64
	rotate        bool
}

func (f *recommendationAPISource) Snapshot(ctx context.Context) (context.Context, string, error) {
	n := f.snapshots.Add(1)
	key := "one"
	if f.rotate && n > 2 {
		key = "two"
	}
	return ctx, key, f.configuredErr
}
func (*recommendationAPISource) ResolveKeyword(context.Context, string) ([]ports.Keyword, error) {
	return []ports.Keyword{{ID: 1}}, nil
}
func (*recommendationAPISource) Candidates(context.Context, ports.CandidateRequest) (ports.CandidatePage, error) {
	return ports.CandidatePage{TotalPages: 1, Items: []ports.SearchResult{{TMDBID: 1}, {TMDBID: 2}, {TMDBID: 3}}}, nil
}
func (*recommendationAPISource) Facts(_ context.Context, id int64) (rules.Facts, error) {
	return rules.Facts{IDs: domain.ExternalIDs{TMDB: id, TVDB: 100 + id, IMDB: "tt123" + string(rune('0'+id))}, Title: "Example", Overview: "Three gay men navigate adult relationships and build their lives together.", Keywords: []string{"gay theme"}, GenreIDs: []int{18}, OriginalLanguage: "en", FetchedAt: time.Now()}, nil
}
func (f *recommendationAPISource) ResolveSeed(ctx context.Context, _ domain.ExternalRef) (rules.Facts, error) {
	if f.seedErr != nil {
		return rules.Facts{}, f.seedErr
	}
	return f.Facts(ctx, 99)
}

type recommendationAPIEncoder struct{}

func (recommendationAPIEncoder) ModelID() string         { return "test" }
func (recommendationAPIEncoder) State() (string, uint64) { return "ready", 1 }
func (recommendationAPIEncoder) Close() error            { return nil }
func (recommendationAPIEncoder) Encode(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = make([]float32, 384)
		out[i][0] = 1
	}
	return out, nil
}

type recommendationAPIModel struct{ fail bool }

func (*recommendationAPIModel) Enable(bool)              {}
func (*recommendationAPIModel) Detail() (string, string) { return "ready", "Verified" }
func (m *recommendationAPIModel) Install() error {
	if m.fail {
		return errors.New("unavailable")
	}
	return nil
}
func (m *recommendationAPIModel) Uninstall() error {
	if m.fail {
		return errors.New("enabled")
	}
	return nil
}

func TestRecommendationAPIResultsOwnershipAndLifecycle(t *testing.T) {
	e := newAPIEnv(t)
	source := &recommendationAPISource{}
	e.srv.deps.Recommendations = recommendation.New(source, e.db, recommendationAPIEncoder{})
	e.srv.deps.RecommendationModel = &recommendationAPIModel{}
	for _, item := range []domain.MediaItem{{Kind: domain.KindSeries, Title: "Owned", IDs: domain.ExternalIDs{TMDB: 1, TVDB: 101}}, {Kind: domain.KindSeries, Title: "Conflicting", IDs: domain.ExternalIDs{TMDB: 2, TVDB: 999}}} {
		if _, err := e.db.CreateMediaItem(context.Background(), item); err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest("POST", "/api/v1/metadata/recommendations", strings.NewReader(`{"kind":"series","query":"gay series","seed":{"provider":"tmdb","id":"99"},"filters":{"hideInLibrary":false}}`))
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, request)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var got struct {
		Ranking string
		Results []struct {
			Ownership     string
			Addability    string
			LibraryItemID int64 `json:"libraryItemId"`
			Reasons       []struct{ Code string }
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Ranking != "semantic" || len(got.Results) != 3 {
		t.Fatalf("%s", w.Body.String())
	}
	if got.Results[0].Ownership != "present" || got.Results[0].LibraryItemID == 0 || got.Results[1].Ownership != "ambiguous" || got.Results[1].Addability != "conflict" || got.Results[2].Ownership != "absent" {
		t.Fatalf("identity contract %s", w.Body.String())
	}
	if len(got.Results[0].Reasons) < 2 {
		t.Fatal("semantic/theme provenance absent")
	}
	for _, tc := range []struct {
		method, path string
		status       int
	}{{"GET", "/api/v1/metadata/recommendations/status", 200}, {"POST", "/api/v1/metadata/recommendations/model/install", 202}, {"DELETE", "/api/v1/metadata/recommendations/model", 204}} {
		w = httptest.NewRecorder()
		e.h.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.status {
			t.Fatalf("%s: %d", tc.path, w.Code)
		}
	}
	e.srv.deps.RecommendationModel = &recommendationAPIModel{fail: true}
	for _, tc := range []struct {
		method, path string
		status       int
	}{{"POST", "/api/v1/metadata/recommendations/model/install", 503}, {"DELETE", "/api/v1/metadata/recommendations/model", 409}} {
		w = httptest.NewRecorder()
		e.h.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.status {
			t.Fatal(w.Code)
		}
	}
}

func TestRecommendationAPIFailureCategories(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{{&ports.RemoteError{Category: ports.RemoteNotFound}, 404, "seed_not_found"}, {&ports.RemoteError{Category: ports.RemoteIdentityConflict}, 409, "identity_conflict"}, {&ports.RemoteError{Category: ports.RemoteUnsupportedHydration}, 422, "unsupported_seed"}, {&ports.RemoteError{Category: ports.RemoteAuth}, 503, "provider_unavailable"}, {errors.New("private detail"), 503, "provider_unavailable"}} {
		e := newAPIEnv(t)
		e.srv.deps.Recommendations = recommendation.New(&recommendationAPISource{seedErr: tc.err}, e.db, nil)
		w := httptest.NewRecorder()
		e.h.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/metadata/recommendations", strings.NewReader(`{"kind":"series","seed":{"provider":"tmdb","id":"99"}}`)))
		if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.code) || strings.Contains(w.Body.String(), "private detail") {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
	}
	e := newAPIEnv(t)
	for _, source := range []*recommendationAPISource{{configuredErr: ports.ErrProviderNotConfigured}, {rotate: true}} {
		e.srv.deps.Recommendations = recommendation.New(source, e.db, nil)
		w := httptest.NewRecorder()
		e.h.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/metadata/recommendations", strings.NewReader(`{"kind":"series","query":"gay series"}`)))
		if w.Code != 503 {
			t.Fatal(w.Code)
		}
	}
	for _, body := range []string{`{bad}`, `{"kind":"movie","query":"gay"}`} {
		w := httptest.NewRecorder()
		e.h.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/metadata/recommendations", strings.NewReader(body)))
		if w.Code != 400 {
			t.Fatal(w.Code)
		}
	}
	e.srv.deps.Recommendations = nil
	e.srv.deps.RecommendationModel = nil
	for _, tc := range []struct {
		method, path string
		status       int
	}{{"GET", "/api/v1/metadata/recommendations/status", 200}, {"POST", "/api/v1/metadata/recommendations/model/install", 503}, {"DELETE", "/api/v1/metadata/recommendations/model", 204}, {"POST", "/api/v1/metadata/recommendations", 503}} {
		w := httptest.NewRecorder()
		e.h.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{"kind":"series"}`)))
		if w.Code != tc.status {
			t.Fatalf("%s %d", tc.path, w.Code)
		}
	}
}
