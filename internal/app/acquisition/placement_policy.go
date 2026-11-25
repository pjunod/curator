package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"slices"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/hdr"
	"github.com/pjunod/monarr/internal/domain/mediainfo"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/infra/probe"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

// validateMeasuredPlacement runs under the import lock, after measurement and
// before staging/rename. Claim-based permission cannot authorize destruction.
func (s *Service) validateMeasuredPlacement(ctx context.Context, item domain.MediaItem, scope importScope, p sqlite.Placement, replace bool) error {
	if err := s.validateHouseholdImport(ctx, item, scope, p); err != nil {
		return err
	}
	if scope.ProfileID == 0 {
		return nil
	} // journal-only callers have no policy scope
	profile, err := s.db.GetProfile(ctx, scope.ProfileID)
	if err != nil {
		return err
	}
	langs := importLanguages(scope.Release, p.Source)
	measuredHDR := hdr.Evidence{}
	if p.Info.Measured() {
		measuredHDR = hdr.Measure(p.Info.HDRTier())
	}
	if measured, known := p.Info.AudioLanguages(); known {
		langs = measured
	}
	// Fresh fills retain measured facts even when they need a later upgrade.
	// Destructive replacements must satisfy policy before publication.
	if replace && (p.Provenance == mediainfo.ProvenanceImplausible || (!scope.Manual && (!profile.Acceptable(p.Quality) || !profile.LanguageAcceptable(langs)))) {
		s.recordLanguageMismatch(ctx, item.ID, p.Target, scope.Release, scope.Indexer, profile, p.Info)
		return importPolicyRefusalf("measured payload does not satisfy current floor, resolution or language policy")
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
	targetAuthorized := p.PreviousSHA256 == "" || p.PreviousSHA256 == p.SHA256
	targetTracked := false
	for _, f := range files {
		if f.CopyID == scope.CopyID && f.Path == p.Target {
			targetTracked = true
		}
		if f.CopyID != scope.CopyID {
			continue
		}
		if f.Path != p.Target && scope.DeferCleanup && scope.PublishedDigests[f.Path] != "" {
			digest, _, e := fileDigest(ctx, f.Path)
			if e != nil {
				return e
			}
			if digest == scope.PublishedDigests[f.Path] {
				continue
			}
		}
		overlap := len(p.EpisodeIDs) == 0 || f.Path == p.Target
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
			digest, _, e := fileDigest(ctx, f.Path)
			if e != nil {
				return e
			}
			if digest == p.SHA256 {
				continue
			}
		}
		for _, e := range f.EpisodeIDs {
			if len(p.EpisodeIDs) > 0 && !slices.Contains(p.EpisodeIDs, e) {
				// A reviewed replacement operation publishes beside the old
				// target and later detaches only the selected episode links. The
				// shared source remains in place for its unselected episodes, so
				// it is safe for this placement to overlap that source record.
				// Ordinary imports still cannot replace a shared file implicitly.
				if scope.ReplacementOperationID != "" && filepath.Clean(f.Path) != filepath.Clean(p.Target) {
					continue
				}
				return importLibraryRefusalf("replacement overlaps protected sibling episode %d", e)
			}
		}
		r := byID[f.ID]
		if r.Provenance == mediainfo.ProvenanceImplausible {
			// A disbelieved file holds no episode (episodeStates,
			// DiskStateForItem), so it protects nothing. When it occupies the
			// destination itself, its bytes still need authorization to be
			// replaced; without it the want stays open and every grab is
			// refused here forever (Good Trouble S02E06, 2026-11-25).
			if f.Path == p.Target {
				if err := authorizeDisbelievedTarget(item, p); err != nil {
					return err
				}
				targetAuthorized = true
			}
			continue
		}
		if scope.Manual && !replace && f.Path != p.Target {
			continue
		}
		if !r.Known {
			return importLibraryRefusalf("existing file quality is unverified")
		}
		allowed := profile.UpgradesAllowed && profile.Upgrade(p.Quality, langs, measuredHDR, r.Quality, r.SourceVerified(), r.AudioOf(), r.HDROf())
		if scope.Manual {
			allowed = profile.Upgrade(p.Quality, langs, measuredHDR, r.Quality, r.SourceVerified(), r.AudioOf(), r.HDROf()) || (quality.Better(p.Quality, r.Quality) && profile.LanguageAcceptable(langs))
		}
		if !allowed {
			return importPolicyRefusalf("measured %s cannot replace existing %s under current policy", p.Quality.Display(), r.Quality.Display())
		}
		// A claim permitting cleanup must be validated against these same facts.
		if !replace {
			return importLibraryRefusalf("replacement requires measured authorization")
		}
		if f.Path == p.Target {
			targetAuthorized = true
		}
	}
	if !targetAuthorized {
		if !targetTracked {
			return importLibraryRefusalf("existing destination bytes have no measured replacement authorization: the library does not track the file at %s", p.Target)
		}
		return importLibraryRefusalf("existing destination bytes have no measured replacement authorization")
	}
	return nil
}

// authorizeDisbelievedTarget proves, from the bytes rather than the library
// row, that the file at the destination is still the implausible file the
// library disbelieved. The row is only a pointer: the measurement is
// repeated now, bracketed by the file generation the placement recorded, and
// publishPlacement re-proves PreviousSHA256 before anything is renamed. A
// payload that is itself implausible never replaces anything.
func authorizeDisbelievedTarget(item domain.MediaItem, p sqlite.Placement) error {
	if p.Provenance == mediainfo.ProvenanceImplausible {
		return importPolicyRefusalf("measured payload is itself implausible; it cannot replace the disbelieved file at %s", p.Target)
	}
	reviewed := func(info os.FileInfo) bool {
		g := deliveryFileGeneration(info)
		return g != "" && (g == p.TargetFileGeneration || (p.TargetAfterBackupGen != "" && g == p.TargetAfterBackupGen))
	}
	before, err := os.Lstat(p.Target)
	if err != nil || !before.Mode().IsRegular() || !reviewed(before) {
		return importPolicyRefusalf("disbelieved destination changed after placement preview: %s", p.Target)
	}
	info, _ := probe.File(p.Target)
	after, err := os.Lstat(p.Target)
	if err != nil || deliveryFileGeneration(after) != deliveryFileGeneration(before) {
		return importPolicyRefusalf("disbelieved destination changed while it was measured: %s", p.Target)
	}
	if !info.Measured() {
		return importLibraryRefusalf("disbelieved destination could not be measured again: %s", p.Target)
	}
	if _, implausible := mediainfo.Check(info, item.Runtime); !implausible {
		return importLibraryRefusalf("the file at %s is recorded as implausible but now measures plausible; rescan it before replacing", p.Target)
	}
	return nil
}
