package acquisition

import (
	"context"
	"fmt"
	"slices"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/mediainfo"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

// validateMeasuredPlacement runs under the import lock, after measurement and
// before staging/rename. Claim-based permission cannot authorize destruction.
func (s *Service) validateMeasuredPlacement(ctx context.Context, item domain.MediaItem, scope importScope, p sqlite.Placement, replace bool) error {
	if scope.ProfileID == 0 {
		return nil
	} // journal-only callers have no policy scope
	profile, err := s.db.GetProfile(ctx, scope.ProfileID)
	if err != nil {
		return err
	}
	langs := importLanguages(scope.Release, p.Source)
	if measured, known := p.Info.AudioLanguages(); known {
		langs = measured
	}
	if !scope.Manual && (p.Provenance == mediainfo.ProvenanceImplausible || !profile.Acceptable(p.Quality) || !profile.LanguageAcceptable(langs)) {
		return fmt.Errorf("measured payload does not satisfy current floor, resolution or language policy")
	}
	files, err := s.db.ListFilesForItem(ctx, item.ID)
	if err != nil {
		return err
	}
	records, err := s.db.FileQualityRecords(ctx, item.ID)
	if err != nil {
		return err
	}
	byID := map[int64]sqlite.FileQuality{}
	for _, r := range records {
		byID[r.FileID] = r
	}
	for _, f := range files {
		if f.CopyID != scope.CopyID {
			continue
		}
		overlap := len(p.EpisodeIDs) == 0
		for _, e := range f.EpisodeIDs {
			if slices.Contains(p.EpisodeIDs, e) {
				overlap = true
			}
		}
		if !overlap {
			continue
		}
		// A receipt for the same bytes is idempotent; it does not replace them.
		if f.Path == p.Target {
			if digest, _, e := fileDigest(ctx, f.Path); e == nil && digest == p.SHA256 {
				continue
			}
		}
		for _, e := range f.EpisodeIDs {
			if len(p.EpisodeIDs) > 0 && !slices.Contains(p.EpisodeIDs, e) {
				return fmt.Errorf("replacement overlaps protected sibling episode %d", e)
			}
		}
		r := byID[f.ID]
		if r.Provenance == mediainfo.ProvenanceImplausible {
			continue
		}
		if scope.Manual && !replace && f.Path != p.Target {
			continue
		}
		if !r.Known {
			return fmt.Errorf("existing file quality is unverified")
		}
		allowed := profile.UpgradesAllowed && profile.Upgrade(p.Quality, langs, r.Quality, r.SourceVerified(), r.AudioOf())
		if scope.Manual {
			allowed = profile.Upgrade(p.Quality, langs, r.Quality, r.SourceVerified(), r.AudioOf()) || (quality.Better(p.Quality, r.Quality) && profile.LanguageAcceptable(langs))
		}
		if !allowed {
			return fmt.Errorf("measured %s cannot replace existing %s under current policy", p.Quality.Display(), r.Quality.Display())
		}
		// A claim permitting cleanup must be validated against these same facts.
		if !replace {
			return fmt.Errorf("replacement requires measured authorization")
		}
	}
	return nil
}
