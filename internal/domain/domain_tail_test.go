package domain_test

import (
	"testing"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/mediainfo"
	"github.com/pjunod/monarr/internal/domain/quality"
)

func q(src quality.Source, res int) quality.Quality {
	return quality.Quality{Source: src, Resolution: res}
}

func qp(src quality.Source, res int) *quality.Quality {
	v := q(src, res)
	return &v
}

// Sort titles are what an alphabetical library list orders on, so the
// leading article has to come off — otherwise a third of the library files
// under T and the list is useless.
func TestTailSortTitleDropsLeadingArticles(t *testing.T) {
	cases := map[string]string{
		"The Matrix":    "matrix",
		"A Quiet Place": "quiet place",
		"An Education":  "education",
		"  Heat  ":      "heat",
		"Thelma":        "thelma", // "The" is only an article with a space after it
		"Andor":         "andor",
		// A title that IS the article is left alone: stripping it would sort
		// the item under the empty string.
		"The": "the",
		"":    "",
	}
	for in, want := range cases {
		if got := domain.SortTitle(in); got != want {
			t.Errorf("SortTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

// The schema enforces the same three kinds with a CHECK constraint, so this
// guards the Go half of a pair that must never drift.
func TestTailValidKindMatchesTheSchemasThreeValues(t *testing.T) {
	for _, k := range []domain.MediaKind{domain.KindMovie, domain.KindSeries, domain.KindBook} {
		if !domain.ValidKind(k) {
			t.Errorf("%q is not valid but the schema accepts it", k)
		}
	}
	for _, k := range []domain.MediaKind{"", "mixed", "music"} {
		if domain.ValidKind(k) {
			t.Errorf("%q is accepted but the schema's CHECK would reject it", k)
		}
	}
}

// A mixed root is what every pre-ADR-0009 root migrated to, and it has to
// behave exactly as roots behaved before kinds existed: accept everything.
func TestTailRootKindRestrictsOnlyWhenItIsTyped(t *testing.T) {
	movies := domain.RootKindOf(domain.KindMovie)
	if got, ok := movies.Media(); !ok || got != domain.KindMovie {
		t.Errorf("Media() = %q,%v for a movie root", got, ok)
	}
	if !movies.Accepts(domain.KindMovie) || movies.Accepts(domain.KindSeries) {
		t.Error("a movie root must accept movies and nothing else")
	}

	if _, ok := domain.KindMixed.Media(); ok {
		t.Error("a mixed root reported a restriction; it restricts nothing")
	}
	for _, k := range []domain.MediaKind{domain.KindMovie, domain.KindSeries, domain.KindBook} {
		if !domain.KindMixed.Accepts(k) {
			t.Errorf("a mixed root rejected %q — that is the pre-0009 behaviour it must preserve", k)
		}
	}

	// A root kind nobody recognises restricts nothing either: refusing every
	// item would strand a library whose row was written by a newer build.
	junk := domain.RootKind("music")
	if domain.ValidRootKind(junk) {
		t.Error("an unknown root kind validated")
	}
	if _, ok := junk.Media(); ok {
		t.Error("an unknown root kind reported a restriction")
	}
	if !junk.Accepts(domain.KindMovie) {
		t.Error("an unknown root kind refused an item rather than falling back to mixed")
	}
	if !domain.ValidRootKind(domain.KindMixed) {
		t.Error("mixed is not a valid root kind")
	}
}

// Manual records have no provider behind them, and the paths that must know
// are the ones that would otherwise go looking for one: metadata refresh and
// anything reporting an external id.
func TestTailIsManualIdentifiesProviderlessRecords(t *testing.T) {
	if !(domain.MediaItem{Source: domain.SourceManual}).IsManual() {
		t.Error("a manual record did not report itself as manual")
	}
	if (domain.MediaItem{Source: "tmdb"}).IsManual() {
		t.Error("a TMDB-backed record reported itself as manual")
	}
	// The zero value predates the Source column; it is not manual, because
	// treating it as such would stop refreshing every legacy row.
	if (domain.MediaItem{}).IsManual() {
		t.Error("a record with no recorded source reported itself as manual")
	}
}

// The don't-churn rule in one predicate: a guess about the source axis must
// never evict a file that is already the right size.
func TestTailMediaFileSourceVerifiedFollowsProvenance(t *testing.T) {
	cases := []struct {
		prov mediainfo.Provenance
		conf mediainfo.Confidence
		want bool
	}{
		{mediainfo.ProvenanceManual, mediainfo.ConfidenceNone, true},
		{mediainfo.ProvenanceFilename, mediainfo.ConfidenceNone, true},
		{mediainfo.ProvenanceProbe, mediainfo.ConfidenceHigh, true},
		{mediainfo.ProvenanceProbe, mediainfo.ConfidenceMedium, false},
		{mediainfo.ProvenanceFailed, mediainfo.ConfidenceNone, false},
		{mediainfo.ProvenanceImplausible, mediainfo.ConfidenceHigh, false},
	}
	for _, c := range cases {
		f := domain.MediaFile{Provenance: c.prov, Confidence: c.conf}
		if got := f.SourceVerified(); got != c.want {
			t.Errorf("SourceVerified(%q/%q) = %v, want %v", c.prov, c.conf, got, c.want)
		}
	}
}

// Wantable ids are the identity in-flight suppression and grab bookkeeping
// key off. A copy's want must be a DIFFERENT identity from the primary's, or
// hunting a 720p second copy would look like the 1080p grab already running.
func TestTailWantableIDsSeparateCopiesFromThePrimary(t *testing.T) {
	cases := []struct {
		w    domain.Wantable
		want domain.WantableID
	}{
		{domain.MovieWantable{Item: 42}, "movie:42"},
		{domain.MovieWantable{Item: 42, Copy: 3}, "movie:42:c3"},
		{domain.EpisodeWantable{Item: 42, Season: 2, Episode: 5}, "episode:42:2:5"},
		{domain.EpisodeWantable{Item: 42, Season: 2, Episode: 5, Copy: 3}, "episode:42:2:5:c3"},
		{domain.SeasonWantable{Item: 42, Season: 2}, "season:42:2"},
		{domain.SeasonWantable{Item: 42, Season: 2, Copy: 3}, "season:42:2:c3"},
		{domain.BookWantable{Item: 42}, "book:42"},
	}
	for _, c := range cases {
		if got := c.w.ID(); got != c.want {
			t.Errorf("ID() = %q, want %q", got, c.want)
		}
	}
}

// The rest of the Wantable contract, which the whole acquisition pipeline is
// written against and never against movies or episodes directly.
func TestTailWantableContractIsUniformAcrossKinds(t *testing.T) {
	have := qp(quality.SourceWEBDL, 1080)
	cases := []struct {
		name string
		w    domain.Wantable
	}{
		{"movie", domain.MovieWantable{Item: 1, Profile: 7, Mon: true, Have: have, Verified: true}},
		{"episode", domain.EpisodeWantable{Item: 1, Profile: 7, Mon: true, Have: have, Verified: true}},
		{"book", domain.BookWantable{Item: 1, Profile: 7, Mon: true, Have: have, Verified: true}},
		{"season", domain.SeasonWantable{Item: 1, Profile: 7, Mon: true, Episodes: []domain.EpisodeWantable{
			{Have: have, Files: true, Verified: true},
		}}},
	}
	for _, c := range cases {
		if c.w.MediaItemID() != 1 || c.w.ProfileID() != 7 || !c.w.Monitored() {
			t.Errorf("%s: identity/profile/monitored = %d/%d/%v",
				c.name, c.w.MediaItemID(), c.w.ProfileID(), c.w.Monitored())
		}
		got, ok := c.w.CurrentQuality()
		if !ok || got != *have {
			t.Errorf("%s: CurrentQuality = %v,%v", c.name, got, ok)
		}
		if !c.w.OnDisk() || !c.w.SourceVerified() {
			t.Errorf("%s: OnDisk=%v SourceVerified=%v", c.name, c.w.OnDisk(), c.w.SourceVerified())
		}
	}
}

// "On disk but unmeasurable" and "not on disk" are different states, and
// treating them alike is precisely what made monarr hunt files it already
// had (ADR 0013 §5).
func TestTailOnDiskIsNotTheSameQuestionAsCurrentQuality(t *testing.T) {
	unmeasured := domain.MovieWantable{Item: 1, Files: true} // Have == nil
	if _, ok := unmeasured.CurrentQuality(); ok {
		t.Error("a file whose quality could not be read reported a quality")
	}
	if !unmeasured.OnDisk() {
		t.Error("a file whose quality could not be read is still a file on disk")
	}

	missing := domain.MovieWantable{Item: 1}
	if missing.OnDisk() {
		t.Error("a wantable with no files and no quality reported itself on disk")
	}
	// Same distinction for the other two file-backed kinds.
	if !(domain.EpisodeWantable{Files: true}).OnDisk() || (domain.EpisodeWantable{}).OnDisk() {
		t.Error("episode OnDisk does not follow its file set")
	}
	if !(domain.BookWantable{Files: true}).OnDisk() || (domain.BookWantable{}).OnDisk() {
		t.Error("book OnDisk does not follow its file set")
	}
}

// A season is a weakest-link aggregate: a pack only upgrades it if it beats
// the worst episode, so nine 1080p episodes and one 720p is a 720p season.
func TestTailSeasonWantableGradesOnItsWeakestEpisode(t *testing.T) {
	season := domain.SeasonWantable{Item: 1, Season: 2, Episodes: []domain.EpisodeWantable{
		{Have: qp(quality.SourceBluray, 1080), Files: true, Verified: true},
		{Have: qp(quality.SourceHDTV, 720), Files: true, Verified: true},
		{Have: qp(quality.SourceWEBDL, 1080), Files: true, Verified: true},
	}}
	got, ok := season.CurrentQuality()
	if !ok {
		t.Fatal("a fully-populated season reported no quality")
	}
	if got.Resolution != 720 {
		t.Errorf("season quality = %+v, want the 720p weakest link", got)
	}
	if !season.OnDisk() || !season.SourceVerified() {
		t.Errorf("OnDisk=%v SourceVerified=%v for a complete verified season",
			season.OnDisk(), season.SourceVerified())
	}

	// One missing episode makes the whole season an incomplete thing to
	// hunt, whatever the others are.
	gap := season
	gap.Episodes = append([]domain.EpisodeWantable{{}}, season.Episodes...)
	if _, ok := gap.CurrentQuality(); ok {
		t.Error("a season with a missing episode reported a quality")
	}
	if gap.OnDisk() {
		t.Error("a season with a missing episode reported itself on disk")
	}

	// One unverified episode drags the season's verification down with it.
	unsure := domain.SeasonWantable{Episodes: []domain.EpisodeWantable{
		{Have: qp(quality.SourceBluray, 1080), Files: true, Verified: true},
		{Have: qp(quality.SourceBluray, 1080), Files: true, Verified: false},
	}}
	if unsure.SourceVerified() {
		t.Error("a season claimed verification its weakest episode does not have")
	}

	// An empty season is not "on disk and perfect", it is nothing at all.
	empty := domain.SeasonWantable{Item: 1, Season: 3}
	if _, ok := empty.CurrentQuality(); ok {
		t.Error("an episode-less season reported a quality")
	}
	if empty.OnDisk() || empty.SourceVerified() {
		t.Error("an episode-less season reported itself complete")
	}
}

// Copy identity has to be readable off any wantable without the caller
// switching on its type — that is what keeps copy support out of the rest of
// the pipeline.
func TestTailWantableCopyAccessorsCoverEveryKind(t *testing.T) {
	cases := []struct {
		w    domain.Wantable
		id   int64
		name string
	}{
		{domain.MovieWantable{Copy: 3, CopyName: "720p for dad"}, 3, "720p for dad"},
		{domain.EpisodeWantable{Copy: 4, CopyName: "archive"}, 4, "archive"},
		{domain.SeasonWantable{Copy: 5, CopyName: "archive"}, 5, "archive"},
		{domain.MovieWantable{}, 0, ""},
		// A book has no copies at all; asking must give the primary rather
		// than panicking on a type the switch does not list.
		{domain.BookWantable{Item: 9}, 0, ""},
	}
	for _, c := range cases {
		if got := domain.WantableCopy(c.w); got != c.id {
			t.Errorf("WantableCopy(%T) = %d, want %d", c.w, got, c.id)
		}
		if got := domain.WantableCopyName(c.w); got != c.name {
			t.Errorf("WantableCopyName(%T) = %q, want %q", c.w, got, c.name)
		}
	}
}

// PlanSearch is one of exactly two places media-kind knowledge is allowed to
// live, so the shape of every query it emits is worth pinning.
func TestTailPlanSearchEmitsTheQueriesEachKindNeeds(t *testing.T) {
	movie := domain.PlanSearch(domain.MovieWantable{Title: "Heat", Year: 1995})
	if len(movie) != 1 || movie[0].Q != "Heat 1995" || movie[0].Kind != domain.KindMovie {
		t.Errorf("movie queries = %+v", movie)
	}
	// A movie with no year must not search for "Heat 0".
	noYear := domain.PlanSearch(domain.MovieWantable{Title: "Heat"})
	if len(noYear) != 1 || noYear[0].Q != "Heat" {
		t.Errorf("year-less movie queries = %+v", noYear)
	}

	ep := domain.PlanSearch(domain.EpisodeWantable{Title: "The Show", Season: 2, Episode: 5})
	if len(ep) != 1 || ep[0].Q != "The Show S02E05" ||
		ep[0].Season != 2 || ep[0].Episode != 5 || ep[0].Kind != domain.KindSeries {
		t.Errorf("episode queries = %+v", ep)
	}

	// A season query carries the season but NOT an episode: episode 0 is
	// what tells the indexer this is a pack search.
	season := domain.PlanSearch(domain.SeasonWantable{Title: "The Show", Season: 2})
	if len(season) != 1 || season[0].Q != "The Show S02" ||
		season[0].Season != 2 || season[0].Episode != 0 {
		t.Errorf("season queries = %+v", season)
	}

	// Author+title discriminates; title alone casts wider, so both go out.
	book := domain.PlanSearch(domain.BookWantable{Title: "Dune", Author: "Frank Herbert"})
	if len(book) != 2 || book[0].Q != "Frank Herbert Dune" || book[1].Q != "Dune" {
		t.Errorf("book queries = %+v", book)
	}
	for _, sq := range book {
		if sq.Kind != domain.KindBook {
			t.Errorf("book query %+v is not tagged as a book — Torznab category "+
				"selection reads this", sq)
		}
	}
	anon := domain.PlanSearch(domain.BookWantable{Title: "Beowulf"})
	if len(anon) != 1 || anon[0].Q != "Beowulf" {
		t.Errorf("author-less book queries = %+v", anon)
	}

	// An unknown implementation of the interface gets no queries rather than
	// a panic or a nonsense search.
	if got := domain.PlanSearch(stubWantable{}); got != nil {
		t.Errorf("an unrecognised wantable produced queries: %+v", got)
	}
}

// stubWantable is a Wantable the planner has never heard of — the shape a
// future kind takes before PlanSearch learns about it.
type stubWantable struct{}

func (stubWantable) ID() domain.WantableID { return "stub:1" }
func (stubWantable) MediaItemID() int64    { return 1 }
func (stubWantable) ProfileID() int64      { return 1 }
func (stubWantable) Monitored() bool       { return true }
func (stubWantable) CurrentQuality() (quality.Quality, bool) {
	return quality.Quality{}, false
}
func (stubWantable) OnDisk() bool         { return false }
func (stubWantable) SourceVerified() bool { return false }
