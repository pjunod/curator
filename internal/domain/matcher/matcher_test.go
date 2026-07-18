package matcher

import (
	"testing"

	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/domain/decision"
	"github.com/monarr-media/monarr/internal/domain/naming"
	"github.com/monarr-media/monarr/internal/domain/parser"
	"github.com/monarr-media/monarr/internal/domain/quality"
)

func q(s quality.Source, r int) *quality.Quality {
	qq := quality.Quality{Source: s, Resolution: r}
	return &qq
}

func TestNormalizeTitle(t *testing.T) {
	cases := [][2]string{
		{"The Office (US)", "office us"},
		{"the.office.us", "office us"},
		{"Marvel's Agents of S.H.I.E.L.D.", "marvel s agents of s h i e l d"},
		{"A Quiet Place", "quiet place"},
		{"The", "the"}, // lone article stays
	}
	for _, c := range cases {
		if got := NormalizeTitle(c[0]); got != c[1] {
			t.Errorf("NormalizeTitle(%q) = %q, want %q", c[0], got, c[1])
		}
	}
}

func TestMovieMatching(t *testing.T) {
	movie := domain.MovieWantable{Item: 1, Title: "The Matrix", Year: 1999, Mon: true}
	p := parser.Parse("The.Matrix.1999.1080p.BluRay.x264-GRP")
	if len(Match(p, []domain.Wantable{movie})) != 1 {
		t.Error("exact movie should match")
	}
	// Year tolerance ±1.
	if len(Match(parser.Parse("The.Matrix.2000.1080p.BluRay"), []domain.Wantable{movie})) != 1 {
		t.Error("±1 year should match")
	}
	if len(Match(parser.Parse("The.Matrix.2003.1080p.BluRay"), []domain.Wantable{movie})) != 0 {
		t.Error("distant year must not match (that's Reloaded)")
	}
	if len(Match(parser.Parse("The.Matrix.Reloaded.2003.1080p"), []domain.Wantable{movie})) != 0 {
		t.Error("different title must not match")
	}
	// An episode release never matches a movie.
	if len(Match(parser.Parse("The.Matrix.S01E01.1080p"), []domain.Wantable{movie})) != 0 {
		t.Error("episode release must not match a movie")
	}
}

func TestEpisodeAndSeasonPackMatching(t *testing.T) {
	ep1 := domain.EpisodeWantable{Item: 7, EpisodeID: 71, Title: "Test Show", Season: 2, Episode: 1, Mon: true}
	ep2 := domain.EpisodeWantable{Item: 7, EpisodeID: 72, Title: "Test Show", Season: 2, Episode: 2, Mon: true}
	season := domain.SeasonWantable{Item: 7, Title: "Test Show", Season: 2, Mon: true,
		Episodes: []domain.EpisodeWantable{ep1, ep2}}
	cands := []domain.Wantable{ep1, ep2, season}

	// Single episode matches exactly one wantable.
	m := Match(parser.Parse("Test.Show.S02E01.720p.WEB-DL"), cands)
	if len(m) != 1 || m[0].Wantable.ID() != ep1.ID() {
		t.Fatalf("single ep match = %+v", m)
	}
	// Multi-episode covers both episodes.
	m = Match(parser.Parse("Test.Show.S02E01E02.1080p"), cands)
	if len(m) != 2 {
		t.Fatalf("multi-ep should cover both eps, got %+v", m)
	}
	// A season pack matches the episodes AND the season wantable (fan-out).
	m = Match(parser.Parse("Test.Show.S02.1080p.BluRay"), cands)
	var full int
	for _, mm := range m {
		if mm.FullSeason {
			full++
		}
	}
	if len(m) != 3 || full != 1 {
		t.Fatalf("season pack coverage = %+v", m)
	}
	// Wrong season: nothing.
	if len(Match(parser.Parse("Test.Show.S03E01.720p"), cands)) != 0 {
		t.Error("wrong season must not match")
	}
}

func TestDecisionEngine(t *testing.T) {
	profile := quality.DefaultProfiles()[1] // HD-1080p, cutoff WEBDL-1080
	missing := domain.MovieWantable{Item: 1, Title: "M", Mon: true}
	haveHDTV := domain.MovieWantable{Item: 1, Title: "M", Mon: true, Have: q(quality.SourceHDTV, 1080)}
	haveCutoff := domain.MovieWantable{Item: 1, Title: "M", Mon: true, Have: q(quality.SourceBluray, 1080)}
	unmon := domain.MovieWantable{Item: 1, Title: "M", Mon: false}

	web1080 := quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080}
	uhd := quality.Quality{Source: quality.SourceWEBDL, Resolution: 2160}

	if d := decision.Decide(web1080, missing, profile); !d.Accepted || d.IsUpgrade {
		t.Errorf("missing + allowed = accept: %+v", d)
	}
	if d := decision.Decide(uhd, missing, profile); d.Accepted || d.Rejections[0].Code != decision.CodeQualityNotAllow {
		t.Errorf("2160 on HD profile = quality_not_allowed: %+v", d)
	}
	if d := decision.Decide(web1080, haveHDTV, profile); !d.Accepted || !d.IsUpgrade {
		t.Errorf("webdl over hdtv = upgrade: %+v", d)
	}
	if d := decision.Decide(web1080, haveCutoff, profile); d.Accepted || d.Rejections[0].Code != decision.CodeAtCutoff {
		t.Errorf("already at cutoff: %+v", d)
	}
	hdtv := quality.Quality{Source: quality.SourceHDTV, Resolution: 1080}
	if d := decision.Decide(hdtv, haveHDTV, profile); d.Accepted || d.Rejections[0].Code != decision.CodeNotAnUpgrade {
		t.Errorf("sidegrade rejected: %+v", d)
	}
	if d := decision.Decide(web1080, unmon, profile); d.Accepted || d.Rejections[0].Code != decision.CodeUnmonitored {
		t.Errorf("unmonitored rejected: %+v", d)
	}
	if d := decision.Decide(quality.Quality{}, missing, profile); d.Accepted || d.Rejections[0].Code != decision.CodeQualityUnknown {
		t.Errorf("unknown quality rejected: %+v", d)
	}
	// Season pack upgrade uses the WORST episode quality.
	epMissing := domain.EpisodeWantable{Item: 7, Season: 1, Episode: 2, Mon: true}
	epHave := domain.EpisodeWantable{Item: 7, Season: 1, Episode: 1, Mon: true, Have: q(quality.SourceWEBDL, 1080)}
	pack := domain.SeasonWantable{Item: 7, Season: 1, Mon: true, Episodes: []domain.EpisodeWantable{epHave, epMissing}}
	if d := decision.Decide(web1080, pack, profile); !d.Accepted {
		t.Errorf("pack with a missing episode = accept: %+v", d)
	}
}

func TestPlannerAndRenamer(t *testing.T) {
	movie := domain.MovieWantable{Item: 1, Title: "The Matrix", Year: 1999}
	if qs := domain.PlanSearch(movie); len(qs) != 1 || qs[0].Q != "The Matrix 1999" {
		t.Errorf("movie plan = %+v", qs)
	}
	ep := domain.EpisodeWantable{Item: 7, Title: "Test Show", Season: 2, Episode: 5}
	if qs := domain.PlanSearch(ep); len(qs) != 1 || qs[0].Q != "Test Show S02E05" || qs[0].Season != 2 {
		t.Errorf("episode plan = %+v", qs)
	}
	season := domain.SeasonWantable{Item: 7, Title: "Test Show", Season: 2}
	if qs := domain.PlanSearch(season); len(qs) != 1 || qs[0].Q != "Test Show S02" {
		t.Errorf("season plan = %+v", qs)
	}

	got := naming.Render(naming.EpisodeFileTemplate, map[string]string{
		"Series Title": "Test Show", "season": "2", "episode": "5",
		"Episode Title": "Pilot", "Quality Full": "WEB-DL 1080p",
	})
	if got != "Test Show - S02E05 - Pilot [WEB-DL 1080p]" {
		t.Errorf("episode render = %q", got)
	}
	got = naming.Render(naming.MovieFileTemplate, map[string]string{
		"Movie Title": "The Matrix", "Release Year": "1999", "Quality Full": "Bluray 1080p",
	})
	if got != "The Matrix (1999) [Bluray 1080p]" {
		t.Errorf("movie render = %q", got)
	}
	// Empty tokens collapse cleanly.
	got = naming.Render(naming.EpisodeFileTemplate, map[string]string{
		"Series Title": "Show", "season": "1", "episode": "1", "Quality Full": "HDTV 720p",
	})
	if got != "Show - S01E01 - [HDTV 720p]" {
		t.Errorf("empty-token render = %q", got)
	}
}

func TestBookMatching(t *testing.T) {
	book := domain.BookWantable{
		Item: 9, Profile: 4, Mon: true,
		Title: "Project Hail Mary", Author: "Andy Weir", Year: 2021,
	}
	cands := []domain.Wantable{book}

	for _, release := range []string{
		"Andy Weir - Project Hail Mary (2021) EPUB",      // author-first dash
		"Project Hail Mary - Andy Weir 2021 EPUB",        // swapped dash order
		"Project Hail Mary by Andy Weir [AZW3]",          // "by" form
		"Andy Weir - Project Hail Mary (Unabridged) M4B", // audiobook noise
	} {
		if got := Match(parser.Parse(release), cands); len(got) != 1 {
			t.Errorf("%q: matches = %d, want 1", release, len(got))
		}
	}

	for _, release := range []string{
		"Andy Weir - Artemis (2017) EPUB",          // wrong book, right author
		"John Scalzi - Project Hail Mary EPUB",     // right title, wrong author
		"Project.Hail.Mary.2024.1080p.WEB-DL.x264", // the MOVIE, not the book
	} {
		if got := Match(parser.Parse(release), cands); len(got) != 0 {
			t.Errorf("%q: matched but should not", release)
		}
	}

	// Title-only wantable (no author known) still matches on title.
	loose := domain.BookWantable{Item: 10, Title: "Dune"}
	if got := Match(parser.Parse("Frank Herbert - Dune M4B"), []domain.Wantable{loose}); len(got) != 1 {
		t.Errorf("title-only book match failed")
	}
}

func TestBookPlannerAndNaming(t *testing.T) {
	book := domain.BookWantable{Item: 9, Title: "Project Hail Mary", Author: "Andy Weir"}
	qs := domain.PlanSearch(book)
	if len(qs) != 2 || qs[0].Q != "Andy Weir Project Hail Mary" || qs[0].Kind != domain.KindBook {
		t.Errorf("book plan = %+v", qs)
	}
	if qs[1].Q != "Project Hail Mary" {
		t.Errorf("book fallback query = %+v", qs[1])
	}

	if got := naming.BookFolder("Andy Weir", "Project Hail Mary"); got != "Andy Weir/Project Hail Mary" {
		t.Errorf("BookFolder = %q", got)
	}
	if got := naming.BookFileName("Andy Weir", "Project Hail Mary"); got != "Project Hail Mary - Andy Weir" {
		t.Errorf("BookFileName = %q", got)
	}
	if got := naming.BookFileName("", "Standalone"); got != "Standalone" {
		t.Errorf("authorless BookFileName = %q", got)
	}
}

func TestAnimeAbsoluteMatching(t *testing.T) {
	ep15 := domain.EpisodeWantable{
		Item: 3, Title: "Frieren Beyond Journeys End", Season: 2, Episode: 3, Absolute: 15,
	}
	p := parser.Parse("[SubsPlease] Frieren Beyond Journeys End - 15 (1080p) [ABCD1234]")
	if len(p.Absolute) != 1 || p.Absolute[0] != 15 {
		t.Fatalf("parsed absolute = %v", p.Absolute)
	}
	if got := Match(p, []domain.Wantable{ep15}); len(got) != 1 {
		t.Errorf("absolute match = %d, want 1", len(got))
	}
	// Wrong absolute number: no match.
	other := domain.EpisodeWantable{Item: 3, Title: "Frieren Beyond Journeys End", Season: 2, Episode: 4, Absolute: 16}
	if got := Match(p, []domain.Wantable{other}); len(got) != 0 {
		t.Errorf("wrong absolute matched")
	}
	// Multi-episode absolute run covers both.
	p2 := parser.Parse("[Judas] Frieren Beyond Journeys End - 15-16 [1080p]")
	if got := Match(p2, []domain.Wantable{ep15, other}); len(got) != 2 {
		t.Errorf("run matched %d, want 2 (parsed %v)", len(got), p2.Absolute)
	}
}
