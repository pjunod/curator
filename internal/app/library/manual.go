package library

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/filename"
	"github.com/pjunod/monarr/internal/domain/parser"
)

// ManualRequest describes a library record no provider backs (ADR 0012).
//
// The user supplies what they know — a title, a kind, maybe a year — and the
// folder. Everything else that a provider would have given is absent by
// definition, and nothing here invents it.
type ManualRequest struct {
	Kind  domain.MediaKind
	Title string
	Year  int
	// Author is the book case; ignored for movies and series.
	Author string
	// Path is the folder on disk. Required: a manual entry exists because
	// files exist, and its episode list is read from them.
	Path string
	// Overview is optional prose the user typed.
	Overview string
}

// AddManual creates a record for media no provider has right.
//
// The escape hatch ADR 0012 exists for: programmes no database carries, home
// video, anything a user wants filed their own way. Before this, the only
// exit from review was dismissal, which left a folder on disk unmanaged and
// invisible — a media manager telling its user their files are the problem.
func (s *Service) AddManual(ctx context.Context, req ManualRequest) (domain.MediaItem, error) {
	title := strings.TrimSpace(req.Title)
	if title == "" {
		return domain.MediaItem{}, fmt.Errorf("%w: a manual entry needs a title", ErrInvalidInput)
	}
	switch req.Kind {
	case domain.KindMovie, domain.KindSeries, domain.KindBook:
	default:
		return domain.MediaItem{}, fmt.Errorf("%w: %q", ErrUnsupportedKind, req.Kind)
	}
	if !filepath.IsAbs(req.Path) {
		return domain.MediaItem{}, fmt.Errorf("%w: a manual entry needs an absolute folder", ErrInvalidInput)
	}
	clean := filepath.Clean(req.Path)

	// The same check adoption makes, for the same reason: the library must
	// not be aimable at arbitrary places on the host by anyone who can post
	// JSON (see rootFolderFor).
	root, err := s.rootFolderFor(ctx, clean)
	if err != nil {
		return domain.MediaItem{}, err
	}
	if info, statErr := os.Stat(clean); statErr != nil || !info.IsDir() {
		return domain.MediaItem{}, fmt.Errorf("%w: %s is not a directory on disk", ErrNotFound, clean)
	}
	if !root.Kind.Accepts(req.Kind) {
		return domain.MediaItem{}, fmt.Errorf("%w: %s holds %s", ErrRootKindMismatch, root.Path, root.Kind)
	}
	if err := s.ensureFolderFree(ctx, clean, 0); err != nil {
		return domain.MediaItem{}, err
	}

	item := domain.MediaItem{
		Kind:      req.Kind,
		Source:    domain.SourceManual,
		Title:     title,
		SortTitle: domain.SortTitle(title),
		Year:      req.Year,
		Author:    req.Author,
		Overview:  strings.TrimSpace(req.Overview),
		Path:      clean,

		RootFolderID: root.ID,
		// Created unmonitored (ADR 0012 §4). Title matching without a
		// provider's alternate names is materially weaker, and the failure
		// mode is grabbing something unrelated — so switching it on is a
		// deliberate act, not a default.
		Monitored: false,
	}
	if req.Kind == domain.KindSeries {
		item.Seasons = episodesFromDisk(clean)
	}

	id, err := s.db.CreateMediaItem(ctx, item)
	if err != nil {
		return domain.MediaItem{}, folderTakenErr(err, clean)
	}
	s.log.Info("library: manual entry created",
		"kind", req.Kind, "title", title, "path", clean, "seasons", len(item.Seasons))
	s.publish(MediaAdded{ID: id, Kind: string(req.Kind), Title: title})

	// The folder is answered for now, so stop offering it.
	s.dropFromOutstanding(ctx, clean)
	return s.Get(ctx, id)
}

// episodesFromDisk builds a season/episode list by reading the folder.
//
// This inverts the usual relationship, and deliberately (ADR 0012 §2): for a
// provider-backed series the metadata is the truth and files are matched to
// it, while a manual entry has no metadata to be true — **the files are**.
// The parser doing the reading is the same one acquisition matches releases
// with, so the numbering agrees with release matching by construction rather
// than by coincidence.
//
// Files that carry no episode number are skipped rather than guessed at. A
// folder of unnumbered videos is a real thing (home video), and inventing
// S01E01…E0n for it would create episodes whose numbers mean nothing and
// which a later, correctly-named file would then collide with.
func episodesFromDisk(dir string) []domain.Season {
	type key struct{ season, episode int }
	seen := map[key]domain.Episode{}

	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !filename.IsVideo(path) {
			return nil //nolint:nilerr // an unreadable subdirectory is not fatal
		}
		p := parser.Parse(filepath.Base(path))
		if len(p.Episodes) == 0 {
			return nil
		}
		for _, ep := range p.Episodes {
			k := key{p.Season, ep}
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = domain.Episode{
				SeasonNumber:  p.Season,
				EpisodeNumber: ep,
				// No air dates and no titles: nothing on disk knows them,
				// and a blank is honest where a guess is not.
				Monitored: false,
			}
		}
		return nil
	})

	bySeason := map[int][]domain.Episode{}
	for k, e := range seen {
		bySeason[k.season] = append(bySeason[k.season], e)
	}
	numbers := make([]int, 0, len(bySeason))
	for n := range bySeason {
		numbers = append(numbers, n)
	}
	sort.Ints(numbers)

	out := make([]domain.Season, 0, len(numbers))
	for _, n := range numbers {
		eps := bySeason[n]
		sort.Slice(eps, func(i, j int) bool { return eps[i].EpisodeNumber < eps[j].EpisodeNumber })
		out = append(out, domain.Season{Number: n, Monitored: false, Episodes: eps})
	}
	return out
}

// RescanManual re-reads a manual series' folder and adds episodes for files
// that have appeared since.
//
// Never removes: a missing file is the normal state of a monitored episode,
// and deleting the episode when its file goes would erase the record of what
// the series contains every time a disk is unmounted.
func (s *Service) RescanManual(ctx context.Context, id int64) (domain.MediaItem, error) {
	item, err := s.db.GetMediaItemFull(ctx, id)
	if err != nil {
		return domain.MediaItem{}, err
	}
	if !item.IsManual() {
		return domain.MediaItem{}, fmt.Errorf(
			"%w: %q came from %s and is refreshed from there", ErrUnsupportedKind, item.Title, item.Source)
	}
	if item.Kind != domain.KindSeries || item.Path == "" {
		return item, nil
	}

	have := map[[2]int]bool{}
	for _, season := range item.Seasons {
		for _, e := range season.Episodes {
			have[[2]int{e.SeasonNumber, e.EpisodeNumber}] = true
		}
	}
	fresh := episodesFromDisk(item.Path)

	// Merge: stored seasons keep their monitored flags, new episodes arrive
	// unmonitored, and nothing already known is touched.
	merged := append([]domain.Season(nil), item.Seasons...)
	added := 0
	for _, season := range fresh {
		idx := -1
		for i := range merged {
			if merged[i].Number == season.Number {
				idx = i
				break
			}
		}
		if idx == -1 {
			merged = append(merged, domain.Season{Number: season.Number})
			idx = len(merged) - 1
		}
		for _, e := range season.Episodes {
			if have[[2]int{e.SeasonNumber, e.EpisodeNumber}] {
				continue
			}
			e.Monitored = merged[idx].Monitored
			merged[idx].Episodes = append(merged[idx].Episodes, e)
			added++
		}
	}
	if added == 0 {
		return item, nil
	}

	sort.Slice(merged, func(i, j int) bool { return merged[i].Number < merged[j].Number })
	item.Seasons = merged
	if err := s.db.UpdateMediaItemMetadata(ctx, id, item); err != nil {
		return domain.MediaItem{}, err
	}
	s.log.Info("library: manual entry rescanned", "title", item.Title, "new episodes", added)
	return s.Get(ctx, id)
}
