package acquisition

import (
	"context"
	"fmt"
	"sync"

	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/domain/quality"
)

// wantedIndex caches "everything the system still wants": monitored
// wantables that are missing or below their profile cutoff (blueprint §5.1).
// It is invalidated by library/import events and rebuilt lazily.
type wantedIndex struct {
	mu    sync.Mutex
	items []domain.Wantable
	fresh bool
}

// InvalidateWanted marks the index stale; the next Wanted() rebuilds it.
// Wired to library events in cmd/monarr (adds, imports, scans).
func (s *Service) InvalidateWanted() {
	s.wanted.mu.Lock()
	s.wanted.fresh = false
	s.wanted.mu.Unlock()
}

// Wanted returns the current wanted list, rebuilding it if stale.
func (s *Service) Wanted(ctx context.Context) ([]domain.Wantable, error) {
	s.wanted.mu.Lock()
	defer s.wanted.mu.Unlock()
	if s.wanted.fresh {
		return s.wanted.items, nil
	}
	items, err := s.buildWanted(ctx)
	if err != nil {
		return nil, err
	}
	s.wanted.items, s.wanted.fresh = items, true
	return items, nil
}

// buildWanted walks the library and collects monitored wantables that are
// missing or upgradable. Series contribute per-episode wantables (season
// packs are a search strategy, not a wanted unit).
func (s *Service) buildWanted(ctx context.Context) ([]domain.Wantable, error) {
	summaries, err := s.db.ListMediaItems(ctx, "")
	if err != nil {
		return nil, err
	}
	var out []domain.Wantable
	for _, sum := range summaries {
		if !sum.Monitored {
			continue
		}
		item, err := s.db.GetMediaItemFull(ctx, sum.ID)
		if err != nil {
			continue
		}
		profile, err := s.db.GetProfile(ctx, item.QualityProfileID)
		if err != nil {
			continue
		}
		wants := func(q *quality.Quality) bool {
			if q == nil {
				return true // missing
			}
			return profile.UpgradesAllowed && !profile.MeetsCutoff(*q)
		}
		switch item.Kind {
		case domain.KindMovie, domain.KindBook:
			w, err := s.target(ctx, item, 0, 0)
			if err != nil {
				continue
			}
			var have *quality.Quality
			if q, ok := w.CurrentQuality(); ok {
				have = &q
			}
			if wants(have) {
				out = append(out, w)
			}
		case domain.KindSeries:
			epQuals, err := s.episodeQualities(ctx, item)
			if err != nil {
				continue
			}
			for _, season := range item.Seasons {
				if !season.Monitored {
					continue
				}
				for _, e := range season.Episodes {
					if !e.Monitored || e.AirDate == "" {
						continue
					}
					if wants(epQuals[e.ID]) {
						out = append(out, domain.EpisodeWantable{
							Item: item.ID, EpisodeID: e.ID, Profile: item.QualityProfileID,
							Mon: true, Title: item.Title, Year: item.Year,
							Season: e.SeasonNumber, Episode: e.EpisodeNumber,
							Have: epQuals[e.ID], Absolute: e.AbsoluteNum,
						})
					}
				}
			}
		}
	}
	return out, nil
}

// notInFlight filters out wantables that already have an active download —
// including episodes covered by an in-flight season pack.
func (s *Service) notInFlight(ctx context.Context, wanted []domain.Wantable) []domain.Wantable {
	active, err := s.db.ListActiveDownloads(ctx)
	if err != nil {
		return wanted
	}
	inFlight := map[string]bool{}
	for _, dl := range active {
		for _, id := range dl.WantableIDs {
			inFlight[id] = true
		}
	}
	var out []domain.Wantable
	for _, w := range wanted {
		id := string(w.ID())
		if inFlight[id] {
			continue
		}
		if ep, ok := w.(domain.EpisodeWantable); ok {
			// season:<item>:<season> pack covers this episode.
			packID := fmt.Sprintf("season:%d:%d", ep.Item, ep.Season)
			if inFlight[packID] {
				continue
			}
		}
		out = append(out, w)
	}
	return out
}

// wantableGrabTarget maps a wantable to the grab request coordinates.
func wantableGrabTarget(w domain.Wantable) (season, episode int) {
	switch t := w.(type) {
	case domain.EpisodeWantable:
		return t.Season, t.Episode
	case domain.SeasonWantable:
		return t.Season, 0
	}
	return -1, 0
}
