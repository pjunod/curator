package matcher

import (
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/parser"
)

func identityFixture(now time.Time) ([]domain.MediaItem, domain.EpisodeWantable, domain.EpisodeWantable) {
	fresh := func(source string) []domain.IdentitySourceStatus {
		return []domain.IdentitySourceStatus{{Source: source, FetchedAt: now.Add(-time.Hour)}}
	}
	uk := domain.MediaItem{
		ID: 1, Kind: domain.KindSeries, Title: "Have I Got News for You", Year: 1990,
		Source: "tvmaze", IDs: domain.ExternalIDs{TVDB: 74281, IMDB: "tt0098820"},
		Countries:       []domain.CountryEvidence{{Code: "GB", Source: "tvmaze", Basis: "network"}},
		IdentitySources: fresh("tvmaze"),
	}
	us := domain.MediaItem{
		ID: 2, Kind: domain.KindSeries, Title: "Have I Got News for You", Year: 2024,
		Source: "tvmaze", IDs: domain.ExternalIDs{TVDB: 453187, IMDB: "tt33096993"},
		Aliases: []domain.TitleAlias{
			{Title: "Have I Got News for You US", Source: "tvmaze", SourceID: "78874", Scope: "work", Role: "alternate"},
			{Title: "Have I Got News For You U. S.", Source: "tvmaze", SourceID: "78874", Scope: "work", Role: "alternate"},
		},
		Countries:       []domain.CountryEvidence{{Code: "US", Source: "tmdb", Basis: "origin"}},
		IdentitySources: fresh("tvmaze"),
	}
	ukWant := domain.EpisodeWantable{Item: 1, Title: uk.Title, Year: uk.Year, Season: 68, Episode: 1,
		Identity: domain.MediaIdentity{Title: uk.Title, Year: uk.Year, IDs: uk.IDs, Aliases: uk.Aliases, Countries: uk.Countries, Sources: uk.IdentitySources}}
	usWant := domain.EpisodeWantable{Item: 2, Title: us.Title, Year: us.Year, Season: 5, Episode: 1,
		Identity: domain.MediaIdentity{Title: us.Title, Year: us.Year, IDs: us.IDs, Aliases: us.Aliases, Countries: us.Countries, Sources: us.IdentitySources}}
	return []domain.MediaItem{uk, us}, ukWant, usWant
}

func TestEvaluateRegionalScreenshotAndConvention(t *testing.T) {
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	items, uk, us := identityFixture(now)
	index := NewIdentityIndex(items, now)

	qualified := ReleaseEvidence{Parsed: parser.Parse("Have.I.Got.News.for.You.US.S05E01.1080p.WEB.h264-EDITH")}
	decision := Evaluate(qualified, us, index)
	if !decision.Matched || decision.Method != "alias" {
		t.Fatalf("qualified US decision = %+v", decision)
	}
	if wrong := Evaluate(qualified, uk, index); wrong.Matched {
		t.Fatalf("qualified US release matched UK: %+v", wrong)
	}

	unqualified := ReleaseEvidence{Parsed: parser.Parse("Have.I.Got.News.for.You.S68E01.1080p.WEB.h264-RAWR")}
	decision = Evaluate(unqualified, uk, index)
	if !decision.Matched || decision.Method != "convention" {
		t.Fatalf("unqualified UK decision = %+v", decision)
	}
	if wrong := Evaluate(unqualified, us, index); wrong.Matched {
		t.Fatalf("unqualified release matched US remake: %+v", wrong)
	}
}

func TestEvaluateConventionRequiresFreshCompleteSnapshots(t *testing.T) {
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	items, uk, _ := identityFixture(now)
	items[1].IdentitySources[0].FetchedAt = now.Add(-8 * 24 * time.Hour)
	decision := Evaluate(ReleaseEvidence{Parsed: parser.Parse("Have.I.Got.News.for.You.S68E01")}, uk, NewIdentityIndex(items, now))
	if decision.Matched || decision.Code != "ambiguous_identity" {
		t.Fatalf("stale convention decision = %+v", decision)
	}
}

func TestEvaluateIDCanRescueTitleButNotConflictOrCoverage(t *testing.T) {
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	items, _, us := identityFixture(now)
	index := NewIdentityIndex(items, now)

	rescued := Evaluate(ReleaseEvidence{
		Parsed: parser.Parse("Completely.Different.S05E01"),
		IDs:    domain.ExternalIDs{TVDB: 453187},
	}, us, index)
	if !rescued.Matched || rescued.Method != "id" {
		t.Fatalf("ID rescue = %+v", rescued)
	}

	wrongEpisode := Evaluate(ReleaseEvidence{
		Parsed: parser.Parse("Completely.Different.S05E02"),
		IDs:    domain.ExternalIDs{TVDB: 453187},
	}, us, index)
	if wrongEpisode.Matched || wrongEpisode.Code != "episode_mismatch" {
		t.Fatalf("ID wrong episode = %+v", wrongEpisode)
	}

	conflict := Evaluate(ReleaseEvidence{
		Parsed: parser.Parse("Have.I.Got.News.for.You.US.S05E01"),
		IDs:    domain.ExternalIDs{TVDB: 74281},
	}, us, index)
	if conflict.Matched || conflict.Code != "id_conflict" {
		t.Fatalf("conflicting returned ID = %+v", conflict)
	}
}

func TestEvaluateMatchingIDCannotOverrideExplicitWrongYear(t *testing.T) {
	now := time.Now()
	item := domain.MediaItem{ID: 9, Kind: domain.KindSeries, Title: "Example", Year: 2020, IDs: domain.ExternalIDs{TVDB: 99}}
	want := domain.EpisodeWantable{Item: 9, EpisodeID: 901, Title: item.Title, Year: item.Year, Season: 1, Episode: 1, Mon: true,
		Identity: domain.MediaIdentity{Title: item.Title, Year: item.Year, IDs: item.IDs}}
	decision := Evaluate(ReleaseEvidence{
		Parsed: parser.Parse("Example.2024.S01E01.1080p"), IDs: domain.ExternalIDs{TVDB: 99},
	}, want, NewIdentityIndex([]domain.MediaItem{item}, now))
	if decision.Matched || decision.Code != "year_conflict" {
		t.Fatalf("wrong-year ID match = %+v", decision)
	}
}

func TestEvaluateCountryConflictSurvivesMatchingID(t *testing.T) {
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	item := domain.MediaItem{ID: 9, Kind: domain.KindSeries, Title: "Show", Year: 2020,
		IDs: domain.ExternalIDs{TVDB: 9}, Countries: []domain.CountryEvidence{{Code: "GB", Source: "manual", Basis: "manual"}}}
	want := domain.EpisodeWantable{Item: 9, Title: "Show", Season: 1, Episode: 1,
		Identity: domain.MediaIdentity{Title: "Show", Year: 2020, IDs: item.IDs, Countries: item.Countries}}
	decision := Evaluate(ReleaseEvidence{Parsed: parser.Parse("Show.US.S01E01"), IDs: domain.ExternalIDs{TVDB: 9}}, want, NewIdentityIndex([]domain.MediaItem{item}, now))
	if decision.Matched || decision.Code != "country_conflict" {
		t.Fatalf("country conflict = %+v", decision)
	}
}

func TestEvaluateRanksCanonicalAboveAliasesAndRejectsEmptyKeys(t *testing.T) {
	now := time.Now()
	items := []domain.MediaItem{
		{ID: 1, Kind: domain.KindMovie, Title: "Heat", Year: 1995},
		{ID: 2, Kind: domain.KindMovie, Title: "Other", Year: 1995,
			Aliases: []domain.TitleAlias{{Title: "Heat", Source: "manual", Scope: "work", Role: "manual"}}},
		{ID: 3, Kind: domain.KindMovie, Title: "日本語", Year: 2020},
		{ID: 4, Kind: domain.KindMovie, Title: "中文", Year: 2020},
	}
	heat := domain.MovieWantable{Item: 1, Title: "Heat", Year: 1995, Identity: domain.MediaIdentity{Title: "Heat", Year: 1995}}
	decision := Evaluate(ReleaseEvidence{Parsed: parser.Parse("Heat.1995.1080p")}, heat, NewIdentityIndex(items, now))
	if !decision.Matched || decision.Method != "title" {
		t.Fatalf("canonical ranking = %+v", decision)
	}
	cjk := domain.MovieWantable{Item: 3, Title: "日本語", Year: 2020, Identity: domain.MediaIdentity{Title: "日本語", Year: 2020}}
	decision = Evaluate(ReleaseEvidence{Parsed: parser.Parse("中文.2020.1080p")}, cjk, NewIdentityIndex(items, now))
	if decision.Matched {
		t.Fatalf("empty normalized keys matched: %+v", decision)
	}
}

func TestEvaluateScopeAndDailyCoverage(t *testing.T) {
	now := time.Now()
	item := domain.MediaItem{ID: 1, Kind: domain.KindSeries, Title: "Cunk Universe",
		Aliases: []domain.TitleAlias{{Title: "Cunk on Earth", Source: "tmdb", SourceID: "79063", Scope: "unsupported_numbering", Role: "alternate"}}}
	want := domain.EpisodeWantable{Item: 1, Title: item.Title, Season: 1, Episode: 1,
		Identity: domain.MediaIdentity{Title: item.Title, Aliases: item.Aliases}}
	decision := Evaluate(ReleaseEvidence{Parsed: parser.Parse("Cunk.on.Earth.S01E01")}, want, NewIdentityIndex([]domain.MediaItem{item}, now))
	if decision.Code != "numbering_scope_conflict" {
		t.Fatalf("scope decision = %+v", decision)
	}

	dailyItem := domain.MediaItem{ID: 2, Kind: domain.KindSeries, Title: "Daily Show", IDs: domain.ExternalIDs{TVDB: 2}}
	dailyWant := domain.EpisodeWantable{Item: 2, Title: dailyItem.Title, Season: 1, Episode: 1,
		Identity: domain.MediaIdentity{Title: dailyItem.Title, IDs: dailyItem.IDs}}
	decision = Evaluate(ReleaseEvidence{Parsed: parser.Parse("Daily.Show.2026.09.17.1080p"), IDs: domain.ExternalIDs{TVDB: 2}}, dailyWant, NewIdentityIndex([]domain.MediaItem{dailyItem}, now))
	if decision.Code != "coverage_unsupported" {
		t.Fatalf("daily decision = %+v", decision)
	}
}
