package discover

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/ports"
)

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeProvider serves one movie row and one series row, and can be made to
// fail on demand.
type fakeProvider struct {
	name       string
	lists      []ports.DiscoverList
	configured atomic.Bool
	calls      atomic.Int64

	mu    sync.Mutex
	err   error
	items []ports.SearchResult
}

func newFake(name string, ids ...string) *fakeProvider {
	p := &fakeProvider{name: name}
	for _, id := range ids {
		p.lists = append(p.lists, ports.DiscoverList{
			ID: id, Title: "Row " + id, Blurb: "About " + id,
			Kind: domain.KindMovie, Source: name,
		})
	}
	p.configured.Store(true)
	p.items = []ports.SearchResult{{Kind: domain.KindMovie, TMDBID: 1, Title: "One"}}
	return p
}

func (p *fakeProvider) Name() string                    { return p.name }
func (p *fakeProvider) Lists() []ports.DiscoverList     { return p.lists }
func (p *fakeProvider) Configured(context.Context) bool { return p.configured.Load() }
func (p *fakeProvider) fail(err error)                  { p.mu.Lock(); p.err = err; p.mu.Unlock() }
func (p *fakeProvider) serve(items []ports.SearchResult) {
	p.mu.Lock()
	p.items, p.err = items, nil
	p.mu.Unlock()
}

func (p *fakeProvider) Discover(_ context.Context, listID string, _ int) ([]ports.SearchResult, error) {
	p.calls.Add(1)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return nil, p.err
	}
	for _, l := range p.lists {
		if l.ID == listID {
			return append([]ports.SearchResult(nil), p.items...), nil
		}
	}
	return nil, ports.ErrUnknownList
}

// age backdates a cache entry so the clock does not have to be waited on.
func (s *Service) age(key string, by time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.cache[key]
	e.fetched = e.fetched.Add(-by)
	s.cache[key] = e
}

func TestCatalogueSkipsUnconfiguredProviders(t *testing.T) {
	a, b := newFake("a", "a-1"), newFake("b", "b-1")
	b.configured.Store(false)
	s := New(quietLog(), nil, a, b)

	got := s.Lists(context.Background())
	if len(got) != 1 || got[0].ID != "a-1" {
		t.Fatalf("catalogue %+v, want only a-1", got)
	}
	if !s.Configured(context.Background()) {
		t.Error("one configured provider should make the service configured")
	}

	b.configured.Store(true)
	if len(s.Lists(context.Background())) != 2 {
		t.Error("configuring a provider should add its rows without a restart")
	}

	a.configured.Store(false)
	b.configured.Store(false)
	if s.Configured(context.Background()) {
		t.Error("no configured provider should report unconfigured")
	}
	if l := s.Lists(context.Background()); l == nil {
		t.Error("an empty catalogue must be an empty slice, not nil: it is serialized as JSON")
	}
}

func TestFreshRowIsServedFromCache(t *testing.T) {
	p := newFake("p", "row")
	s := New(quietLog(), nil, p)

	for range 3 {
		if _, err := s.Items(context.Background(), "row", 1); err != nil {
			t.Fatal(err)
		}
	}
	if p.calls.Load() != 1 {
		t.Errorf("%d upstream calls for 3 reads, want 1", p.calls.Load())
	}

	// Pages are cached separately — page 2 is a different row, not a repeat.
	if _, err := s.Items(context.Background(), "row", 2); err != nil {
		t.Fatal(err)
	}
	if p.calls.Load() != 2 {
		t.Errorf("%d upstream calls after paging, want 2", p.calls.Load())
	}
}

func TestExpiredRowIsRefetched(t *testing.T) {
	p := newFake("p", "row")
	s := New(quietLog(), nil, p)

	if _, err := s.Items(context.Background(), "row", 1); err != nil {
		t.Fatal(err)
	}
	s.age("row#1", freshFor+time.Minute)
	p.serve([]ports.SearchResult{{Kind: domain.KindMovie, TMDBID: 2, Title: "Two"}})

	got, err := s.Items(context.Background(), "row", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Title != "Two" {
		t.Errorf("served %q after expiry, want the refetched Two", got[0].Title)
	}
}

// The point of the stale window: an upstream blip degrades the page rather
// than blanking it.
func TestStaleIsServedWhenUpstreamFails(t *testing.T) {
	p := newFake("p", "row")
	s := New(quietLog(), nil, p)

	if _, err := s.Items(context.Background(), "row", 1); err != nil {
		t.Fatal(err)
	}
	s.age("row#1", freshFor+time.Minute)
	p.fail(errors.New("tmdb: unexpected status 503"))

	got, err := s.Items(context.Background(), "row", 1)
	if err != nil {
		t.Fatalf("a failed refresh with a stale entry should not error: %v", err)
	}
	if len(got) != 1 || got[0].Title != "One" {
		t.Errorf("served %+v, want the stale contents", got)
	}
}

// ...but only for a bounded time. Past that the row is not "less fresh", it
// is wrong, and reporting the outage beats hiding it.
func TestStaleExpiresAndTheErrorSurfaces(t *testing.T) {
	p := newFake("p", "row")
	s := New(quietLog(), nil, p)

	if _, err := s.Items(context.Background(), "row", 1); err != nil {
		t.Fatal(err)
	}
	s.age("row#1", staleFor+time.Minute)
	p.fail(errors.New("tmdb: unexpected status 503"))

	if _, err := s.Items(context.Background(), "row", 1); err == nil {
		t.Fatal("a day-old row with a failing upstream should return the error")
	}
}

func TestFirstFetchFailureHasNothingToFallBackOn(t *testing.T) {
	p := newFake("p", "row")
	p.fail(ports.ErrProviderNotConfigured)
	s := New(quietLog(), nil, p)

	_, err := s.Items(context.Background(), "row", 1)
	if !errors.Is(err, ports.ErrProviderNotConfigured) {
		t.Errorf("err = %v, want it passed through for the handler to map to a 503", err)
	}
}

func TestUnknownListIsRejectedWithoutCallingAnyProvider(t *testing.T) {
	p := newFake("p", "row")
	s := New(quietLog(), nil, p)

	_, err := s.Items(context.Background(), "nope", 1)
	if !errors.Is(err, ports.ErrUnknownList) {
		t.Errorf("err = %v, want ErrUnknownList", err)
	}
	if p.calls.Load() != 0 {
		t.Error("an unknown list should not reach a provider")
	}
}

func TestHydrationFillsArtworkAndSurvivesFailures(t *testing.T) {
	p := newFake("p", "row")
	p.serve([]ports.SearchResult{
		{Kind: domain.KindMovie, TMDBID: 1, Title: "No poster"},
		{Kind: domain.KindMovie, TMDBID: 2, Title: "Also none"},
		{Kind: domain.KindMovie, TMDBID: 3, Title: "Has one", PosterPath: "/already.jpg"},
		{Kind: domain.KindSeries, TVDBID: 9, Title: "No tmdb id"},
	})

	var hydrated atomic.Int64
	hydrate := func(_ context.Context, _ domain.MediaKind, id int64) (ports.SearchResult, error) {
		hydrated.Add(1)
		if id == 2 {
			return ports.SearchResult{}, errors.New("rate limited")
		}
		return ports.SearchResult{PosterPath: "/fetched.jpg", Overview: "Filled in.", Year: 1999}, nil
	}
	s := New(quietLog(), hydrate, p)

	got, err := s.Items(context.Background(), "row", 1)
	if err != nil {
		t.Fatal(err)
	}
	// Only the two missing a poster AND carrying a TMDB id are worth a call.
	if hydrated.Load() != 2 {
		t.Errorf("%d hydration calls, want 2", hydrated.Load())
	}
	if got[0].PosterPath != "/fetched.jpg" || got[0].Overview != "Filled in." || got[0].Year != 1999 {
		t.Errorf("first result not hydrated: %+v", got[0])
	}
	if got[0].Title != "No poster" {
		t.Errorf("hydration overwrote the provider's title: %q", got[0].Title)
	}
	if got[1].PosterPath != "" || got[1].Title != "Also none" {
		t.Errorf("a failed hydration should cost the card its poster, not the row: %+v", got[1])
	}
	if got[2].PosterPath != "/already.jpg" {
		t.Errorf("an existing poster was replaced: %+v", got[2])
	}
}

func TestCallersCannotEditWhatTheNextCallerReads(t *testing.T) {
	p := newFake("p", "row")
	s := New(quietLog(), nil, p)

	first, err := s.Items(context.Background(), "row", 1)
	if err != nil {
		t.Fatal(err)
	}
	first[0].Title = "clobbered"

	second, err := s.Items(context.Background(), "row", 1)
	if err != nil {
		t.Fatal(err)
	}
	if second[0].Title != "One" {
		t.Errorf("cached row was mutated through a returned slice: %q", second[0].Title)
	}
}

func TestDuplicateListIDsAreSurvivable(t *testing.T) {
	a, b := newFake("a", "shared"), newFake("b", "shared")
	b.serve([]ports.SearchResult{{Kind: domain.KindMovie, TMDBID: 9, Title: "From b"}})
	s := New(quietLog(), nil, a, b)

	got, err := s.Items(context.Background(), "shared", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Title != "From b" {
		t.Errorf("last provider registered should win, got %q", got[0].Title)
	}
}
