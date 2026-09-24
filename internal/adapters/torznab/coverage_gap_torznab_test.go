package torznab

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/ports"
)

// gapClient builds a client keyed uniquely in the process-wide capability
// cache (distinct id + key per test) and clears its entry on cleanup so a
// -count=N rerun on a reused port cannot see a stale entry.
func gapClient(t *testing.T, id int64, srvURL string) *Client {
	t.Helper()
	c := New(ports.IndexerConfig{ID: id, Name: "gap", URL: srvURL, APIKey: "gap-" + t.Name(), Protocol: "torrent"})
	t.Cleanup(func() {
		capabilityCache.Lock()
		delete(capabilityCache.entries, cacheKey(c.cfg))
		capabilityCache.Unlock()
	})
	return c
}

func gapRemote(t *testing.T, err error) *ports.RemoteError {
	t.Helper()
	var remote *ports.RemoteError
	if !errors.As(err, &remote) {
		t.Fatalf("err = %v, want *ports.RemoteError", err)
	}
	return remote
}

func TestGapAdFetchRSSIsAnEmptyPlainSearch(t *testing.T) {
	srv, queries := newServer(t)
	c := gapClient(t, 9001, srv.URL)
	rs, err := c.FetchRSS(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 3 || rs[0].Title != "Test.Show.S01E01.1080p.WEB-DL.x264-GRP" || rs[0].Peers != 7 {
		t.Fatalf("releases = %+v", rs)
	}
	if !rs[0].PublishDate.Equal(time.Date(2026, 7, 17, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("publish date = %v", rs[0].PublishDate)
	}
	q, err := url.ParseQuery((*queries)[0])
	if err != nil {
		t.Fatal(err)
	}
	if q.Get("t") != "search" || q.Get("q") != "" || q.Has("season") || q.Has("cat") {
		t.Errorf("rss query = %q", (*queries)[0])
	}
	if _, ok := q["q"]; !ok {
		t.Errorf("rss mode still sends an (empty) q parameter: %q", (*queries)[0])
	}
}

func TestGapAdBookSearchWithoutFormatUsesEveryBookCategory(t *testing.T) {
	srv, queries := newServer(t)
	c := gapClient(t, 9002, srv.URL)
	if _, err := c.Search(context.Background(), domain.SearchQuery{Q: "Dune", Kind: domain.KindBook}); err != nil {
		t.Fatal(err)
	}
	q, _ := url.ParseQuery((*queries)[0])
	if q.Get("cat") != "7000,7020,3030" || q.Get("t") != "search" {
		t.Errorf("legacy book query = %q", (*queries)[0])
	}
	// Configured categories win over the book defaults.
	narrowed := New(ports.IndexerConfig{ID: 9003, Name: "gap", URL: srv.URL, APIKey: "k", Protocol: "torrent", Categories: []int{7020}})
	if _, err := narrowed.Search(context.Background(), domain.SearchQuery{Q: "Dune", Kind: domain.KindBook}); err != nil {
		t.Fatal(err)
	}
	q, _ = url.ParseQuery((*queries)[1])
	if q.Get("cat") != "7020" {
		t.Errorf("configured categories should narrow the book search: %q", (*queries)[1])
	}
}

func TestGapAdSearchSurfacesHTTPAndProtocolErrors(t *testing.T) {
	var status atomic.Int64
	var body atomic.Value
	body.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if s := int(status.Load()); s != 0 {
			w.Header().Set("Retry-After", "90")
			w.WriteHeader(s)
			return
		}
		_, _ = w.Write([]byte(body.Load().(string)))
	}))
	t.Cleanup(srv.Close)
	c := gapClient(t, 9004, srv.URL)
	ctx := context.Background()

	for _, tc := range []struct {
		status   int
		category string
	}{
		{http.StatusUnauthorized, ports.RemoteAuth},
		{http.StatusForbidden, ports.RemoteAuth},
		{http.StatusNotFound, ports.RemoteNotFound},
		{http.StatusTooManyRequests, ports.RemoteRateLimit},
		{http.StatusBadGateway, ports.RemoteTransport},
	} {
		status.Store(int64(tc.status))
		_, err := c.Search(ctx, domain.SearchQuery{Q: "x"})
		remote := gapRemote(t, err)
		if remote.Category != tc.category || remote.HTTPStatus != tc.status {
			t.Errorf("status %d: remote = %+v", tc.status, remote)
		}
		if until := time.Until(remote.RetryAt); until < 80*time.Second || until > 91*time.Second {
			t.Errorf("status %d: Retry-After: 90 should set RetryAt ~90s out, got %v", tc.status, until)
		}
	}

	status.Store(0)
	for _, tc := range []struct {
		code     string
		category string
	}{
		{"101", ports.RemoteAuth},
		{"201", ports.RemoteUnsupportedQuery},
		{"429", ports.RemoteRateLimit},
		{"300", ports.RemoteInvalidResponse},
	} {
		body.Store(`<error code="` + tc.code + `" description="boom"/>`)
		_, err := c.Search(ctx, domain.SearchQuery{Q: "x"})
		remote := gapRemote(t, err)
		if remote.Category != tc.category || remote.ProtocolCode != tc.code || remote.HTTPStatus != http.StatusOK {
			t.Errorf("code %s: remote = %+v", tc.code, remote)
		}
	}

	body.Store("<rss><channel><item><title>x</title>")
	if _, err := c.Search(ctx, domain.SearchQuery{Q: "x"}); err == nil || !strings.Contains(err.Error(), "bad XML") {
		t.Errorf("truncated feed: err = %v", err)
	}

	// A feed whose items lack a title or a download link yields nothing.
	body.Store(`<rss><channel><item><title>no link</title></item><item><link>http://x/1</link></item></channel></rss>`)
	rs, err := c.Search(ctx, domain.SearchQuery{Q: "x"})
	if err != nil || len(rs) != 0 {
		t.Errorf("incomplete items: rs = %+v err = %v", rs, err)
	}

	srv.Close()
	_, err = c.Search(ctx, domain.SearchQuery{Q: "x"})
	if remote := gapRemote(t, err); remote.Category != ports.RemoteTransport {
		t.Errorf("closed server: remote = %+v", remote)
	}
}

func TestGapAdRetryAtParsesSecondsDatesAndGarbage(t *testing.T) {
	if !retryAt("").IsZero() || !retryAt("   ").IsZero() {
		t.Error("blank Retry-After should be the zero time")
	}
	if got := time.Until(retryAt("120")); got < 118*time.Second || got > 121*time.Second {
		t.Errorf("seconds form: %v", got)
	}
	want := time.Date(2026, time.October, 1, 12, 30, 0, 0, time.UTC)
	if got := retryAt(want.Format(http.TimeFormat)); !got.Equal(want) {
		t.Errorf("http-date form: %v, want %v", got, want)
	}
	if !retryAt("soon").IsZero() {
		t.Error("garbage Retry-After should be the zero time")
	}
}

func TestGapAdCapabilityHelpers(t *testing.T) {
	if !capabilityRetryAfter(nil).IsZero() {
		t.Error("nil error should have no retry time")
	}
	at := time.Now().Add(time.Hour)
	if got := capabilityRetryAfter(&ports.RemoteError{Category: ports.RemoteRateLimit, RetryAt: at}); !got.Equal(at) {
		t.Errorf("RetryAt should be preserved: %v", got)
	}
	if got := time.Until(capabilityRetryAfter(errors.New("plain"))); got < 55*time.Second || got > time.Minute {
		t.Errorf("errors without RetryAt should back off a minute, got %v", got)
	}
	if got := time.Until(capabilityRetryAfter(&ports.RemoteError{Category: ports.RemoteAuth})); got < 55*time.Second || got > time.Minute {
		t.Errorf("remote errors without RetryAt should back off a minute, got %v", got)
	}
	for _, tc := range []struct {
		err   error
		fatal bool
	}{
		{nil, false},
		{errors.New("plain"), false},
		{&ports.RemoteError{Category: ports.RemoteTransport}, false},
		{&ports.RemoteError{Category: ports.RemoteAuth}, true},
		{&ports.RemoteError{Category: ports.RemoteRateLimit}, true},
	} {
		if got := capabilityFatal(tc.err); got != tc.fatal {
			t.Errorf("capabilityFatal(%v) = %t", tc.err, got)
		}
	}
}

func TestGapAdCapabilitiesAreCachedPerConfigAndTTL(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(caps))
	}))
	t.Cleanup(srv.Close)
	c := gapClient(t, 9010, srv.URL)
	ctx := context.Background()

	first, err := c.Capabilities(ctx)
	if err != nil || !first.TV.Available || first.Degraded {
		t.Fatalf("caps = %+v err = %v", first, err)
	}
	second, err := c.Capabilities(ctx)
	if err != nil || hits.Load() != 1 || !second.FetchedAt.Equal(first.FetchedAt) {
		t.Fatalf("second call should be a cache hit: hits = %d err = %v", hits.Load(), err)
	}

	// An entry older than the TTL is refetched.
	capabilityCache.Lock()
	capabilityCache.entries[cacheKey(c.cfg)].caps.FetchedAt = time.Now().Add(-capabilitiesTTL - time.Minute)
	capabilityCache.Unlock()
	third, err := c.Capabilities(ctx)
	if err != nil || hits.Load() != 2 || !third.FetchedAt.After(first.FetchedAt) {
		t.Fatalf("expired entry should refetch: hits = %d err = %v", hits.Load(), err)
	}

	// A different credential is a different cache key.
	other := New(ports.IndexerConfig{ID: 9010, Name: "gap", URL: srv.URL, APIKey: "other-" + t.Name(), Protocol: "torrent"})
	t.Cleanup(func() {
		capabilityCache.Lock()
		delete(capabilityCache.entries, cacheKey(other.cfg))
		capabilityCache.Unlock()
	})
	if _, err := other.Capabilities(ctx); err != nil || hits.Load() != 3 {
		t.Fatalf("different key should fetch: hits = %d err = %v", hits.Load(), err)
	}
}

func TestGapAdCapabilitiesInFlightWaitAndCancel(t *testing.T) {
	release := make(chan struct{})
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		<-release
		_, _ = w.Write([]byte(caps))
	}))
	t.Cleanup(srv.Close)
	c := gapClient(t, 9011, srv.URL)
	key := cacheKey(c.cfg)

	var wg sync.WaitGroup
	results := make([]ports.IndexerCapabilities, 2)
	errs := make([]error, 2)
	wg.Add(1)
	go func() {
		defer wg.Done()
		results[0], errs[0] = c.Capabilities(context.Background())
	}()
	// Wait until the first caller has parked a pending entry.
	deadline := time.Now().Add(5 * time.Second)
	for {
		capabilityCache.Lock()
		entry := capabilityCache.entries[key]
		pending := entry != nil && entry.ready != nil
		capabilityCache.Unlock()
		if pending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first caller never registered an in-flight entry")
		}
		time.Sleep(time.Millisecond)
	}

	// A waiter whose context is already cancelled gives up without fetching.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Capabilities(cancelled); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled waiter: err = %v", err)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		results[1], errs[1] = c.Capabilities(context.Background())
	}()
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	for i := range results {
		if errs[i] != nil || !results[i].Movie.Available {
			t.Errorf("caller %d: caps = %+v err = %v", i, results[i], errs[i])
		}
	}
	if hits.Load() != 1 {
		t.Errorf("concurrent callers should share one fetch, got %d", hits.Load())
	}
}

func TestGapAdCapabilitiesDegradeToPreviousOnTransientFailure(t *testing.T) {
	var status atomic.Int64
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		if s := int(status.Load()); s != 0 {
			w.WriteHeader(s)
			return
		}
		_, _ = w.Write([]byte(caps))
	}))
	t.Cleanup(srv.Close)
	c := gapClient(t, 9012, srv.URL)
	ctx := context.Background()

	good, err := c.Capabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	status.Store(http.StatusServiceUnavailable)
	// Test forces a refetch; the transient failure keeps the previous snapshot
	// flagged as degraded rather than failing the indexer.
	if err := c.Test(ctx); err != nil {
		t.Fatalf("transient failure with a previous snapshot: %v", err)
	}
	degraded, err := c.Capabilities(ctx)
	if err != nil || !degraded.Degraded || !degraded.TV.Available || !degraded.FetchedAt.Equal(good.FetchedAt) {
		t.Fatalf("degraded caps = %+v err = %v", degraded, err)
	}
	if hits.Load() != 2 {
		t.Errorf("degraded snapshot should be served from cache, hits = %d", hits.Load())
	}
}

func TestGapAdCapabilitiesWithoutPreviousSnapshotAreDegradedAndUncached(t *testing.T) {
	var body atomic.Value
	body.Store("<html>not caps</html>")
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(body.Load().(string)))
	}))
	t.Cleanup(srv.Close)
	c := gapClient(t, 9013, srv.URL)
	ctx := context.Background()

	got, err := c.Capabilities(ctx)
	if err != nil || !got.Degraded || got.TV.Known {
		t.Fatalf("unrecognized caps XML: caps = %+v err = %v", got, err)
	}
	capabilityCache.Lock()
	_, cached := capabilityCache.entries[cacheKey(c.cfg)]
	capabilityCache.Unlock()
	if cached {
		t.Error("a non-fatal failure must not be cached")
	}
	if err := c.Test(ctx); err != nil {
		t.Errorf("Test on a transiently broken indexer: %v", err)
	}
	// Once the indexer answers, the next call fetches and caches for real.
	body.Store(caps)
	got, err = c.Capabilities(ctx)
	if err != nil || got.Degraded || !got.Generic.Available || hits.Load() != 3 {
		t.Fatalf("recovered caps = %+v err = %v hits = %d", got, err, hits.Load())
	}
}

func TestGapAdCapabilitiesFatalAuthIsCachedUntilRetryAfter(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Retry-After", "300")
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	c := gapClient(t, 9014, srv.URL)
	ctx := context.Background()

	_, err := c.Capabilities(ctx)
	remote := gapRemote(t, err)
	if remote.Category != ports.RemoteAuth || remote.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("remote = %+v", remote)
	}
	capabilityCache.Lock()
	entry := capabilityCache.entries[cacheKey(c.cfg)]
	capabilityCache.Unlock()
	if entry == nil || entry.err == nil || !entry.retryAfter.Equal(remote.RetryAt) {
		t.Fatalf("fatal errors should be cached with the Retry-After deadline: %+v", entry)
	}
	if until := time.Until(entry.retryAfter); until < 290*time.Second || until > 301*time.Second {
		t.Errorf("retryAfter should honour Retry-After: 300, got %v", until)
	}
	// Until the deadline the cached error is served without a request.
	if _, again := c.Capabilities(ctx); !errors.Is(again, err) || hits.Load() != 1 {
		t.Errorf("cached fatal error: err = %v hits = %d", again, hits.Load())
	}
	if err := c.Test(ctx); err == nil || hits.Load() != 2 {
		t.Errorf("Test forces a refetch even for fatal errors: err = %v hits = %d", err, hits.Load())
	}
	// Invalidation is a no-op on an errored entry.
	c.InvalidateSearchCapability("tv")

	// Past the deadline, the next call fetches again.
	capabilityCache.Lock()
	capabilityCache.entries[cacheKey(c.cfg)].retryAfter = time.Now().Add(-time.Second)
	capabilityCache.Unlock()
	if _, err := c.Capabilities(ctx); err == nil || hits.Load() != 3 {
		t.Errorf("expired deadline should refetch: err = %v hits = %d", err, hits.Load())
	}
}

func TestGapAdCapabilitiesRateLimitUsesHTTPDateRetryAfter(t *testing.T) {
	deadline := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", deadline.Format(http.TimeFormat))
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)
	c := gapClient(t, 9015, srv.URL)

	_, err := c.Capabilities(context.Background())
	remote := gapRemote(t, err)
	if remote.Category != ports.RemoteRateLimit || !remote.RetryAt.Equal(deadline) {
		t.Fatalf("remote = %+v, want rate limit until %v", remote, deadline)
	}
	if _, err := c.Capabilities(context.Background()); !errors.Is(err, remote) {
		t.Errorf("rate-limited entry should be served from cache: %v", err)
	}
}

func TestGapAdInvalidateSearchCapability(t *testing.T) {
	srv, _ := newServer(t)
	c := gapClient(t, 9016, srv.URL)
	ctx := context.Background()

	// Nothing cached yet: a no-op that must not panic or create an entry.
	c.InvalidateSearchCapability("tv")
	capabilityCache.Lock()
	_, cached := capabilityCache.entries[cacheKey(c.cfg)]
	capabilityCache.Unlock()
	if cached {
		t.Fatal("invalidation must not create an entry")
	}

	if _, err := c.Capabilities(ctx); err != nil {
		t.Fatal(err)
	}
	c.InvalidateSearchCapability("tv")
	got, _ := c.Capabilities(ctx)
	if !got.TV.Known || got.TV.Available || !got.Movie.Available || !got.Generic.Available {
		t.Errorf("after tv invalidation: %+v", got)
	}
	c.InvalidateSearchCapability("movie")
	got, _ = c.Capabilities(ctx)
	if !got.Movie.Known || got.Movie.Available || !got.Generic.Available {
		t.Errorf("after movie invalidation: %+v", got)
	}
	c.InvalidateSearchCapability("generic")
	got, _ = c.Capabilities(ctx)
	if !got.Generic.Known || got.Generic.Available {
		t.Errorf("after generic invalidation: %+v", got)
	}
	c.InvalidateSearchCapability("book")
	if again, _ := c.Capabilities(ctx); again.Generic.Available || again.TV.Available || again.Movie.Available || !again.FetchedAt.Equal(got.FetchedAt) {
		t.Errorf("unknown mode should change nothing: %+v", again)
	}
	// A forced refetch restores what the indexer advertises.
	if err := c.Test(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ = c.Capabilities(ctx); !got.TV.Available || !got.Movie.Available || !got.Generic.Available {
		t.Errorf("refetch should restore capabilities: %+v", got)
	}
}
