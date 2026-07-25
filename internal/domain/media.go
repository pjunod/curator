package domain

import (
	"strings"
	"time"

	"github.com/monarr-media/monarr/internal/domain/quality"
)

// SourceManual marks a library record no provider backs: its metadata was
// typed by the user and its episodes were read off the disk (ADR 0012).
const SourceManual = "manual"

// IsManual reports whether this record has no provider behind it. The
// paths that must know are the ones that would otherwise go looking for a
// provider: metadata refresh, and anything reporting an external id.
func (m MediaItem) IsManual() bool { return m.Source == SourceManual }

// UpgradeState answers, for a list view, "is this done or is it still being
// hunted" — the question a poster grid otherwise leaves to guesswork.
type UpgradeState string

// The four states an item can be in against its quality profile.
const (
	// UpgradeUnknown is the zero value: not computed for this view.
	UpgradeUnknown UpgradeState = ""
	// UpgradeMissing means nothing is on disk yet.
	UpgradeMissing UpgradeState = "missing"
	// UpgradeSeeking means what is on disk is below the profile's cutoff and
	// the profile allows upgrades, so monarr is still looking for better.
	UpgradeSeeking UpgradeState = "seeking"
	// UpgradeMet means the cutoff is reached: nothing further is wanted.
	UpgradeMet UpgradeState = "met"
	// UpgradeCapped means it is below the cutoff and staying there, because
	// the profile has upgrades switched off.
	UpgradeCapped UpgradeState = "capped"
)

// SortTitle normalizes a title for ordering: lowercase, trimmed, leading
// English articles dropped ("The Matrix" sorts under m).
func SortTitle(title string) string {
	t := strings.ToLower(strings.TrimSpace(title))
	for _, article := range []string{"the ", "a ", "an "} {
		if strings.HasPrefix(t, article) && len(t) > len(article) {
			return t[len(article):]
		}
	}
	return t
}

// MediaKind discriminates the aggregate root. Exactly three values exist
// (ADR 0002, ADR 0006); the schema enforces the same set with a CHECK
// constraint so the two can never drift silently.
type MediaKind string

// The three media kinds. Book is reserved from the first migration and
// implemented in Phase 2.5.
const (
	KindMovie  MediaKind = "movie"
	KindSeries MediaKind = "series"
	KindBook   MediaKind = "book"
)

// ValidKind reports whether k is one of the three known kinds.
func ValidKind(k MediaKind) bool {
	switch k {
	case KindMovie, KindSeries, KindBook:
		return true
	}
	return false
}

// ExternalIDs are provider identities for a media item. Zero values mean
// "not known". ISBN/OLID/ASIN serve the book kind (ADR 0006).
type ExternalIDs struct {
	TMDB   int64
	IMDB   string
	TVDB   int64
	ISBN13 string
	OLID   string
	ASIN   string
}

// Rating is one labeled community rating. Value is in the source's native
// scale (Scale = 10, 5, or 100) so "94%" and "4.3/5" render honestly.
type Rating struct {
	Source string  `json:"source"` // tmdb | openlibrary | imdb | rt | metacritic
	Value  float64 `json:"value"`
	Votes  int     `json:"votes,omitempty"`
	Scale  int     `json:"scale"`
}

// MediaItem is the aggregate root: one library entry (blueprint §4.2).
type MediaItem struct {
	ID        int64
	Kind      MediaKind
	Title     string
	SortTitle string
	Year      int
	Author    string // books only (ADR 0006); "" for movies/series
	IDs       ExternalIDs

	// Metadata cache (hydrated from the provider).
	Overview     string
	PosterPath   string // provider-relative path, e.g. "/abc.jpg"
	BackdropPath string
	Genres       []string
	Status       string // e.g. "Released", "Returning Series", "Ended"
	ReleaseDate  string // ISO date; first air date for series
	Runtime      int    // minutes; per-episode average for series
	Rating       float64
	RatingVotes  int // 0 = no rating known; Rating is provider-scale (TMDB /10, Open Library /5)
	Ratings      []Rating

	// Source names the provider this record came from ("tmdb", "tvmaze",
	// "openlibrary") or SourceManual when none did (ADR 0012).
	Source string

	Monitored        bool
	QualityProfileID int64
	RootFolderID     int64  // 0 = none assigned
	Path             string // absolute on-disk folder; "" if unassigned

	// Series-only.
	Ended   bool
	Seasons []Season

	Files  []MediaFile
	Copies []MediaCopy // additional quality targets (movies/series)

	// Completeness, derived for list views (hydrated by ListMediaItems, zero
	// elsewhere): monitored aired episodes vs those with files, and the raw
	// file count for movies/books.
	EpisodeCount     int
	EpisodeFileCount int
	FileCount        int
	// Quality is the WEAKEST quality among the primary copy's files, and
	// Upgrade is what the item's profile makes of it. The weakest rather
	// than the best because that is the one deciding whether the item is
	// still being hunted: a series with nine 1080p episodes and one 720p is
	// not finished at 1080p. Zero value = nothing on disk (or files whose
	// quality was never recorded).
	Quality quality.Quality
	Upgrade UpgradeState
	// QualityTarget is the profile's cutoff — the "good enough" point that
	// Quality is being compared against.
	QualityTarget quality.Quality

	AddedAt   time.Time
	UpdatedAt time.Time
}

// MediaCopy is an ADDITIONAL quality target for one item: the same
// movie/series kept at a second (third, …) quality, each copy with its own
// profile and its own automation lifecycle. Path "" means the copy shares
// the item's folder (filenames carry [Quality], so versions coexist);
// otherwise the copy lives in its own folder under its own root.
type MediaCopy struct {
	ID               int64
	MediaItemID      int64
	Name             string // display label, e.g. "720p for dad"; "" = profile name
	QualityProfileID int64
	RootFolderID     int64  // 0 = same folder as the item
	Path             string // "" = item folder
	Monitored        bool
	AddedAt          time.Time
}

// Season groups episodes; season 0 is specials (unmonitored by default).
type Season struct {
	ID        int64
	Number    int
	Monitored bool
	Episodes  []Episode
}

// Episode is one wanted unit of a series.
type Episode struct {
	ID            int64
	SeasonNumber  int
	EpisodeNumber int
	AbsoluteNum   int // 0 = unknown
	Title         string
	AirDate       string // ISO date, "" if unknown
	Monitored     bool
	HasFile       bool
}

// RootKind is what a root folder holds. It is MediaKind plus KindMixed,
// which is a property of folders and never of items — hence a distinct type
// rather than a fourth MediaKind (ADR 0009).
type RootKind string

// KindMixed means "this folder holds more than one kind, so ask". It is the
// value every pre-ADR-0009 root migrates to, and it is defined to behave
// exactly as roots behaved before kinds existed.
const KindMixed RootKind = "mixed"

// RootKindOf lifts a MediaKind into a RootKind.
func RootKindOf(k MediaKind) RootKind { return RootKind(k) }

// ValidRootKind reports whether k is one of the three media kinds or mixed.
func ValidRootKind(k RootKind) bool {
	return k == KindMixed || ValidKind(MediaKind(k))
}

// Media returns the media kind a root is restricted to, and false when the
// root is mixed and therefore restricts nothing.
func (k RootKind) Media() (MediaKind, bool) {
	if k == KindMixed || !ValidRootKind(k) {
		return "", false
	}
	return MediaKind(k), true
}

// Accepts reports whether an item of kind m may live in a root of kind k.
// A mixed root accepts everything; a typed root accepts only its own kind.
func (k RootKind) Accepts(m MediaKind) bool {
	want, restricted := k.Media()
	return !restricted || want == m
}

// RootFolder is a library root on disk. One shared system serves every kind
// (ADR 0002); Kind records which kind this particular root holds so that
// adoption, the Add flow, and the compat personalities can route correctly
// (ADR 0009).
type RootFolder struct {
	ID      int64
	Path    string
	Kind    RootKind
	AddedAt time.Time
}

// IgnoredPath is a directory the user dismissed as not-media. Without this
// record a scan re-offers every non-media folder forever (ADR 0009 §4).
type IgnoredPath struct {
	Path      string
	Reason    string
	IgnoredAt time.Time
}

// MediaFile is a file on disk belonging to the library. For series it links
// to 1..n episodes (multi-episode files are real — ADR 0002); for movies it
// links to the item alone. MediaItemID 0 means the file is unmatched.
type MediaFile struct {
	ID          int64
	MediaItemID int64
	CopyID      int64 // 0 = the primary copy
	Path        string
	Size        int64
	EpisodeIDs  []int64
	AddedAt     time.Time
}
