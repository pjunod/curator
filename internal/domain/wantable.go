package domain

import (
	"fmt"

	"github.com/monarr-media/monarr/internal/domain/quality"
)

// WantableID is a stable identity: "movie:42", "episode:42:2:5", "season:42:2".
type WantableID string

// Wantable is "a thing the system wants on disk at a given quality" — the
// contract the entire acquisition pipeline is built against (blueprint §4.1).
type Wantable interface {
	ID() WantableID
	MediaItemID() int64
	ProfileID() int64
	Monitored() bool
	// CurrentQuality is the best quality already on disk; ok=false when
	// nothing is on disk (a missing wantable).
	CurrentQuality() (quality.Quality, bool)
}

// MovieWantable is the degenerate case: one item, one file.
type MovieWantable struct {
	Item    int64
	Profile int64
	Mon     bool
	Have    *quality.Quality
	Title   string
	Year    int
}

// ID implements Wantable.
func (m MovieWantable) ID() WantableID { return WantableID(fmt.Sprintf("movie:%d", m.Item)) }

// MediaItemID implements Wantable.
func (m MovieWantable) MediaItemID() int64 { return m.Item }

// ProfileID implements Wantable.
func (m MovieWantable) ProfileID() int64 { return m.Profile }

// Monitored implements Wantable.
func (m MovieWantable) Monitored() bool { return m.Mon }

// CurrentQuality implements Wantable.
func (m MovieWantable) CurrentQuality() (quality.Quality, bool) {
	if m.Have == nil {
		return quality.Quality{}, false
	}
	return *m.Have, true
}

// EpisodeWantable is one episode of a series.
type EpisodeWantable struct {
	Item      int64
	EpisodeID int64
	Profile   int64
	Mon       bool
	Have      *quality.Quality
	Title     string // series title
	Year      int
	Season    int
	Episode   int
}

// ID implements Wantable.
func (e EpisodeWantable) ID() WantableID {
	return WantableID(fmt.Sprintf("episode:%d:%d:%d", e.Item, e.Season, e.Episode))
}

// MediaItemID implements Wantable.
func (e EpisodeWantable) MediaItemID() int64 { return e.Item }

// ProfileID implements Wantable.
func (e EpisodeWantable) ProfileID() int64 { return e.Profile }

// Monitored implements Wantable.
func (e EpisodeWantable) Monitored() bool { return e.Mon }

// CurrentQuality implements Wantable.
func (e EpisodeWantable) CurrentQuality() (quality.Quality, bool) {
	if e.Have == nil {
		return quality.Quality{}, false
	}
	return *e.Have, true
}

// SeasonWantable is the season-pack search target: one download that
// satisfies many episode wantables at import time — Sonarr's hardest
// structural feature, modeled from day one (blueprint §4.1).
type SeasonWantable struct {
	Item     int64
	Profile  int64
	Mon      bool
	Title    string
	Year     int
	Season   int
	Episodes []EpisodeWantable // the episodes a pack would satisfy
}

// ID implements Wantable.
func (s SeasonWantable) ID() WantableID {
	return WantableID(fmt.Sprintf("season:%d:%d", s.Item, s.Season))
}

// MediaItemID implements Wantable.
func (s SeasonWantable) MediaItemID() int64 { return s.Item }

// ProfileID implements Wantable.
func (s SeasonWantable) ProfileID() int64 { return s.Profile }

// Monitored implements Wantable.
func (s SeasonWantable) Monitored() bool { return s.Mon }

// CurrentQuality implements Wantable: the WORST quality across the season's
// episodes (a pack upgrades the season only if it beats the weakest link);
// ok=false if any episode is missing entirely.
func (s SeasonWantable) CurrentQuality() (quality.Quality, bool) {
	var worst *quality.Quality
	for _, e := range s.Episodes {
		q, ok := e.CurrentQuality()
		if !ok {
			return quality.Quality{}, false
		}
		if worst == nil || quality.Better(*worst, q) {
			qq := q
			worst = &qq
		}
	}
	if worst == nil {
		return quality.Quality{}, false
	}
	return *worst, true
}

// BookWantable is one book (ADR 0006): like a movie, one item and one file,
// but matched by author+title and graded on format instead of resolution.
type BookWantable struct {
	Item    int64
	Profile int64
	Mon     bool
	Have    *quality.Quality
	Title   string
	Author  string
	Year    int
}

// ID implements Wantable.
func (b BookWantable) ID() WantableID { return WantableID(fmt.Sprintf("book:%d", b.Item)) }

// MediaItemID implements Wantable.
func (b BookWantable) MediaItemID() int64 { return b.Item }

// ProfileID implements Wantable.
func (b BookWantable) ProfileID() int64 { return b.Profile }

// Monitored implements Wantable.
func (b BookWantable) Monitored() bool { return b.Mon }

// CurrentQuality implements Wantable.
func (b BookWantable) CurrentQuality() (quality.Quality, bool) {
	if b.Have == nil {
		return quality.Quality{}, false
	}
	return *b.Have, true
}

// SearchQuery is what a planner emits for indexers (blueprint §4.1).
type SearchQuery struct {
	Q       string
	Season  int // 0 = unset
	Episode int // 0 = unset
	// Kind steers indexer category selection (books → Torznab 7000s/3030).
	Kind MediaKind
}

// PlanSearch builds the indexer queries for a wantable — one of exactly two
// places media-kind knowledge lives (the other is the matcher).
func PlanSearch(w Wantable) []SearchQuery {
	switch t := w.(type) {
	case MovieWantable:
		q := t.Title
		if t.Year > 0 {
			q = fmt.Sprintf("%s %d", t.Title, t.Year)
		}
		return []SearchQuery{{Q: q, Kind: KindMovie}}
	case EpisodeWantable:
		return []SearchQuery{{
			Q:      fmt.Sprintf("%s S%02dE%02d", t.Title, t.Season, t.Episode),
			Season: t.Season, Episode: t.Episode, Kind: KindSeries,
		}}
	case SeasonWantable:
		return []SearchQuery{{
			Q:      fmt.Sprintf("%s S%02d", t.Title, t.Season),
			Season: t.Season, Kind: KindSeries,
		}}
	case BookWantable:
		if t.Author == "" {
			return []SearchQuery{{Q: t.Title, Kind: KindBook}}
		}
		// Author+title is the discriminating query; title-only casts wider.
		return []SearchQuery{
			{Q: fmt.Sprintf("%s %s", t.Author, t.Title), Kind: KindBook},
			{Q: t.Title, Kind: KindBook},
		}
	default:
		return nil
	}
}
