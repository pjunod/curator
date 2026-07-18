package domain

import (
	"strings"
	"time"
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

	Monitored        bool
	QualityProfileID int64
	RootFolderID     int64  // 0 = none assigned
	Path             string // absolute on-disk folder; "" if unassigned

	// Series-only.
	Ended   bool
	Seasons []Season

	Files []MediaFile

	// Completeness, derived for list views (hydrated by ListMediaItems, zero
	// elsewhere): monitored aired episodes vs those with files, and the raw
	// file count for movies/books.
	EpisodeCount     int
	EpisodeFileCount int
	FileCount        int

	AddedAt   time.Time
	UpdatedAt time.Time
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

// RootFolder is a library root on disk (one shared system for every kind —
// ADR 0002).
type RootFolder struct {
	ID      int64
	Path    string
	AddedAt time.Time
}

// MediaFile is a file on disk belonging to the library. For series it links
// to 1..n episodes (multi-episode files are real — ADR 0002); for movies it
// links to the item alone. MediaItemID 0 means the file is unmatched.
type MediaFile struct {
	ID          int64
	MediaItemID int64
	Path        string
	Size        int64
	EpisodeIDs  []int64
	AddedAt     time.Time
}
