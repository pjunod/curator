package recommendation

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	rules "github.com/pjunod/monarr/internal/domain/recommendation"
	"github.com/pjunod/monarr/internal/ports"
)

type fakeSource struct {
	calls      atomic.Int64
	generation atomic.Int64
	block      <-chan struct{}
	entered    chan struct{}
	once       sync.Once
	facts      map[int64]rules.Facts
	pageFn     func(ports.CandidateRequest) (ports.CandidatePage, error)
	factsFn    func(int64) (rules.Facts, error)
}

func (f *fakeSource) Snapshot(ctx context.Context) (context.Context, string, error) {
	return ctx, fmt.Sprint(f.generation.Load()), nil
}
func (f *fakeSource) ResolveKeyword(context.Context, string) ([]ports.Keyword, error) {
	f.calls.Add(1)
	return []ports.Keyword{{ID: 1}}, nil
}
func (f *fakeSource) Candidates(ctx context.Context, request ports.CandidateRequest) (ports.CandidatePage, error) {
	f.calls.Add(1)
	if f.pageFn != nil {
		return f.pageFn(request)
	}
	if f.entered != nil {
		f.once.Do(func() { close(f.entered) })
	}
	if f.block != nil {
		select {
		case <-ctx.Done():
			return ports.CandidatePage{}, ctx.Err()
		case <-f.block:
		}
	}
	return ports.CandidatePage{TotalPages: 1, Items: []ports.SearchResult{{TMDBID: 1, Overview: "Three friends navigate life as gay men."}, {TMDBID: 2, Overview: "A detective solves crime."}, {TMDBID: 3, Overview: "Gay men fall in love."}}}, nil
}
func (f *fakeSource) Facts(_ context.Context, id int64) (rules.Facts, error) {
	f.calls.Add(1)
	if f.factsFn != nil {
		return f.factsFn(id)
	}
	if v, ok := f.facts[id]; ok {
		return v, nil
	}
	return rules.Facts{IDs: domain.ExternalIDs{TMDB: id}, Title: fmt.Sprint(id), Overview: "Three friends navigate life as gay men.", Keywords: []string{"gay theme"}, OriginalLanguage: "en", Year: 2022, FetchedAt: time.Now()}, nil
}

type fakeEncoder struct {
	state      string
	generation uint64
	fail       bool
	change     bool
}

func (e *fakeEncoder) State() (string, uint64) { return e.state, e.generation }
func (*fakeEncoder) ModelID() string           { return "test" }
func (*fakeEncoder) Close() error              { return nil }
func (e *fakeEncoder) Encode(_ context.Context, texts []string) ([][]float32, error) {
	if e.fail {
		e.state = "failed"
		if e.change {
			e.generation++
		}
		return nil, errors.New("helper failed")
	}
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = make([]float32, 384)
		out[i][0] = 1
	}
	return out, nil
}

func TestRuntimeFailureFallsBackButConfigurationChangeInvalidates(t *testing.T) {
	for _, change := range []bool{false, true} {
		encoder := &fakeEncoder{state: "ready", generation: 1, fail: true, change: change}
		s := New(&fakeSource{}, &fakeOwner{}, encoder)
		got, err := s.Search(context.Background(), query())
		if change {
			if !errors.Is(err, ErrStateChanged) {
				t.Fatalf("configuration change published: %v", err)
			}
		} else if err != nil || len(got.Results) != 3 || got.Ranking != "metadata_only" || got.ModelState != "failed" {
			t.Fatalf("no usable fallback: %+v %v", got, err)
		}
	}
}

func TestRelatedPaginationUsesRecommendationsPageCount(t *testing.T) {
	seenSecond := false
	source := &fakeSource{pageFn: func(r ports.CandidateRequest) (ports.CandidatePage, error) {
		if r.Path == "recommendations" && r.Page == 1 {
			return ports.CandidatePage{}, errors.New("temporary failure")
		}
		if r.Path == "recommendations" && r.Page == 2 {
			seenSecond = true
		}
		return ports.CandidatePage{TotalPages: 1, Items: []ports.SearchResult{{TMDBID: 2, Overview: "A story"}}}, nil
	}}
	s := New(source, &fakeOwner{}, nil)
	r := rules.Request{Kind: domain.KindSeries, Seed: &domain.ExternalRef{Provider: "tmdb", Value: "1"}}
	if _, err := s.Search(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if !seenSecond {
		t.Fatal("similar page count suppressed recommendations page 2")
	}
}

func TestPartialWithoutEligibleResultsIsUnavailable(t *testing.T) {
	for _, partial := range []bool{false, true} {
		source := &fakeSource{pageFn: func(r ports.CandidateRequest) (ports.CandidatePage, error) {
			if partial && r.KeywordID == 1 && r.Path == "theme" && r.Page == 1 {
				return ports.CandidatePage{}, errors.New("failed")
			}
			return ports.CandidatePage{TotalPages: 1}, nil
		}}
		s := New(source, &fakeOwner{}, nil)
		_, err := s.Search(context.Background(), query())
		if partial && !errors.Is(err, ErrUnavailable) {
			t.Fatalf("partial empty success: %v", err)
		}
		if !partial && err != nil {
			t.Fatalf("complete empty failed: %v", err)
		}
	}
	// Successful details for an ineligible title cannot mask another failed detail.
	source := &fakeSource{factsFn: func(id int64) (rules.Facts, error) {
		if id == 1 {
			return rules.Facts{}, errors.New("failed")
		}
		return rules.Facts{IDs: domain.ExternalIDs{TMDB: id}, Overview: "A detective solves a theft."}, nil
	}}
	if _, err := New(source, &fakeOwner{}, nil).Search(context.Background(), query()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("partial ineligible success: %v", err)
	}
}

func TestCacheReplacementAccountingAndNoProseRetention(t *testing.T) {
	s := New(&fakeSource{}, &fakeOwner{}, nil)
	for range 3 {
		got, err := s.Search(context.Background(), query())
		if err != nil || got.Applied.Query != query().Query {
			t.Fatalf("applied query lost: %+v %v", got, err)
		}
		s.mu.Lock()
		sum := 0
		for k, v := range s.cache {
			sum += v.bytes
			if v.value.Applied.Query != "" || v.value.Applied.RankingText != "" {
				t.Fatal("prose retained")
			}
			v.expires = time.Now().Add(-time.Second)
			s.cache[k] = v
		}
		if sum != s.bytes {
			t.Fatalf("cache accounting: %d/%d", sum, s.bytes)
		}
		s.mu.Unlock()
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestFourAttachedCallersAndLastCancellation(t *testing.T) {
	source := &fakeSource{block: make(chan struct{}), entered: make(chan struct{})}
	s := New(source, &fakeOwner{}, nil)
	cancellations := []context.CancelFunc{}
	results := make(chan error, 4)
	for i := 0; i < 4; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancellations = append(cancellations, cancel)
		go func() { _, err := s.Search(ctx, query()); results <- err }()
		want := i + 1
		waitFor(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.active != nil && s.active.waiters == want })
	}
	if _, err := s.Search(context.Background(), query()); !errors.Is(err, ErrBusy) {
		t.Fatalf("fifth caller accepted: %v", err)
	}
	for _, cancel := range cancellations {
		cancel()
	}
	for range 4 {
		if err := <-results; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	waitFor(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.active == nil })
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.cache) > 0 {
		t.Fatal("cancelled work cached")
	}
}

type blockingOwner struct {
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int64
}

func (o *blockingOwner) LookupSeries(ctx context.Context, ids []domain.ExternalIDs) ([]ports.Ownership, error) {
	o.calls.Add(1)
	o.entered <- struct{}{}
	select {
	case <-o.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return (&fakeOwner{}).LookupSeries(ctx, ids)
}

func TestTwoCachedReadersWhileColdSearchRunsAndPublicationFence(t *testing.T) {
	source := &fakeSource{}
	s := New(source, &fakeOwner{}, nil)
	if _, err := s.Search(context.Background(), query()); err != nil {
		t.Fatal(err)
	}
	owner := &blockingOwner{entered: make(chan struct{}, 2), release: make(chan struct{})}
	s.ownership = owner
	coldRelease := make(chan struct{})
	source.block = coldRelease
	source.entered = make(chan struct{})
	other := query()
	other.Query = "gay romance"
	cold := make(chan error, 1)
	go func() { _, err := s.Search(context.Background(), other); cold <- err }()
	<-source.entered
	reads := make(chan error, 2)
	for range 2 {
		go func() { _, err := s.Search(context.Background(), query()); reads <- err }()
		<-owner.entered
	}
	if _, err := s.Search(context.Background(), query()); !errors.Is(err, ErrBusy) {
		t.Fatalf("third cached reader accepted: %v", err)
	}
	source.generation.Add(1)
	close(owner.release)
	for range 2 {
		if err := <-reads; !errors.Is(err, ErrStateChanged) {
			t.Fatalf("stale cached result published: %v", err)
		}
	}
	close(coldRelease)
	if err := <-cold; !errors.Is(err, ErrStateChanged) {
		t.Fatalf("stale cold result published: %v", err)
	}
}

func TestCombinedSelectedFactsStayInRetainedPool(t *testing.T) {
	source := &fakeSource{}
	source.pageFn = func(r ports.CandidateRequest) (ports.CandidatePage, error) {
		items := []ports.SearchResult{}
		base := int64(100)
		if r.Path != "theme" {
			base = 200
		}
		for i := int64(0); i < 60; i++ {
			overview := "A detective solves a theft."
			if i > 30 {
				overview = "Gay men fall in love."
			}
			items = append(items, ports.SearchResult{TMDBID: base + i, Overview: overview})
		}
		return ports.CandidatePage{Items: items, TotalPages: 1}, nil
	}
	source.factsFn = func(id int64) (rules.Facts, error) {
		if id != 1 && (id < 100 || id >= 130) && (id < 200 || id >= 230) {
			t.Errorf("enriched outside retained pool: %d", id)
		}
		return rules.Facts{IDs: domain.ExternalIDs{TMDB: id}, Overview: "Gay men fall in love."}, nil
	}
	r := query()
	r.Seed = &domain.ExternalRef{Provider: "tmdb", Value: "1"}
	if _, err := New(source, &fakeOwner{}, nil).Search(context.Background(), r); err != nil {
		t.Fatal(err)
	}
}
func (f *fakeSource) ResolveSeed(ctx context.Context, _ domain.ExternalRef) (rules.Facts, error) {
	return f.Facts(ctx, 1)
}

type fakeOwner struct{ owned atomic.Bool }

func (o *fakeOwner) LookupSeries(_ context.Context, ids []domain.ExternalIDs) ([]ports.Ownership, error) {
	out := make([]ports.Ownership, len(ids))
	for i := range out {
		out[i].State = "absent"
		if o.owned.Load() {
			out[i].State = "present"
			out[i].LibraryItemID = 10
		}
	}
	return out, nil
}
func query() rules.Request { return rules.Request{Kind: domain.KindSeries, Query: "gay-themed series"} }
func TestCachedResultsRefreshOwnershipWithoutProviderReads(t *testing.T) {
	source := &fakeSource{}
	owner := &fakeOwner{}
	s := New(source, owner, nil)
	first, e := s.Search(context.Background(), query())
	if e != nil || len(first.Results) != 3 {
		t.Fatalf("first %+v %v", first, e)
	}
	calls := source.calls.Load()
	owner.owned.Store(true)
	second, e := s.Search(context.Background(), query())
	if e != nil || len(second.Results) != 0 || second.HiddenOwnedCount != 3 || source.calls.Load() != calls {
		t.Fatalf("cached %+v %v calls %d/%d", second, e, source.calls.Load(), calls)
	}
}
func TestJoinedCancellationAndColdRejection(t *testing.T) {
	release := make(chan struct{})
	source := &fakeSource{block: release, entered: make(chan struct{})}
	s := New(source, &fakeOwner{}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, e := s.Search(ctx, query()); first <- e }()
	<-source.entered
	second := make(chan error, 1)
	go func() { _, e := s.Search(context.Background(), query()); second <- e }()
	deadline := time.Now().Add(time.Second)
	for {
		s.mu.Lock()
		waiters := s.active.waiters
		s.mu.Unlock()
		if waiters == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("join did not attach")
		}
		time.Sleep(time.Millisecond)
	}
	other := query()
	other.Query = "gay romance"
	if _, e := s.Search(context.Background(), other); !errors.Is(e, ErrBusy) {
		t.Fatalf("different cold accepted: %v", e)
	}
	cancel()
	if e := <-first; !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	close(release)
	if e := <-second; e != nil {
		t.Fatalf("joined caller lost work: %v", e)
	}
}
func TestCombinedThemeGateAndMissingLanguage(t *testing.T) {
	source := &fakeSource{facts: map[int64]rules.Facts{2: {IDs: domain.ExternalIDs{TMDB: 2}, Overview: "A detective solves a crime.", Keywords: []string{"lgbt"}, OriginalLanguage: "ko"}, 3: {IDs: domain.ExternalIDs{TMDB: 3}, Keywords: []string{"gay theme"}}}}
	s := New(source, &fakeOwner{}, nil)
	r := query()
	r.Seed = &domain.ExternalRef{Provider: "tmdb", Value: "1"}
	language := "ko"
	r.Filters.OriginalLanguage = &language
	value, e := s.Search(context.Background(), r)
	if e != nil {
		t.Fatal(e)
	}
	if len(value.Results) != 0 {
		t.Fatalf("combined path bypassed required theme/language: %+v", value.Results)
	}
}
func TestMergeRanksAreAbsoluteAndCountEachListOnce(t *testing.T) {
	pages := []ports.CandidatePage{{Items: []ports.SearchResult{{TMDBID: 1}}}, {Items: []ports.SearchResult{{TMDBID: 1}}}, {Items: []ports.SearchResult{{TMDBID: 2}, {TMDBID: 2}, {TMDBID: 1}}}}
	out := merge(pages, []string{"recommendations", "similar", "recommendations"}, []int{1, 1, 2})
	if len(out) != 2 || len(out[0].ranks) != 2 || out[0].ranks["recommendations"] != 1 || out[1].ranks["recommendations"] != 21 {
		t.Fatalf("ranks %+v", out)
	}
}
