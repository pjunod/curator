package library

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/parser"
	"github.com/pjunod/monarr/internal/infra/bus"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// gapMetaProvider is fakeProvider plus the two optional capabilities the
// identity code paths look for: aliases for verified ids, and exact-id
// lookup. Both are function-valued so each test decides what comes back.
type gapMetaProvider struct {
	fakeProvider
	identity func(kind domain.MediaKind, ids domain.ExternalIDs) (ports.IdentityMetadata, error)
	lookup   func(kind domain.MediaKind, ref domain.ExternalRef) ([]ports.SearchResult, error)
	calls    int
}

func (p *gapMetaProvider) IdentityMetadata(_ context.Context, kind domain.MediaKind, ids domain.ExternalIDs) (ports.IdentityMetadata, error) {
	p.calls++
	if p.identity == nil {
		return ports.IdentityMetadata{}, nil
	}
	return p.identity(kind, ids)
}

func (p *gapMetaProvider) LookupExternal(_ context.Context, kind domain.MediaKind, ref domain.ExternalRef) ([]ports.SearchResult, error) {
	p.calls++
	if p.lookup == nil {
		return nil, nil
	}
	return p.lookup(kind, ref)
}

// gapSeriesProvider is a chain link that also answers identity and exact-id
// questions, so the TVDB/IMDB routes through the chain are reachable.
type gapSeriesProvider struct {
	chainProvider
	identity func(kind domain.MediaKind, ids domain.ExternalIDs) (ports.IdentityMetadata, error)
	lookup   func(kind domain.MediaKind, ref domain.ExternalRef) ([]ports.SearchResult, error)
}

func (p gapSeriesProvider) IdentityMetadata(_ context.Context, kind domain.MediaKind, ids domain.ExternalIDs) (ports.IdentityMetadata, error) {
	if p.identity == nil {
		return ports.IdentityMetadata{}, nil
	}
	return p.identity(kind, ids)
}

func (p gapSeriesProvider) LookupExternal(_ context.Context, kind domain.MediaKind, ref domain.ExternalRef) ([]ports.SearchResult, error) {
	if p.lookup == nil {
		return nil, nil
	}
	return p.lookup(kind, ref)
}

// gapFailingProvider errors on every search, for the "provider down" paths.
type gapFailingProvider struct{ fakeProvider }

func (gapFailingProvider) SearchMovies(context.Context, string) ([]ports.SearchResult, error) {
	return nil, errors.New("tmdb down")
}

func gapAlias(title string) domain.TitleAlias {
	return domain.TitleAlias{Title: title, Searchable: true}
}

func gapAddMovie(t *testing.T, svc *Service, tmdbID int64) domain.MediaItem {
	t.Helper()
	item, err := svc.Add(context.Background(), AddRequest{Kind: domain.KindMovie, TMDBID: tmdbID, Monitored: true})
	if err != nil {
		t.Fatal(err)
	}
	return item
}

func gapSourceStatus(t *testing.T, db *sqlite.DB, itemID int64, source string) domain.IdentitySourceStatus {
	t.Helper()
	item, err := db.GetMediaItemFull(context.Background(), itemID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range item.IdentitySources {
		if s.Source == source {
			return s
		}
	}
	t.Fatalf("no identity source %q on item %d: %+v", source, itemID, item.IdentitySources)
	return domain.IdentitySourceStatus{}
}

// ---- small value methods ----

func TestGapLibEventTypesAndConflictError(t *testing.T) {
	if got := (IdentityChanged{}).EventType(); got != "library.identity.changed" {
		t.Errorf("IdentityChanged.EventType = %q", got)
	}
	if got := (MediaAdded{}).EventType(); got != "media.added" {
		t.Errorf("MediaAdded.EventType = %q", got)
	}
	if got := (FileProbed{}).EventType(); got != "library.file.probed" {
		t.Errorf("FileProbed.EventType = %q", got)
	}
	if got := (ScanCompleted{}).EventType(); got != "library.scan.completed" {
		t.Errorf("ScanCompleted.EventType = %q", got)
	}
	cause := errors.New("boom")
	err := &IdentityConflictError{ItemIDs: []int64{3, 7}, Cause: cause}
	if !strings.Contains(err.Error(), "[3 7]") {
		t.Errorf("Error = %q, want the item ids", err.Error())
	}
	if !errors.Is(err, cause) {
		t.Errorf("Unwrap should expose the cause")
	}
}

// ---- manual aliases ----

func TestGapLibAddManualAliasStoresAndPublishes(t *testing.T) {
	svc, db, b := newService(t)
	ctx := context.Background()
	item := gapAddMovie(t, svc, 550)
	events, cancel := bus.Subscribe[IdentityChanged](b, 8)
	defer cancel()

	before, err := db.IdentityRevision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	alias, err := svc.AddManualAlias(ctx, item.ID, "  El Club de la Lucha  ", true)
	if err != nil {
		t.Fatal(err)
	}
	if alias.ID == 0 || alias.Title != "El Club de la Lucha" || alias.Source != "manual" || alias.Role != "manual" || !alias.Searchable {
		t.Errorf("alias = %+v", alias)
	}
	stored, err := db.GetMediaItemFull(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range stored.Aliases {
		if a.ID == alias.ID && a.Title == "El Club de la Lucha" {
			found = true
		}
	}
	if !found {
		t.Errorf("alias not stored on the item: %+v", stored.Aliases)
	}
	select {
	case e := <-events:
		if e.ItemID != item.ID || e.Revision <= before {
			t.Errorf("event = %+v, want item %d and a revision above %d", e, item.ID, before)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("IdentityChanged not published")
	}

	// The same spelling twice is a duplicate, not a second alias.
	if _, err := svc.AddManualAlias(ctx, item.ID, "El Club de la Lucha", true); !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("duplicate alias err = %v, want ErrAlreadyExists", err)
	}
}

func TestGapLibAddManualAliasRejectsBadInput(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()
	item := gapAddMovie(t, svc, 550)

	if _, err := svc.AddManualAlias(ctx, 424242, "Anything", true); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown item err = %v, want ErrNotFound", err)
	}
	for _, title := range []string{"", "   ", strings.Repeat("x", 257)} {
		if _, err := svc.AddManualAlias(ctx, item.ID, title, true); err == nil {
			t.Errorf("alias %q should be rejected", title)
		} else if errors.Is(err, ErrAlreadyExists) || errors.Is(err, ErrNotFound) {
			t.Errorf("alias %q err = %v, want a validation error", title, err)
		}
	}
}

func TestGapLibDeleteManualAlias(t *testing.T) {
	svc, db, b := newService(t)
	ctx := context.Background()
	item := gapAddMovie(t, svc, 550)
	alias, err := svc.AddManualAlias(ctx, item.ID, "Club de Combat", false)
	if err != nil {
		t.Fatal(err)
	}
	events, cancel := bus.Subscribe[IdentityChanged](b, 8)
	defer cancel()

	if err := svc.DeleteManualAlias(ctx, item.ID, alias.ID); err != nil {
		t.Fatal(err)
	}
	stored, err := db.GetMediaItemFull(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range stored.Aliases {
		if a.ID == alias.ID {
			t.Errorf("alias %d still stored after delete", alias.ID)
		}
	}
	select {
	case e := <-events:
		if e.ItemID != item.ID {
			t.Errorf("event = %+v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("IdentityChanged not published on delete")
	}
	// Gone, and an alias that never existed, both answer not-found.
	if err := svc.DeleteManualAlias(ctx, item.ID, alias.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete err = %v, want ErrNotFound", err)
	}
	if err := svc.DeleteManualAlias(ctx, item.ID, 999999); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown alias err = %v, want ErrNotFound", err)
	}
}

// ---- RefreshIdentity ----

func TestGapLibRefreshIdentityStoresSnapshotAndSkipsFresh(t *testing.T) {
	svc, db, b := newService(t)
	ctx := context.Background()
	item := gapAddMovie(t, svc, 550)

	meta := &gapMetaProvider{identity: func(kind domain.MediaKind, ids domain.ExternalIDs) (ports.IdentityMetadata, error) {
		if kind != domain.KindMovie || ids.TMDB != 550 {
			return ports.IdentityMetadata{}, fmt.Errorf("unexpected ids %+v", ids)
		}
		return ports.IdentityMetadata{
			Aliases:   []domain.TitleAlias{gapAlias("Fight Club"), gapAlias("Клуб Бойцов"), gapAlias("  ")},
			Countries: []domain.CountryEvidence{{Code: "US", Source: "tmdb", Basis: "origin"}},
		}, nil
	}}
	svc.meta = meta
	events, cancel := bus.Subscribe[IdentityChanged](b, 8)
	defer cancel()

	if err := svc.RefreshIdentity(ctx, item.ID, false); err != nil {
		t.Fatal(err)
	}
	if meta.calls != 1 {
		t.Fatalf("provider calls = %d, want 1", meta.calls)
	}
	stored, err := db.GetMediaItemFull(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	var tmdbAliases []string
	for _, a := range stored.Aliases {
		if a.Source == "tmdb" {
			tmdbAliases = append(tmdbAliases, a.Title)
		}
	}
	if len(tmdbAliases) != 2 {
		t.Errorf("tmdb aliases = %v, want the two non-blank ones", tmdbAliases)
	}
	status := gapSourceStatus(t, db, item.ID, "tmdb")
	if status.FetchedAt.IsZero() || status.LastError != "" || !status.RetryAfter.IsZero() {
		t.Errorf("status = %+v, want a clean fetched snapshot", status)
	}
	if len(status.Countries) != 1 || status.Countries[0].Code != "US" {
		t.Errorf("countries = %+v", status.Countries)
	}
	select {
	case e := <-events:
		if e.ItemID != item.ID || e.Revision == 0 {
			t.Errorf("event = %+v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("IdentityChanged not published")
	}

	// Fresh: a second non-forced refresh does not go back to the provider.
	if err := svc.RefreshIdentity(ctx, item.ID, false); err != nil {
		t.Fatal(err)
	}
	if meta.calls != 1 {
		t.Errorf("fresh snapshot was refetched: calls = %d", meta.calls)
	}
	// Forced: it does.
	if err := svc.RefreshIdentity(ctx, item.ID, true); err != nil {
		t.Fatal(err)
	}
	if meta.calls != 2 {
		t.Errorf("forced refresh should refetch: calls = %d", meta.calls)
	}
}

func TestGapLibRefreshIdentityRecordsProviderFailure(t *testing.T) {
	svc, db, _ := newService(t)
	ctx := context.Background()
	item := gapAddMovie(t, svc, 550)

	meta := &gapMetaProvider{identity: func(domain.MediaKind, domain.ExternalIDs) (ports.IdentityMetadata, error) {
		return ports.IdentityMetadata{}, errors.New("tmdb: 503")
	}}
	svc.meta = meta

	// Ordinary failure is recorded, not returned: the sweep owns retries.
	start := time.Now()
	if err := svc.RefreshIdentity(ctx, item.ID, false); err != nil {
		t.Fatalf("provider failure must not fail the job: %v", err)
	}
	status := gapSourceStatus(t, db, item.ID, "tmdb")
	if status.LastError == "" || !strings.Contains(status.LastError, "503") {
		t.Errorf("LastError = %q", status.LastError)
	}
	if !status.FetchedAt.IsZero() {
		t.Errorf("a never-successful source must not look fetched: %+v", status)
	}
	wantRetry := start.Add(identityRetryDelay)
	if status.RetryAfter.Before(wantRetry.Add(-5*time.Second)) || status.RetryAfter.After(wantRetry.Add(5*time.Second)) {
		t.Errorf("RetryAfter = %v, want about %v", status.RetryAfter, wantRetry)
	}

	// While the retry window is open the provider is left alone, even forced.
	if err := svc.RefreshIdentity(ctx, item.ID, true); err != nil {
		t.Fatal(err)
	}
	if meta.calls != 1 {
		t.Errorf("provider called inside its retry window: calls = %d", meta.calls)
	}
}

func TestGapLibRefreshIdentityHonoursRemoteRetryAt(t *testing.T) {
	svc, db, _ := newService(t)
	ctx := context.Background()
	item := gapAddMovie(t, svc, 550)

	retryAt := time.Now().Add(6 * time.Hour).Truncate(time.Millisecond)
	svc.meta = &gapMetaProvider{identity: func(domain.MediaKind, domain.ExternalIDs) (ports.IdentityMetadata, error) {
		return ports.IdentityMetadata{}, &ports.RemoteError{Category: ports.RemoteRateLimit, HTTPStatus: 429, RetryAt: retryAt}
	}}
	if err := svc.RefreshIdentity(ctx, item.ID, false); err != nil {
		t.Fatal(err)
	}
	status := gapSourceStatus(t, db, item.ID, "tmdb")
	if !status.RetryAfter.Equal(retryAt) {
		t.Errorf("RetryAfter = %v, want the provider's %v", status.RetryAfter, retryAt)
	}
	if !strings.Contains(status.LastError, "rate_limit") {
		t.Errorf("LastError = %q", status.LastError)
	}
}

func TestGapLibRefreshIdentitySkipsAndErrors(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()
	meta := &gapMetaProvider{}
	svc.meta = meta

	if err := svc.RefreshIdentity(ctx, 999999, false); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown item err = %v, want ErrNotFound", err)
	}

	// A manual entry has no provider to ask.
	manual, err := svc.AddManual(ctx, ManualRequest{Kind: domain.KindMovie, Title: "Home Video", Path: manualRoot(t, svc, domain.RootKindOf(domain.KindMovie), "Home Video")})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RefreshIdentity(ctx, manual.ID, true); err != nil {
		t.Errorf("manual refresh err = %v", err)
	}
	if meta.calls != 0 {
		t.Errorf("manual entry reached the provider: calls = %d", meta.calls)
	}

	// A book never has identity providers either.
	svc.WithBooks(fakeBooks{})
	book, err := svc.Add(ctx, AddRequest{Kind: domain.KindBook, OLID: "OL17091839W"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RefreshIdentity(ctx, book.ID, true); err != nil {
		t.Errorf("book refresh err = %v", err)
	}
	if meta.calls != 0 {
		t.Errorf("book reached the provider: calls = %d", meta.calls)
	}

	// A cancelled context is returned as such, without touching storage.
	item := gapAddMovie(t, svc, 550)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := svc.RefreshIdentity(cancelled, item.ID, true); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled err = %v", err)
	}
}

func TestGapLibRefreshIdentityCancelledMidFetch(t *testing.T) {
	svc, db, _ := newService(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	item := gapAddMovie(t, svc, 550)
	svc.meta = &gapMetaProvider{identity: func(domain.MediaKind, domain.ExternalIDs) (ports.IdentityMetadata, error) {
		cancel()
		return ports.IdentityMetadata{}, context.Canceled
	}}
	if err := svc.RefreshIdentity(ctx, item.ID, true); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	stored, err := db.GetMediaItemFull(context.Background(), item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.IdentitySources) != 0 {
		t.Errorf("a cancelled fetch must not be recorded as a failure: %+v", stored.IdentitySources)
	}
}

func TestGapLibIdentityProvidersOrderAndSelection(t *testing.T) {
	svc, _, _ := newService(t)
	svc.meta = &gapMetaProvider{}
	svc.WithSeriesProviders(gapSeriesProvider{chainProvider: chainProvider{name: "tvmaze"}})

	// A movie has a TMDB id only: the chain is not consulted.
	got := svc.identityProviders(domain.MediaItem{Kind: domain.KindMovie, Source: "tmdb", IDs: domain.ExternalIDs{TMDB: 1}})
	if len(got) != 1 || got[0].name != "tmdb" {
		t.Errorf("movie providers = %+v", got)
	}
	// A series hydrated from tvmaze with both ids: tvmaze comes first.
	got = svc.identityProviders(domain.MediaItem{Kind: domain.KindSeries, Source: "tvmaze", IDs: domain.ExternalIDs{TMDB: 1, TVDB: 2}})
	if len(got) != 2 || got[0].name != "tvmaze" || got[1].name != "tmdb" {
		t.Errorf("series providers = %+v, want tvmaze first", got)
	}
	// IMDB alone is enough for the chain, and no TMDB id means no TMDB.
	got = svc.identityProviders(domain.MediaItem{Kind: domain.KindSeries, IDs: domain.ExternalIDs{IMDB: "tt1"}})
	if len(got) != 1 || got[0].name != "tvmaze" {
		t.Errorf("imdb-only providers = %+v", got)
	}
	// No ids at all: nothing to ask.
	if got = svc.identityProviders(domain.MediaItem{Kind: domain.KindSeries}); len(got) != 0 {
		t.Errorf("id-less providers = %+v", got)
	}
}

func TestGapLibRefreshIdentityWalksTheChain(t *testing.T) {
	svc, db, _ := newService(t)
	ctx := context.Background()
	tvmazeCalls := 0
	svc.WithSeriesProviders(gapSeriesProvider{
		chainProvider: chainProvider{name: "tvmaze", results: []ports.SearchResult{chained(4141, "Cunk on Earth", 2022)}},
		identity: func(kind domain.MediaKind, ids domain.ExternalIDs) (ports.IdentityMetadata, error) {
			tvmazeCalls++
			if ids.TVDB != 4141 {
				return ports.IdentityMetadata{}, fmt.Errorf("wrong ids %+v", ids)
			}
			return ports.IdentityMetadata{Aliases: []domain.TitleAlias{gapAlias("Cunk sur Terre")}}, nil
		},
	})
	item, err := svc.Add(ctx, AddRequest{Kind: domain.KindSeries, TVDBID: 4141, HydrationSource: "tvmaze"})
	if err != nil {
		t.Fatal(err)
	}
	if item.Source != "tvmaze" {
		t.Fatalf("source = %q", item.Source)
	}
	// Add already refreshed inline (no queue); the snapshot is there.
	if tvmazeCalls != 1 {
		t.Errorf("tvmaze identity calls after add = %d, want 1", tvmazeCalls)
	}
	stored, err := db.GetMediaItemFull(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range stored.Aliases {
		if a.Source == "tvmaze" && a.Title == "Cunk sur Terre" {
			found = true
		}
	}
	if !found {
		t.Errorf("chain alias not stored: %+v", stored.Aliases)
	}
}

// ---- SweepIdentity / enqueue ----

func TestGapLibSweepIdentityQueuesStaleSkipsFresh(t *testing.T) {
	svc, db, _ := newService(t)
	ctx := context.Background()
	// Adds run with a provider that has no identity capability, so nothing
	// is refreshed inline and every item starts without a snapshot.
	never := gapAddMovie(t, svc, 1)
	fresh := gapAddMovie(t, svc, 2)
	stale := gapAddMovie(t, svc, 3)
	failedRetryOpen := gapAddMovie(t, svc, 4)
	failedRetryDue := gapAddMovie(t, svc, 5)
	manual, err := svc.AddManual(ctx, ManualRequest{Kind: domain.KindMovie, Title: "Home Video", Path: manualRoot(t, svc, domain.RootKindOf(domain.KindMovie), "Home Video")})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := db.ReplaceIdentitySnapshot(ctx, fresh.ID, "tmdb", nil, nil, now); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceIdentitySnapshot(ctx, stale.ID, "tmdb", nil, nil, now.Add(-identityFreshFor-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordIdentityFailure(ctx, failedRetryOpen.ID, "tmdb", now, now.Add(time.Hour), "503"); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordIdentityFailure(ctx, failedRetryDue.ID, "tmdb", now.Add(-2*time.Hour), now.Add(-time.Hour), "503"); err != nil {
		t.Fatal(err)
	}

	svc.meta = &gapMetaProvider{}
	q := &fakeQueue{}
	svc.WithQueue(q)
	if err := svc.SweepIdentity(ctx); err != nil {
		t.Fatal(err)
	}
	got := map[string]domain.Job{}
	for _, j := range q.enqueued {
		got[j.DedupeKey] = j
	}
	want := []int64{never.ID, stale.ID, failedRetryDue.ID}
	for _, id := range want {
		key := fmt.Sprintf("%s:%d", JobIdentityRefresh, id)
		j, ok := got[key]
		if !ok {
			t.Errorf("item %d should have been queued; queued = %v", id, q.kinds())
			continue
		}
		if j.Kind != JobIdentityRefresh || j.Priority != 80 || j.Payload != fmt.Sprintf(`{"itemId":%d}`, id) {
			t.Errorf("job for %d = %+v", id, j)
		}
	}
	for _, id := range []int64{fresh.ID, failedRetryOpen.ID, manual.ID} {
		if _, ok := got[fmt.Sprintf("%s:%d", JobIdentityRefresh, id)]; ok {
			t.Errorf("item %d should not have been queued", id)
		}
	}
	if len(q.enqueued) != len(want) {
		t.Errorf("queued %d jobs, want %d: %v", len(q.enqueued), len(want), q.kinds())
	}

	// Already-queued items coalesce rather than count.
	q2 := &fakeQueue{dupes: map[string]bool{fmt.Sprintf("%s:%d", JobIdentityRefresh, never.ID): true}}
	svc.WithQueue(q2)
	if err := svc.SweepIdentity(ctx); err != nil {
		t.Fatal(err)
	}
	if len(q2.enqueued) != len(want)-1 {
		t.Errorf("second sweep queued %d, want %d", len(q2.enqueued), len(want)-1)
	}
}

func TestGapLibSweepIdentityNothingToDoAndErrors(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()
	gapAddMovie(t, svc, 1)
	q := &fakeQueue{}
	svc.WithQueue(q)

	// No identity-capable provider: nothing is ever queued.
	if err := svc.SweepIdentity(ctx); err != nil {
		t.Fatal(err)
	}
	if len(q.enqueued) != 0 {
		t.Errorf("queued without an identity provider: %v", q.kinds())
	}

	svc.meta = &gapMetaProvider{}
	svc.WithQueue(&fakeQueue{err: errors.New("queue down")})
	if err := svc.SweepIdentity(ctx); err == nil || !strings.Contains(err.Error(), "queue down") {
		t.Errorf("queue error should surface, got %v", err)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	svc.WithQueue(q)
	if err := svc.SweepIdentity(cancelled); err == nil {
		t.Error("cancelled sweep should report the context error")
	}

	// Cancellation that lands mid-sweep stops the walk after the current
	// item rather than draining the rest of the library into the queue.
	gapAddMovie(t, svc, 2)
	gapAddMovie(t, svc, 3)
	midway, cancelMidway := context.WithCancel(ctx)
	defer cancelMidway()
	cq := &gapCancellingQueue{cancel: cancelMidway}
	svc.WithQueue(cq)
	if err := svc.SweepIdentity(midway); !errors.Is(err, context.Canceled) {
		t.Errorf("mid-sweep cancel err = %v", err)
	}
	if cq.calls != 1 {
		t.Errorf("sweep continued after cancellation: %d enqueues", cq.calls)
	}
}

// gapCancellingQueue accepts one job and cancels the sweep's context.
type gapCancellingQueue struct {
	cancel context.CancelFunc
	calls  int
}

func (q *gapCancellingQueue) EnqueueUnique(context.Context, domain.Job) (bool, error) {
	q.calls++
	q.cancel()
	return true, nil
}

func TestGapLibEnqueueIdentityRefreshPriorities(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()
	item := gapAddMovie(t, svc, 550)
	q := &fakeQueue{}
	svc.WithQueue(q)

	if err := svc.EnqueueIdentityRefresh(ctx, item.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := svc.EnqueueIdentityRefresh(ctx, item.ID, true); err != nil {
		t.Fatal(err)
	}
	if len(q.enqueued) != 2 {
		t.Fatalf("enqueued = %v", q.enqueued)
	}
	if q.enqueued[0].Priority != 80 || q.enqueued[1].Priority != 40 {
		t.Errorf("priorities = %d, %d; want 80 (background) and 40 (explicit)", q.enqueued[0].Priority, q.enqueued[1].Priority)
	}
	for _, j := range q.enqueued {
		if j.Kind != JobIdentityRefresh || j.DedupeKey != fmt.Sprintf("%s:%d", JobIdentityRefresh, item.ID) {
			t.Errorf("job = %+v", j)
		}
	}

	svc.WithQueue(&fakeQueue{err: errors.New("queue down")})
	if err := svc.EnqueueIdentityRefresh(ctx, item.ID, true); err == nil {
		t.Error("queue error should surface")
	}

	// Without a queue the refresh runs inline: a bad id is an error.
	svc.queue = nil
	if err := svc.EnqueueIdentityRefresh(ctx, 999999, true); !errors.Is(err, ErrNotFound) {
		t.Errorf("inline refresh of unknown item err = %v", err)
	}
}

// ---- RegisterJobHandlers ----

func TestGapLibRegisterJobHandlersIdentityAndProbe(t *testing.T) {
	svc, db, _ := newService(t)
	ctx := context.Background()
	item := gapAddMovie(t, svc, 550)
	meta := &gapMetaProvider{identity: func(domain.MediaKind, domain.ExternalIDs) (ports.IdentityMetadata, error) {
		return ports.IdentityMetadata{Aliases: []domain.TitleAlias{gapAlias("Bökskamp")}}, nil
	}}
	svc.meta = meta

	handlers := map[string]func(context.Context, domain.Job) error{}
	if err := RegisterJobHandlers(svc, func(kind string, h func(context.Context, domain.Job) error) error {
		handlers[kind] = h
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{JobScan, JobAdopt, JobIdentityRefresh, JobProbe} {
		if handlers[kind] == nil {
			t.Fatalf("no handler for %q", kind)
		}
	}

	identity := handlers[JobIdentityRefresh]
	if err := identity(ctx, domain.Job{Kind: JobIdentityRefresh, Payload: "not json"}); err == nil || !strings.Contains(err.Error(), "payload") {
		t.Errorf("bad payload err = %v", err)
	}
	if err := identity(ctx, domain.Job{Kind: JobIdentityRefresh, Payload: `{"itemId":0}`}); err == nil || !strings.Contains(err.Error(), "no item id") {
		t.Errorf("zero id err = %v", err)
	}
	if err := identity(ctx, domain.Job{Kind: JobIdentityRefresh, Payload: fmt.Sprintf(`{"itemId":%d}`, item.ID)}); err != nil {
		t.Errorf("valid identity job err = %v", err)
	}
	if meta.calls != 1 {
		t.Errorf("identity provider calls = %d, want 1", meta.calls)
	}
	if s := gapSourceStatus(t, db, item.ID, "tmdb"); s.FetchedAt.IsZero() {
		t.Errorf("identity job did not store a snapshot: %+v", s)
	}

	probe := handlers[JobProbe]
	if err := probe(ctx, domain.Job{Kind: JobProbe, Payload: "{"}); err == nil || !strings.Contains(err.Error(), "payload") {
		t.Errorf("bad probe payload err = %v", err)
	}
	if err := probe(ctx, domain.Job{Kind: JobProbe, Payload: `{"fileId":0}`}); err == nil || !strings.Contains(err.Error(), "no file id") {
		t.Errorf("zero file id err = %v", err)
	}
	// A file row that vanished between enqueue and run is a result, not a failure.
	if err := probe(ctx, domain.Job{Kind: JobProbe, Payload: `{"fileId":987654}`}); err != nil {
		t.Errorf("vanished file err = %v", err)
	}

	// The adopt handler runs with nothing to adopt and returns cleanly.
	if err := handlers[JobAdopt](ctx, domain.Job{Kind: JobAdopt}); err != nil {
		t.Errorf("adopt handler err = %v", err)
	}
}

func TestGapLibRegisterJobHandlersPropagatesRegistrationFailure(t *testing.T) {
	svc, _, _ := newService(t)
	for _, failing := range []string{JobScan, JobAdopt, JobIdentityRefresh, JobProbe} {
		registered := []string{}
		err := RegisterJobHandlers(svc, func(kind string, _ func(context.Context, domain.Job) error) error {
			if kind == failing {
				return fmt.Errorf("refused %s", kind)
			}
			registered = append(registered, kind)
			return nil
		})
		if err == nil || !strings.Contains(err.Error(), failing) {
			t.Errorf("failing %s: err = %v", failing, err)
		}
		for _, kind := range registered {
			if kind == failing {
				t.Errorf("%s registered despite refusal", kind)
			}
		}
	}
}

// ---- EnqueueProbe ----

func TestGapLibEnqueueProbeWithQueue(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()
	q := &fakeQueue{}
	svc.WithQueue(q)
	if err := svc.EnqueueProbe(ctx, 7); err != nil {
		t.Fatal(err)
	}
	if len(q.enqueued) != 1 {
		t.Fatalf("enqueued = %v", q.enqueued)
	}
	j := q.enqueued[0]
	if j.Kind != JobProbe || j.DedupeKey != "probe:7" || j.Priority != 80 {
		t.Errorf("job = %+v", j)
	}
	var p probePayload
	if err := json.Unmarshal([]byte(j.Payload), &p); err != nil || p.FileID != 7 {
		t.Errorf("payload %q -> %+v, %v", j.Payload, p, err)
	}
	// Coalesced: a second ask for the same file is a no-op.
	q.dupes = map[string]bool{"probe:7": true}
	if err := svc.EnqueueProbe(ctx, 7); err != nil || len(q.enqueued) != 1 {
		t.Errorf("coalesced probe: err=%v enqueued=%d", err, len(q.enqueued))
	}
	svc.WithQueue(&fakeQueue{err: errors.New("queue down")})
	if err := svc.EnqueueProbe(ctx, 7); err == nil || !strings.Contains(err.Error(), "enqueue probe") {
		t.Errorf("queue error should be wrapped, got %v", err)
	}
}

// ---- ResolveExternal ----

func TestGapLibResolveExternalLocalHit(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()
	item := gapAddMovie(t, svc, 550)

	got, err := svc.ResolveExternal(ctx, domain.KindMovie, domain.ExternalRef{Provider: "tmdb", Value: "550"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].TMDBID != 550 || got[0].Title != item.Title || got[0].Source != "tmdb" || got[0].HydrationSource != "tmdb" {
		t.Errorf("local hit = %+v", got)
	}
	// Search recognises the id syntax and routes here rather than to text search.
	viaSearch, err := svc.Search(ctx, domain.KindMovie, "tmdb:550")
	if err != nil {
		t.Fatal(err)
	}
	if len(viaSearch) != 1 || viaSearch[0].TMDBID != 550 {
		t.Errorf("search by id = %+v", viaSearch)
	}
	if _, err := svc.Search(ctx, domain.KindMovie, "tmdb:abc"); !errors.Is(err, ErrInvalidExternalID) {
		t.Errorf("malformed id err = %v", err)
	}
	// An unknown provider namespace is a storage-layer refusal.
	if _, err := svc.ResolveExternal(ctx, domain.KindMovie, domain.ExternalRef{Provider: "nope", Value: "1"}); err == nil {
		t.Error("unknown provider should error")
	}
	// Nothing local and no exact-id capability anywhere.
	if _, err := svc.ResolveExternal(ctx, domain.KindMovie, domain.ExternalRef{Provider: "tmdb", Value: "551"}); !errors.Is(err, ports.ErrProviderNotConfigured) {
		t.Errorf("no lookup provider err = %v", err)
	}
}

func TestGapLibResolveExternalProviderOutcomes(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()
	ref := domain.ExternalRef{Provider: "tmdb", Value: "551"}
	meta := &gapMetaProvider{}
	svc.meta = meta
	remote := func(category string) error {
		return &ports.RemoteError{Category: category, HTTPStatus: 500}
	}

	cases := []struct {
		name         string
		results      []ports.SearchResult
		err          error
		wantErr      error
		wantConflict bool
		wantN        int
		wantNil      bool
	}{
		{name: "one addable result", results: []ports.SearchResult{movie(551, "Se7en", 1995)}, wantN: 1},
		{name: "two results is ambiguity", results: []ports.SearchResult{movie(551, "A", 1), movie(552, "B", 2)}, wantErr: sqlite.ErrIdentityConflict, wantConflict: true},
		{name: "no results is nothing", wantNil: true},
		{name: "not addable", results: []ports.SearchResult{{Kind: domain.KindMovie, Title: "No id"}}, wantErr: ErrUnsupportedHydration},
		{name: "unconfigured is skipped", err: ports.ErrProviderNotConfigured, wantNil: true},
		{name: "remote not found", err: remote(ports.RemoteNotFound), wantNil: true},
		{name: "remote unsupported query", err: remote(ports.RemoteUnsupportedQuery), wantErr: ErrUnsupportedHydration},
		{name: "remote unsupported hydration", err: remote(ports.RemoteUnsupportedHydration), wantErr: ErrUnsupportedHydration},
		{name: "remote identity conflict", err: remote(ports.RemoteIdentityConflict), wantConflict: true},
		{name: "remote transport", err: remote(ports.RemoteTransport), wantErr: ErrProviderUnavailable},
		{name: "plain error", err: errors.New("dns"), wantErr: ErrProviderUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meta.lookup = func(domain.MediaKind, domain.ExternalRef) ([]ports.SearchResult, error) {
				return tc.results, tc.err
			}
			got, err := svc.ResolveExternal(ctx, domain.KindMovie, ref)
			if tc.wantConflict {
				var conflict *IdentityConflictError
				if !errors.As(err, &conflict) {
					t.Fatalf("err = %v (%T), want *IdentityConflictError", err, err)
				}
				if tc.err != nil && !errors.Is(err, tc.err) {
					t.Errorf("conflict should wrap the provider error: %v", err)
				}
			}
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if tc.wantConflict {
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantNil && got != nil {
				t.Errorf("got = %+v, want nil", got)
			}
			if len(got) != tc.wantN {
				t.Errorf("got %d results, want %d: %+v", len(got), tc.wantN, got)
			}
		})
	}
}

func TestGapLibResolveExternalSeriesChainFirst(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()
	meta := &gapMetaProvider{lookup: func(domain.MediaKind, domain.ExternalRef) ([]ports.SearchResult, error) {
		return []ports.SearchResult{series(9, "From TMDB", 2020)}, nil
	}}
	svc.meta = meta
	chainAsked := 0
	svc.WithSeriesProviders(gapSeriesProvider{
		chainProvider: chainProvider{name: "tvmaze"},
		lookup: func(kind domain.MediaKind, ref domain.ExternalRef) ([]ports.SearchResult, error) {
			chainAsked++
			if ref.Provider == "tvdb" {
				return []ports.SearchResult{chained(4141, "From TVmaze", 2022)}, nil
			}
			return nil, &ports.RemoteError{Category: ports.RemoteNotFound}
		},
	})

	got, err := svc.ResolveExternal(ctx, domain.KindSeries, domain.ExternalRef{Provider: "tvdb", Value: "4141"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].TVDBID != 4141 || got[0].Source != "tvmaze" {
		t.Errorf("tvdb ref should be answered by the chain, got %+v", got)
	}
	if chainAsked != 1 || meta.calls != 0 {
		t.Errorf("chain asked %d times, tmdb %d; want 1 and 0", chainAsked, meta.calls)
	}
	// IMDB: the chain has nothing, so TMDB answers.
	got, err = svc.ResolveExternal(ctx, domain.KindSeries, domain.ExternalRef{Provider: "imdb", Value: "tt0000001"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].TMDBID != 9 {
		t.Errorf("imdb fallthrough = %+v", got)
	}
	if chainAsked != 2 || meta.calls != 1 {
		t.Errorf("chain asked %d, tmdb %d; want 2 and 1", chainAsked, meta.calls)
	}
	// A TMDB ref for a series never involves the chain.
	if _, err := svc.ResolveExternal(ctx, domain.KindSeries, domain.ExternalRef{Provider: "tmdb", Value: "9"}); err != nil {
		t.Fatal(err)
	}
	if chainAsked != 2 {
		t.Errorf("chain consulted for a tmdb ref")
	}
}

// ---- hydrateAdd / WithSeriesProviders ----

func TestGapLibHydrateAddRoutes(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()
	hydrates := 0
	svc.WithSeriesProviders(chainProvider{
		name: "tvmaze", results: []ports.SearchResult{chained(4141, "Cunk on Earth", 2022)}, hydrates: &hydrates,
	})
	if len(svc.series) != 1 || svc.series[0].Name() != "tvmaze" {
		t.Fatalf("series chain = %+v", svc.series)
	}

	// Explicit tmdb source, both kinds.
	m, err := svc.Add(ctx, AddRequest{Kind: domain.KindMovie, TMDBID: 550, HydrationSource: "tmdb"})
	if err != nil || m.Source != "tmdb" || m.Title != "Fight Club" {
		t.Errorf("tmdb movie = %+v, %v", m, err)
	}
	s, err := svc.Add(ctx, AddRequest{Kind: domain.KindSeries, TMDBID: 100, HydrationSource: "tmdb"})
	if err != nil || s.Source != "tmdb" || len(s.Seasons) != 1 {
		t.Errorf("tmdb series = %+v, %v", s, err)
	}
	// A movie cannot be hydrated by a series-only link.
	if _, err := svc.Add(ctx, AddRequest{Kind: domain.KindMovie, TMDBID: 551, HydrationSource: "tvmaze"}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("movie via tvmaze err = %v, want ErrInvalidInput", err)
	}
	// A source nobody configured.
	if _, err := svc.Add(ctx, AddRequest{Kind: domain.KindSeries, TVDBID: 4141, HydrationSource: "thetvdb"}); !errors.Is(err, ErrProviderUnavailable) {
		t.Errorf("unconfigured source err = %v, want ErrProviderUnavailable", err)
	}
	// The named link hydrates and stamps itself as the source.
	c, err := svc.Add(ctx, AddRequest{Kind: domain.KindSeries, TVDBID: 4141, HydrationSource: "tvmaze"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Source != "tvmaze" || c.IDs.TVDB != 4141 || c.Title != "Cunk on Earth" {
		t.Errorf("tvmaze series = %+v", c)
	}
	if hydrates != 1 {
		t.Errorf("hydrates = %d, want 1", hydrates)
	}
	// The named link does not know the id: its error comes straight back.
	if _, err := svc.Add(ctx, AddRequest{Kind: domain.KindSeries, TVDBID: 999, HydrationSource: "tvmaze"}); err == nil {
		t.Error("unknown tvdb id through a named link should error")
	}
	// Legacy request: no source, TVDB id only, walks the chain.
	if _, err := svc.Add(ctx, AddRequest{Kind: domain.KindSeries, TVDBID: 999}); !errors.Is(err, ErrNotFound) {
		t.Errorf("legacy unknown tvdb err = %v, want ErrNotFound", err)
	}
}

// ---- SharedFolders ----

func TestGapLibSharedFolders(t *testing.T) {
	svc, db, _ := newService(t)
	ctx := context.Background()
	root := t.TempDir()
	rf, err := svc.AddRootFolder(ctx, root, domain.KindMixed)
	if err != nil {
		t.Fatal(err)
	}
	// fakeProvider answers "Fight Club (1999)" for every movie id, so two
	// distinct ids render to one folder name. They must NOT share it.
	a, err := svc.Add(ctx, AddRequest{Kind: domain.KindMovie, TMDBID: 550, RootFolderID: rf.ID})
	if err != nil {
		t.Fatal(err)
	}
	b, err := svc.Add(ctx, AddRequest{Kind: domain.KindMovie, TMDBID: 551, RootFolderID: rf.ID})
	if err != nil {
		t.Fatal(err)
	}
	if a.Path == b.Path {
		t.Fatalf("two different films were given one folder: %s", a.Path)
	}
	shared, err := svc.SharedFolders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(shared) != 0 {
		t.Fatalf("shared = %+v, want none", shared)
	}

	// What the check still exists for: a copy folder written before the copy
	// path checked other items. Plant one directly.
	if _, err := db.W.ExecContext(ctx, `INSERT INTO media_copies
		(media_item_id, name, quality_profile_id, monitored, root_folder_id, path, added_at)
		VALUES (?, 'x', ?, 1, ?, ?, 0)`, b.ID, b.QualityProfileID, rf.ID, a.Path); err != nil {
		t.Fatal(err)
	}
	shared, err = svc.SharedFolders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	labels, ok := shared[a.Path]
	if len(shared) != 1 || !ok || len(labels) != 2 {
		t.Fatalf("shared = %+v, want %s held by both items", shared, a.Path)
	}
	for _, want := range []string{fmt.Sprintf("(item %d)", a.ID), fmt.Sprintf("(item %d)", b.ID)} {
		if !strings.Contains(strings.Join(labels, " "), want) {
			t.Errorf("labels %v do not name %s — two identical titles are useless in the warning", labels, want)
		}
	}
}

// ---- RefreshAll ----

func TestGapLibRefreshAllSkipsManualAndReportsFirstError(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()
	svc.WithBooks(fakeBooks{})
	svc.WithSeriesProviders(chainProvider{
		name: "tvmaze", results: []ports.SearchResult{chained(4141, "Cunk on Earth", 2022)},
	})
	gapAddMovie(t, svc, 550)
	if _, err := svc.AddManual(ctx, ManualRequest{Kind: domain.KindMovie, Title: "Home Video", Path: manualRoot(t, svc, domain.RootKindOf(domain.KindMovie), "Home Video")}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Add(ctx, AddRequest{Kind: domain.KindBook, OLID: "OL17091839W"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Add(ctx, AddRequest{Kind: domain.KindSeries, TVDBID: 4141, HydrationSource: "tvmaze"}); err != nil {
		t.Fatal(err)
	}

	// Everything configured: a clean pass.
	if err := svc.RefreshAll(ctx); err != nil {
		t.Fatalf("RefreshAll = %v", err)
	}

	// Books without a provider are a setup state and skipped; the series
	// whose chain link vanished is the reported failure.
	svc.books = nil
	svc.series = nil
	err := svc.RefreshAll(ctx)
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("RefreshAll = %v, want ErrProviderUnavailable for the orphaned series", err)
	}
	if !strings.Contains(err.Error(), "Cunk on Earth") {
		t.Errorf("error should name the item: %v", err)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := svc.RefreshAll(cancelled); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled RefreshAll = %v", err)
	}
}

// ---- adopt helpers ----

func TestGapLibSearchTop(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()
	if got := svc.searchTop(ctx, domain.KindMovie, "   "); got != nil {
		t.Errorf("blank query = %+v, want nil", got)
	}
	if got := svc.searchTop(ctx, domain.KindMovie, "fight club"); len(got) != 1 || got[0].TMDBID != 550 {
		t.Errorf("movie search = %+v", got)
	}
	svc.meta = gapFailingProvider{}
	if got := svc.searchTop(ctx, domain.KindMovie, "fight club"); got != nil {
		t.Errorf("a failed search is no candidates, got %+v", got)
	}
}

func TestGapLibClearsRest(t *testing.T) {
	cases := []struct {
		name   string
		kind   domain.MediaKind
		parsed parser.Parsed
		r      ports.SearchResult
		want   bool
	}{
		{"movie needs folder year", domain.KindMovie, parser.Parsed{}, movie(1, "Dune", 2021), false},
		{"movie needs result year", domain.KindMovie, parser.Parsed{Year: 2021}, movie(1, "Dune", 0), false},
		{"movie year within one", domain.KindMovie, parser.Parsed{Year: 2020}, movie(1, "Dune", 2021), true},
		{"movie year off by two", domain.KindMovie, parser.Parsed{Year: 2019}, movie(1, "Dune", 2021), false},
		{"series without years", domain.KindSeries, parser.Parsed{}, series(1, "Andor", 0), true},
		{"series folder year only", domain.KindSeries, parser.Parsed{Year: 2022}, series(1, "Andor", 0), true},
		{"series years agree", domain.KindSeries, parser.Parsed{Year: 2022}, series(1, "Andor", 2023), true},
		{"series years disagree", domain.KindSeries, parser.Parsed{Year: 2010}, series(1, "Andor", 2022), false},
		{"book needs folder author", domain.KindBook, parser.Parsed{}, ports.SearchResult{Kind: domain.KindBook, Author: "Andy Weir"}, false},
		{"book needs result author", domain.KindBook, parser.Parsed{Author: "Andy Weir"}, ports.SearchResult{Kind: domain.KindBook}, false},
		{"book authors match", domain.KindBook, parser.Parsed{Author: "andy weir"}, ports.SearchResult{Kind: domain.KindBook, Author: "Andy Weir"}, true},
		{"book authors differ", domain.KindBook, parser.Parsed{Author: "Frank Herbert"}, ports.SearchResult{Kind: domain.KindBook, Author: "Andy Weir"}, false},
		{"unknown kind", domain.MediaKind("podcast"), parser.Parsed{Year: 2020}, movie(1, "x", 2020), false},
	}
	for _, tc := range cases {
		if got := clearsRest(tc.kind, tc.parsed, tc.r); got != tc.want {
			t.Errorf("%s: clearsRest = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// ---- review queue ----

func TestGapLibSaveReviewQueueRoundTrips(t *testing.T) {
	svc, db, _ := newService(t)
	ctx := context.Background()
	in := []Proposal{
		{RootFolderID: 1, Path: "/x/Dune (2021)", Name: "Dune (2021)", Confidence: ConfidenceExact},
		{RootFolderID: 1, Path: "/x/Unknown", Name: "Unknown"},
	}
	svc.saveReviewQueue(ctx, in)
	raw, err := db.GetMeta(ctx, reviewQueueKey)
	if err != nil {
		t.Fatal(err)
	}
	var out []Proposal
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0].Name != "Dune (2021)" || out[0].Confidence != ConfidenceExact || out[1].Name != "Unknown" {
		t.Errorf("persisted queue = %+v", out)
	}
	// An empty queue is stored as an empty list, not left stale.
	svc.saveReviewQueue(ctx, nil)
	raw, err = db.GetMeta(ctx, reviewQueueKey)
	if err != nil {
		t.Fatal(err)
	}
	if raw != "null" && raw != "[]" {
		t.Errorf("empty queue stored as %q", raw)
	}
	// Persisting is best-effort: a failed write is logged and the stored
	// queue is left as it was rather than the adoption failing.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	svc.saveReviewQueue(cancelled, in)
	after, err := db.GetMeta(ctx, reviewQueueKey)
	if err != nil {
		t.Fatal(err)
	}
	if after != raw {
		t.Errorf("a failed save changed the stored queue: %q -> %q", raw, after)
	}
}
