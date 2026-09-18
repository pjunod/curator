package acquisition

import (
	"context"
	"fmt"
	"sync"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/quality"
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
// missing or upgradable. Series contribute per-episode wantables (season packs
// are a search strategy, not a wanted unit).
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
		switch item.Kind {
		case domain.KindMovie, domain.KindBook:
			w, err := s.target(ctx, item, 0, 0)
			if err != nil {
				continue
			}
			if wants(profile, w) {
				out = append(out, w)
			}
		case domain.KindSeries:
			epStates, err := s.episodeStates(ctx, item, 0)
			if err != nil {
				continue
			}
			out = append(out, s.wantedEpisodes(item, profile, epStates, nil)...)
		}

		// Additional targets: a video copy or the other book edition wants its
		// own file set at its own profile.
		for i := range item.Copies {
			cp := item.Copies[i]
			if !cp.Monitored {
				continue
			}
			copyProfile, err := s.db.GetProfile(ctx, cp.QualityProfileID)
			if err != nil {
				continue
			}
			switch item.Kind {
			case domain.KindMovie, domain.KindBook:
				w, err := s.targetCopy(ctx, item, 0, 0, &cp)
				if err != nil {
					continue
				}
				if wants(copyProfile, w) {
					out = append(out, w)
				}
			case domain.KindSeries:
				epStates, err := s.episodeStates(ctx, item, cp.ID)
				if err != nil {
					continue
				}
				out = append(out, s.wantedEpisodes(item, copyProfile, epStates, &cp)...)
			}
		}
	}
	return out, nil
}

// wants is THE line this whole phase exists to change (ADR 0013 §5).
//
// Three states, three answers:
//
//   - Nothing on disk: wanted. This is what "missing" means, and it still
//     hunts exactly as before.
//   - Files on disk whose quality could not be determined: NOT wanted. The
//     files are right there. Hunting a replacement for something we simply
//     failed to measure is how a 17 GB library file got a duplicate grabbed
//     on top of it and the original left behind with no story.
//   - Files with a known quality: wanted only while upgrades are on and the
//     profile's target is not met — where "met" carries the don't-churn rule,
//     so an unverified SOURCE at the target resolution counts as done.
func wants(profile quality.Profile, w domain.Wantable) bool {
	current, known := w.CurrentQuality()
	if !known {
		return !w.OnDisk()
	}
	return profile.UpgradesAllowed && !profile.Met(current, w.SourceVerified())
}

// wantedEpisodes builds the episode wantables for one copy of a series (cp nil
// = the primary).
func (s *Service) wantedEpisodes(item domain.MediaItem, profile quality.Profile,
	states map[int64]episodeState, cp *domain.MediaCopy) []domain.Wantable {
	profileID := item.QualityProfileID
	var copyID int64
	copyName := ""
	if cp != nil {
		profileID, copyID, copyName = cp.QualityProfileID, cp.ID, copyLabel(*cp)
	}
	var out []domain.Wantable
	for _, season := range item.Seasons {
		if !season.Monitored {
			continue
		}
		for _, e := range season.Episodes {
			if !e.Monitored || e.AirDate == "" {
				continue
			}
			st := states[e.ID]
			ep := domain.EpisodeWantable{
				Item: item.ID, EpisodeID: e.ID, Profile: profileID,
				Mon: true, Title: item.Title, Year: item.Year,
				Identity: mediaIdentity(item),
				Season:   e.SeasonNumber, Episode: e.EpisodeNumber,
				Have: st.Have, Files: st.HasFile, Verified: st.Verified,
				Absolute: e.AbsoluteNum, Copy: copyID, CopyName: copyName,
			}
			if wants(profile, ep) {
				out = append(out, ep)
			}
		}
	}
	return out
}

// notInFlight filters out wantables that already have an active download —
// including episodes covered by an in-flight season pack.
//
// "Active" means work that is actually still moving: grabbed, downloading,
// downloaded, awaiting import, importing. It deliberately does NOT include
// terminal rows.
//
// This used to read every row that was not 'imported', which is every row
// that ever failed — and a download row stays 'failed' forever, because
// nothing ever transitions it out and only the user deleting it from the
// queue removes it. Three separate paths park a row there: handleFailure (the
// client reported the download broke), handleRemoved (the user deleted the
// job in their client), and the import handoff. So one failure of any kind
// made its wantable invisible to *every* search path — Auto Search, the RSS
// sync, and the backlog pass — permanently, and silently. handleRemoved's own
// contract ("the want stays wanted... the next deliberate search still finds
// it") was not true.
//
// Suppressing on terminal rows was also redundant: a release that failed is
// blocklisted, which is the mechanism that stops us re-grabbing the same bad
// copy. Suppressing the whole wantable stopped us grabbing a DIFFERENT one,
// which is exactly what should happen next.
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
			// A season pack in flight FOR THE SAME COPY covers this episode.
			packID := fmt.Sprintf("season:%d:%d", ep.Item, ep.Season)
			if ep.Copy != 0 {
				packID = fmt.Sprintf("%s:c%d", packID, ep.Copy)
			}
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
