package tmdb

import (
	"context"
	"errors"
	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/recommendation"
	"github.com/pjunod/monarr/internal/ports"
	"golang.org/x/time/rate"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestRecommendationSnapshotExactKeywordsAndNoSeasons(t *testing.T) {
	var keyReads atomic.Int64
	var hits atomic.Int64
	key := atomic.Value{}
	key.Store("first")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Query().Get("api_key") != "first" {
			t.Errorf("credential snapshot changed")
		}
		switch r.URL.Path {
		case "/search/keyword":
			_, _ = w.Write([]byte(`{"results":[{"id":1,"name":"gay theme"},{"id":2,"name":"gay themes"}]}`))
		case "/tv/1":
			if r.URL.Query().Get("append_to_response") != "keywords,external_ids" {
				t.Error("wrong append fields")
			}
			_, _ = w.Write([]byte(`{"id":1,"name":"Example","genres":[{"id":18,"name":"Drama"}],"keywords":{"results":[{"id":1,"name":"gay theme"}]},"external_ids":{"tvdb_id":2}}`))
		default:
			t.Errorf("unexpected season or other read: %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	client := New(server.URL, func(context.Context) (string, error) { keyReads.Add(1); return key.Load().(string), nil })
	ctx, hash, err := client.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	key.Store("second")
	keywords, err := client.ResolveKeyword(ctx, "gay theme")
	if err != nil || len(keywords) != 1 || keywords[0].ID != 1 {
		t.Fatalf("exact lookup %+v %v", keywords, err)
	}
	facts, err := client.Facts(ctx, 1)
	if err != nil || facts.IDs.TVDB != 2 || len(facts.Keywords) != 1 {
		t.Fatalf("facts %+v %v", facts, err)
	}
	if hits.Load() != 2 || keyReads.Load() != 1 {
		t.Fatalf("unbounded or resampled reads %d/%d", hits.Load(), keyReads.Load())
	}
	_, next, _ := client.Snapshot(context.Background())
	if hash == next {
		t.Fatal("credential rotation not fenced")
	}
}

func TestRecommendation41CallCeilingAndShared429Cooldown(t *testing.T) {
	var hits atomic.Int64
	limit := atomic.Bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if limit.Load() {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(429)
			return
		}
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer server.Close()
	client := New(server.URL, staticKey("test"))
	client.limiter = rate.NewLimiter(rate.Inf, 100)
	client.recommendationLimiter = rate.NewLimiter(rate.Inf, 100)
	ctx, _, _ := client.Snapshot(context.Background())
	for range 41 {
		if _, err := client.ResolveKeyword(ctx, "gay theme"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := client.ResolveKeyword(ctx, "gay theme"); err == nil || hits.Load() != 41 {
		t.Fatalf("budget exceeded: %v hits %d", err, hits.Load())
	}
	ctx, _, _ = client.Snapshot(context.Background())
	limit.Store(true)
	if _, err := client.ResolveKeyword(ctx, "gay theme"); err == nil {
		t.Fatal("429 accepted")
	}
	started := time.Now()
	_, err := client.SearchSeries(context.Background(), "uncached ordinary query")
	var remote *ports.RemoteError
	if !errors.As(err, &remote) || remote.Category != ports.RemoteRateLimit || hits.Load() != 42 || time.Since(started) > time.Second {
		t.Fatalf("shared cooldown bypassed: %v hits %d", err, hits.Load())
	}
}

func TestCandidateFiltersAndSeedMapping(t *testing.T) {
	var find string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/discover/tv":
			q := r.URL.Query()
			if q.Get("with_original_language") != "ko" || q.Get("with_genres") != "18|35" || q.Get("first_air_date.gte") != "2020-01-01" || q.Get("first_air_date.lte") != "2024-12-31" || q.Get("page") != "1" || q.Get("sort_by") != "vote_count.desc" {
				t.Errorf("incorrect filters %v", q)
			}
			_, _ = w.Write([]byte(`{"total_pages":2,"results":[{"id":1,"name":"Example","first_air_date":"2022-01-01"},{"id":0}]}`))
		case "/find/2":
			_, _ = w.Write([]byte(find))
		case "/tv/1":
			_, _ = w.Write([]byte(`{"id":1,"name":"Example","keywords":{"results":[]},"external_ids":{"tvdb_id":2}}`))
		case "/tv/1/recommendations":
			_, _ = w.Write([]byte(`{"total_pages":1,"results":[]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	client := New(server.URL, staticKey("test"))
	client.limiter = rate.NewLimiter(rate.Inf, 100)
	client.recommendationLimiter = rate.NewLimiter(rate.Inf, 100)
	ctx, _, _ := client.Snapshot(context.Background())
	language := "ko"
	from, to := 2020, 2024
	page, err := client.Candidates(ctx, ports.CandidateRequest{Path: "theme", Page: 1, KeywordID: 3, Filters: recommendation.Filters{OriginalLanguage: &language, YearFrom: &from, YearTo: &to, Genres: []int{18, 35}}})
	if err != nil || len(page.Items) != 1 || page.TotalPages != 2 || page.Items[0].Year != 2022 {
		t.Fatalf("page %+v %v", page, err)
	}
	if _, err = client.Candidates(ctx, ports.CandidateRequest{Path: "recommendations", SeedID: 1, Page: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err = client.Candidates(ctx, ports.CandidateRequest{Path: "seasons"}); err == nil {
		t.Fatal("season retrieval accepted")
	}
	for _, tc := range []struct {
		body     string
		category string
	}{{`{"tv_results":[]}`, ports.RemoteUnsupportedHydration}, {`{"tv_results":[{"id":1},{"id":3}]}`, ports.RemoteIdentityConflict}, {`{"tv_results":[{"id":1}]}`, ""}} {
		find = tc.body
		f, err := client.ResolveSeed(ctx, domain.ExternalRef{Provider: "tvdb", Value: "2"})
		if tc.category == "" {
			if err != nil || f.IDs.TMDB != 1 {
				t.Fatalf("mapped %+v %v", f, err)
			}
		} else {
			var remote *ports.RemoteError
			if !errors.As(err, &remote) || remote.Category != tc.category {
				t.Fatalf("mapping error %v", err)
			}
		}
	}
	for _, ref := range []domain.ExternalRef{{Provider: "tmdb", Value: "bad"}, {Provider: "other", Value: "1"}} {
		if _, err = client.ResolveSeed(ctx, ref); err == nil {
			t.Fatal("invalid seed accepted")
		}
	}
	if _, err = client.ResolveSeed(ctx, domain.ExternalRef{Provider: "tmdb", Value: "1"}); err != nil {
		t.Fatal(err)
	}
}
