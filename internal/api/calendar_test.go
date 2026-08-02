package api

import (
	"context"
	"net/http"
	"testing"

	apigen "github.com/pjunod/monarr/internal/api/gen"
	"github.com/pjunod/monarr/internal/domain"
)

// The DST proof (ADR 0016). The same show, the same stored slot, two dates
// six months apart: the UTC instant differs by an hour because the offset
// does. This is the whole reason the instant is composed per date instead of
// computed once at refresh and stored.
func TestComposeAirTimeFollowsDST(t *testing.T) {
	for _, tc := range []struct {
		date, want string
	}{
		{"2026-01-11", "2026-01-12T02:00:00Z"}, // EST, UTC-5
		{"2026-07-12", "2026-07-13T01:00:00Z"}, // EDT, UTC-4
	} {
		got, ok := composeAirTime(tc.date, "21:00", "America/New_York")
		if !ok {
			t.Fatalf("%s: composition failed", tc.date)
		}
		if got.Format("2006-01-02T15:04:05Z") != tc.want {
			t.Errorf("%s 21:00 America/New_York = %s, want %s",
				tc.date, got.Format("2006-01-02T15:04:05Z"), tc.want)
		}
	}
}

// Anything missing or unparsable means no time at all. A midnight-looking
// instant that actually means "unknown" would sort ahead of every real
// evening slot and read as a claim.
func TestComposeAirTimeRefusesToGuess(t *testing.T) {
	for name, tc := range map[string]struct{ date, at, tz string }{
		"no time":        {"2026-01-11", "", "America/New_York"},
		"no zone":        {"2026-01-11", "21:00", ""},
		"no date":        {"", "21:00", "America/New_York"},
		"unknown zone":   {"2026-01-11", "21:00", "Mars/Olympus"},
		"junk time":      {"2026-01-11", "9pm", "America/New_York"},
		"junk date":      {"11/01/2026", "21:00", "America/New_York"},
		"empty all over": {"", "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok := composeAirTime(tc.date, tc.at, tc.tz); ok {
				t.Error("composed a time from incomplete input")
			}
		})
	}
}

// An episode entry carries the rebuilt calendar's presentation fields, and
// the fields that were there before are byte-identical — plurx parses
// `detail` in production (internal/api/callers.go).
func TestAcqCalendarEpisodeCarriesTheAiringFields(t *testing.T) {
	e := newAPIEnv(t)
	ctx := context.Background()

	id, err := e.db.CreateMediaItem(ctx, domain.MediaItem{
		Kind: domain.KindSeries, Title: "Test Show", SortTitle: "test show",
		Year: 2026, Monitored: true, PosterPath: "/poster.jpg", Runtime: 34,
		IDs:      domain.ExternalIDs{TMDB: 100, IMDB: "tt0000100"},
		AirsTime: "21:00", AirsTimezone: "America/New_York", Network: "E2E One",
		Seasons: []domain.Season{{Number: 2, Monitored: true, Episodes: []domain.Episode{
			{SeasonNumber: 2, EpisodeNumber: 4, Title: "Pilot", AirDate: "2026-01-11", Monitored: true},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	var entries []apigen.CalendarEntry
	e.get(t, "/api/v1/calendar?start=2026-01-01&end=2026-01-31").
		expect(t, http.StatusOK).into(t, &entries)
	if len(entries) != 1 {
		t.Fatalf("got %d entries: %+v", len(entries), entries)
	}
	got := entries[0]

	// The frozen half.
	if got.Date != "2026-01-11" || got.Kind != "episode" || got.MediaItemId != id {
		t.Errorf("existing fields changed: %+v", got)
	}
	if got.Detail != "S02E04 — Pilot" {
		t.Errorf("detail = %q — its format is frozen, plurx parses it", got.Detail)
	}

	// The additive half.
	if got.AirDateUtc == nil {
		t.Fatal("airDateUtc missing for a show with a stored slot")
	}
	if s := got.AirDateUtc.UTC().Format("2006-01-02T15:04:05Z"); s != "2026-01-12T02:00:00Z" {
		t.Errorf("airDateUtc = %s, want the composed EST instant", s)
	}
	if got.Network == nil || *got.Network != "E2E One" {
		t.Errorf("network = %v", got.Network)
	}
	if got.SeasonNumber == nil || *got.SeasonNumber != 2 ||
		got.EpisodeNumber == nil || *got.EpisodeNumber != 4 {
		t.Errorf("season/episode = %v/%v", got.SeasonNumber, got.EpisodeNumber)
	}
	if got.EpisodeTitle == nil || *got.EpisodeTitle != "Pilot" {
		t.Errorf("episodeTitle = %v", got.EpisodeTitle)
	}
	if got.PosterPath == nil || *got.PosterPath != "/poster.jpg" {
		t.Errorf("posterPath = %v", got.PosterPath)
	}
	if got.Runtime == nil || *got.Runtime != 34 {
		t.Errorf("runtime = %v", got.Runtime)
	}
	if got.Monitored == nil || !*got.Monitored {
		t.Errorf("monitored = %v", got.Monitored)
	}
}

// A show with no stored slot stays date-only, and a movie never grows
// episode fields. Absent means unknown, everywhere in this payload.
func TestAcqCalendarOmitsWhatItDoesNotKnow(t *testing.T) {
	e := newAPIEnv(t)
	ctx := context.Background()

	if _, err := e.db.CreateMediaItem(ctx, domain.MediaItem{
		Kind: domain.KindSeries, Title: "Streaming Show", SortTitle: "streaming show",
		Year: 2026, Monitored: true, Network: "Netflix",
		Seasons: []domain.Season{{Number: 1, Monitored: true, Episodes: []domain.Episode{
			{SeasonNumber: 1, EpisodeNumber: 1, AirDate: "2026-01-11", Monitored: true},
		}}},
	}); err != nil {
		t.Fatal(err)
	}
	movie := e.addMovie(t)
	acqSetReleaseDate(t, e, movie, "2026-01-15")

	var entries []apigen.CalendarEntry
	e.get(t, "/api/v1/calendar?start=2026-01-01&end=2026-01-31").
		expect(t, http.StatusOK).into(t, &entries)
	if len(entries) != 2 {
		t.Fatalf("got %d entries: %+v", len(entries), entries)
	}
	byKind := map[string]apigen.CalendarEntry{}
	for _, en := range entries {
		byKind[en.Kind] = en
	}

	ep := byKind["episode"]
	if ep.AirDateUtc != nil {
		t.Errorf("airDateUtc invented for a show with no slot: %v", ep.AirDateUtc)
	}
	if ep.Network == nil || *ep.Network != "Netflix" {
		t.Errorf("a streaming show still has a channel worth naming: %v", ep.Network)
	}
	if ep.EpisodeTitle != nil {
		t.Errorf("episodeTitle present for an untitled episode: %v", ep.EpisodeTitle)
	}

	mv := byKind["movie"]
	if mv.AirDateUtc != nil || mv.Network != nil ||
		mv.SeasonNumber != nil || mv.EpisodeNumber != nil || mv.EpisodeTitle != nil {
		t.Errorf("a movie grew episode fields: %+v", mv)
	}
}
