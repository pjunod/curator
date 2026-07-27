package acquisition

import (
	"context"
	"path/filepath"
	"time"

	"github.com/monarr-media/monarr/internal/domain/mediainfo"
	"github.com/monarr-media/monarr/internal/domain/quality"
	"github.com/monarr-media/monarr/internal/infra/probe"
)

// HistoryQualityMismatch is logged when a release's claimed quality and the
// file's measured quality disagree on RESOLUTION. Resolution is the one axis
// where the file cannot be right and the name also right, so a disagreement
// there is a fact about the release — mislabelled, or a different cut than
// advertised — worth keeping.
//
// It is recorded and shown; it does not punish. Auto-blocklisting on mismatch
// needs its own design once there is mismatch data to design against (plan
// §9), and an automated punishment loop built on an untested inference is
// exactly the thing ADR 0013 §5 warns about.
const HistoryQualityMismatch = "quality_mismatch"

// HistoryImplausibleFile is logged when a placed file's own measurements
// refute each other, or refute the runtime of the thing it was imported as.
// Unlike a quality mismatch this changes what monarr does: the file stops
// counting toward the item being satisfied, so the search continues.
const HistoryImplausibleFile = "implausible_file"

// recordImportedQuality measures a file that has just been placed in the
// library and stores what it found.
//
// This is where a claim meets the bytes. Up to this point the release name is
// all anyone had — a candidate that has not been downloaded has nothing else
// to judge by — and the grab decision was rightly made on it. Now the file
// exists, and it is what it is: the measurement wins, the claim is demoted to
// a hint that can break a tie the measurement cannot see (ADR 0013 §3).
//
// The import always proceeds. A file that turns out to be worse than promised
// is still a file, and if it now sits below the profile's target the wanted
// index will hunt again — honestly, this time, knowing what is on disk.
func (s *Service) recordImportedQuality(ctx context.Context, itemID, fileID int64,
	dest string, claimed quality.Quality, releaseTitle string, runtimeMin int) quality.Quality {
	info, err := probe.File(dest)
	if !info.Measured() {
		// Unmeasurable: keep the claim, and say that is what we did rather
		// than dressing it up as a measurement.
		s.log.Debug("import: could not measure placed file",
			"file", filepath.Base(dest), "err", err)
		if err := s.db.SetFileMediaInfo(ctx, fileID, info,
			mediainfo.ProvenanceRelease, mediainfo.ConfidenceNone, time.Now()); err != nil {
			s.log.Warn("import: could not record probe attempt", "err", err)
		}
		if err := s.db.SetFileQualityFrom(ctx, fileID, claimed,
			mediainfo.ProvenanceRelease, mediainfo.ConfidenceNone); err != nil {
			s.log.Warn("import: could not record quality", "err", err)
		}
		return claimed
	}

	measured, prov, conf := mediainfo.Resolve(info, claimed, mediainfo.ProvenanceRelease)

	// The length check needs the item's runtime, which Resolve has no way to
	// know. Applied here so a 90-second "movie" is caught at the moment it
	// lands rather than the next time somebody looks at the folder.
	if bad, ok := mediainfo.DurationImplausible(info, runtimeMin); ok {
		measured = quality.Quality{Source: quality.SourceUnknown, Resolution: info.ResolutionTier()}
		prov, conf = mediainfo.ProvenanceImplausible, mediainfo.ConfidenceNone
		s.recordImplausible(ctx, itemID, dest, releaseTitle, info, bad)
	} else if prov == mediainfo.ProvenanceImplausible {
		self, _ := mediainfo.Implausible(info)
		s.recordImplausible(ctx, itemID, dest, releaseTitle, info, self)
	}

	if err := s.db.SetFileMediaInfo(ctx, fileID, info, prov, conf, time.Now()); err != nil {
		s.log.Warn("import: could not record media info", "err", err)
	}
	if err := s.db.SetFileQualityFrom(ctx, fileID, measured, prov, conf); err != nil {
		s.log.Warn("import: could not record quality", "err", err)
	}

	if claimed.Resolution != 0 && measured.Resolution != 0 &&
		claimed.Resolution != measured.Resolution {
		s.log.Warn("import: release did not deliver the resolution it claimed",
			"release", releaseTitle, "claimed", claimed.Display(),
			"measured", measured.Display(), "file", filepath.Base(dest))
		_ = s.db.AddHistory(ctx, HistoryQualityMismatch, itemID, releaseTitle, map[string]any{
			"claimed":  claimed.String(),
			"measured": measured.String(),
			"file":     filepath.Base(dest),
			"facts":    info.Summary(),
		})
	}
	return measured
}

// recordImplausible logs and records a file monarr has decided not to believe.
//
// It gets its own history entry rather than reusing quality_mismatch because
// the two say different things and lead to different actions. A mismatch means
// the release lied about which quality it was; the file is real and you have
// it. This means the file is not plausibly the thing at all — the item goes
// back to being hunted, and the entry is the only record of why a movie that
// looked finished last week is being searched for again.
//
// It does not blocklist. Auto-punishment on an inference is what ADR 0013 §5
// warns against, and these thresholds want real-library mileage before they
// are allowed to act on their own. The user gets a button instead.
func (s *Service) recordImplausible(ctx context.Context, itemID int64, dest, releaseTitle string,
	info mediainfo.Info, why mediainfo.Implausibility) {
	s.log.Warn("import: measurement does not add up; not trusting this file",
		"release", releaseTitle, "file", filepath.Base(dest),
		"why", why.Reason, "facts", info.Summary())
	_ = s.db.AddHistory(ctx, HistoryImplausibleFile, itemID, releaseTitle, map[string]any{
		"code":   why.Code,
		"reason": why.Reason,
		"file":   filepath.Base(dest),
		"facts":  info.Summary(),
	})
}
