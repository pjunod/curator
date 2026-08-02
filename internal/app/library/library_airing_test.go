package library

import (
	"context"
	"errors"
	"testing"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/bus"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// fakeAiring answers with whatever the test loaded, or the error it loaded.
type fakeAiring struct {
	out   ports.Airing
	err   error
	calls int
	sawID int64
}

func (f *fakeAiring) Airing(_ context.Context, tvdbID int64, _ string) (ports.Airing, error) {
	f.calls++
	f.sawID = tvdbID
	return f.out, f.err
}

func airingService(t *testing.T, ap ports.AiringProvider) (*Service, *sqlite.DB) {
	t.Helper()
	db, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	b := bus.New(nil)
	t.Cleanup(b.Close)
	series := seriesV1()
	series.IDs.TVDB = 414217
	movie := domain.MediaItem{
		Kind: domain.KindMovie, Title: "Fight Club", SortTitle: "fight club",
		Year: 1999, IDs: domain.ExternalIDs{TMDB: 550, IMDB: "tt0137523"},
	}
	svc := New(db, &mutableProvider{series: series, movie: movie}, b, nil)
	if ap != nil {
		svc = svc.WithAiring(ap)
	}
	return svc, db
}

func TestAiringEnrichmentStoresTheSlot(t *testing.T) {
	fa := &fakeAiring{out: ports.Airing{
		Time: "21:00", Timezone: "America/New_York", Network: "HBO",
	}}
	svc, _ := airingService(t, fa)
	ctx := context.Background()

	item, err := svc.Add(ctx, AddRequest{Kind: domain.KindSeries, TMDBID: 100, Monitored: true})
	if err != nil {
		t.Fatal(err)
	}
	if item.AirsTime != "21:00" || item.AirsTimezone != "America/New_York" || item.Network != "HBO" {
		t.Fatalf("slot not stored on add: %q %q %q", item.AirsTime, item.AirsTimezone, item.Network)
	}
	if fa.sawID != 414217 {
		t.Errorf("looked up tvdb %d, want the item's id", fa.sawID)
	}

	// A schedule change survives a refresh — the metadata provider knows
	// nothing about slots, so refresh must not let its zeroes win.
	fa.out = ports.Airing{Time: "22:00", Timezone: "America/New_York", Network: "HBO"}
	got, err := svc.RefreshItem(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AirsTime != "22:00" {
		t.Errorf("refresh did not pick up the new slot: %q", got.AirsTime)
	}
}

func TestAiringFailureLeavesTheItemRefreshedAndTheSlotIntact(t *testing.T) {
	fa := &fakeAiring{out: ports.Airing{
		Time: "21:00", Timezone: "America/New_York", Network: "HBO",
	}}
	svc, _ := airingService(t, fa)
	ctx := context.Background()

	item, err := svc.Add(ctx, AddRequest{Kind: domain.KindSeries, TMDBID: 100, Monitored: true})
	if err != nil {
		t.Fatal(err)
	}

	// The provider goes down. The refresh must still succeed, and must not
	// blank a slot it already knew — that is the difference between "the
	// show has no schedule" and "we could not ask today".
	fa.err = errors.New("tvmaze: 503")
	got, err := svc.RefreshItem(ctx, item.ID)
	if err != nil {
		t.Fatalf("an airing failure must not fail the refresh: %v", err)
	}
	if got.AirsTime != "21:00" || got.Network != "HBO" {
		t.Errorf("a failed lookup erased the known slot: %q %q", got.AirsTime, got.Network)
	}

	// An unconfigured provider is the same story, with no log line.
	fa.err = ports.ErrProviderNotConfigured
	if _, err := svc.RefreshItem(ctx, item.ID); err != nil {
		t.Fatalf("unconfigured provider must not fail the refresh: %v", err)
	}
}

func TestAiringEmptyAnswerClearsTheSlot(t *testing.T) {
	fa := &fakeAiring{out: ports.Airing{
		Time: "21:00", Timezone: "America/New_York", Network: "HBO",
	}}
	svc, _ := airingService(t, fa)
	ctx := context.Background()
	item, err := svc.Add(ctx, AddRequest{Kind: domain.KindSeries, TMDBID: 100, Monitored: true})
	if err != nil {
		t.Fatal(err)
	}

	// The show moved to streaming: TVmaze now publishes no slot. Keeping the
	// old one would put it on the calendar at 9 PM forever.
	fa.out = ports.Airing{Network: "Netflix"}
	got, err := svc.RefreshItem(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AirsTime != "" || got.AirsTimezone != "" || got.Network != "Netflix" {
		t.Errorf("slot not cleared: %q %q %q", got.AirsTime, got.AirsTimezone, got.Network)
	}
}

func TestAiringIsNotConsultedForMovies(t *testing.T) {
	fa := &fakeAiring{}
	svc, _ := airingService(t, fa)
	if _, err := svc.Add(context.Background(),
		AddRequest{Kind: domain.KindMovie, TMDBID: 550, Monitored: true}); err != nil {
		t.Fatal(err)
	}
	if fa.calls != 0 {
		t.Errorf("a movie has no weekly slot; provider called %d times", fa.calls)
	}
}

func TestNoAiringProviderIsTheOldBehaviour(t *testing.T) {
	svc, _ := airingService(t, nil)
	item, err := svc.Add(context.Background(),
		AddRequest{Kind: domain.KindSeries, TMDBID: 100, Monitored: true})
	if err != nil {
		t.Fatalf("no provider must not break an add: %v", err)
	}
	if item.AirsTime != "" || item.Network != "" {
		t.Errorf("fields invented with no provider: %+v", item)
	}
}
