package acquisition

import (
	"context"
	"fmt"
	"os"
	"slices"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

type householdAdmissionKey struct{}
type householdAdmission struct{ ID, Generation int64 }

func (s *Service) SetHouseholdVerifier(check func(context.Context, int64) error) {
	s.householdCheck = check
}

// AcquireHouseholdTarget reuses matching, ranking, reservations and the existing
// automatic handoff, but never invokes broad monitoring or a season planner.
func (s *Service) AcquireHouseholdTarget(ctx context.Context, target domain.HouseholdTarget) (int64, error) {
	current, _, err := s.db.HouseholdTargetPolicy(ctx, target.ID)
	if err != nil {
		return 0, err
	}
	if current.ApprovalGeneration != target.ApprovalGeneration {
		return 0, sqlite.ErrRequestChanged
	}
	if current.DownloadID != 0 {
		return current.DownloadID, nil
	}
	item, err := s.db.GetMediaItemFull(ctx, target.MediaItemID)
	if err != nil {
		return 0, err
	}
	w, err := s.target(ctx, item, target.Season, target.Episode)
	if err != nil {
		return 0, err
	}
	// The request's verified missing primary leaf is the authority. Old file rows
	// and persistent monitoring do not authorize a wider search or an upgrade.
	switch v := w.(type) {
	case domain.MovieWantable:
		v.Mon = true
		v.Have = nil
		v.Files = false
		v.Verified = false
		v.Audio = quality.Audio{}
		w = v
	case domain.EpisodeWantable:
		v.Mon = true
		v.Have = nil
		v.Files = false
		v.Verified = false
		v.Audio = quality.Audio{}
		w = v
	default:
		return 0, sqlite.ErrRequestChanged
	}
	enabled, err := s.enabledIndexers(ctx)
	if err != nil {
		return 0, err
	}
	if len(enabled) == 0 {
		return 0, ErrNoIndexers
	}
	release, err := s.reservations.acquire(ctx, w)
	if err != nil {
		return 0, err
	}
	defer release()
	ctx = context.WithValue(ctx, householdAdmissionKey{}, householdAdmission{target.ID, target.ApprovalGeneration})
	allowed := candidatePredicate(nil)
	if item.Kind == domain.KindSeries {
		allowed = exactWantedCandidate
	}
	tally, err := s.searchAndGrabBestReserved(ctx, w, enabled, allowed, func() error {
		if capped, _ := s.regrabCapped(ctx, w); capped {
			return sqlite.ErrRequestChanged
		}
		return nil
	})
	if err != nil {
		return tally.DownloadID, err
	}
	return tally.DownloadID, nil
}

// CancelHouseholdOffloadDeliveries uses the same exact-scope notification
// narrowing as Re-acquire. Approved leaves' flags were cleared atomically, so
// later retries cannot resurrect a previously excluded path.
func (s *Service) CancelHouseholdOffloadDeliveries(ctx context.Context, itemID int64, scope domain.RequestScope, operations []string) error {
	item, err := s.db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		return err
	}
	sc := reacquireScope{item: item, episodes: map[int64]bool{}}
	if item.Kind == domain.KindMovie {
		sc.clear = "movie"
	} else {
		sc.clear = "episode"
		for _, sn := range item.Seasons {
			for _, ep := range sn.Episodes {
				for _, coordinate := range scope.Episodes {
					if coordinate.Season == ep.SeasonNumber && coordinate.Episode == ep.EpisodeNumber {
						sc.episodes[ep.ID] = true
					}
				}
			}
		}
	}
	return s.cancelOffloadDeliveriesRestricted(ctx, sc, operations)
}
func (s *Service) validateHouseholdImport(ctx context.Context, item domain.MediaItem, scope importScope, p sqlite.Placement) error {
	origin := scope.Household
	if origin == nil {
		return nil
	}
	identity, generation, err := s.db.PresenceRootBinding(ctx, origin.RootID, origin.RootPath)
	if err != nil {
		return err
	}
	if identity != origin.RootIdentity || generation != origin.RootGeneration {
		return importPolicyRefusalf("household destination volume changed")
	}
	if item.ID != origin.ItemID || item.Kind != origin.Kind || item.IDs.TMDB != origin.TMDBID || scope.CopyID != 0 || item.Path != origin.Path || item.RootFolderID != origin.RootID {
		return importPolicyRefusalf("household import identity or destination changed")
	}
	if item.Kind == domain.KindSeries {
		if len(p.EpisodeIDs) == 0 {
			return importPolicyRefusalf("household episode file has no exact episode mapping")
		}
		for _, id := range p.EpisodeIDs {
			if !slices.Contains(origin.EpisodeIDs, id) {
				return importPolicyRefusalf("file contains an episode outside household approval")
			}
		}
	}
	// Preserve an arriving primary at any quality. Household requests authorize
	// missing files, never implicit replacements, including a newly indexed file.
	files, err := s.db.ListFilesForItem(ctx, item.ID)
	if err != nil {
		return err
	}
	for _, file := range files {
		if file.CopyID != 0 {
			continue
		}
		overlap := item.Kind == domain.KindMovie
		for _, ep := range file.EpisodeIDs {
			overlap = overlap || slices.Contains(p.EpisodeIDs, ep)
		}
		if !overlap {
			continue
		}
		info, err := os.Lstat(file.Path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return importPolicyRefusalf("primary file identity needs owner review")
		}
		if file.Path == p.Target {
			digest, _, e := fileDigest(ctx, file.Path)
			if e != nil {
				return e
			}
			if digest == p.SHA256 && file.SourceGeneration == scope.SourceGeneration && file.SourceTransfer == scope.Transfer {
				continue
			}
		}
		return importPolicyRefusalf("primary file arrived; household approval does not authorize an upgrade")
	}
	if p.PreviousSHA256 != "" && p.PreviousSHA256 != p.SHA256 {
		return importPolicyRefusalf("household placement would replace an existing file")
	}
	if s.householdCheck == nil {
		return fmt.Errorf("household primary verification is unavailable")
	}
	return nil
}
