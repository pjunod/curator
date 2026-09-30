package tmdb

import (
	"context"
	"errors"
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
