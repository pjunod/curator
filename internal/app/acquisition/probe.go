package acquisition

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/pjunod/monarr/internal/domain/language"
	"github.com/pjunod/monarr/internal/domain/mediainfo"
	"github.com/pjunod/monarr/internal/domain/quality"
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

// HistoryLanguageMismatch is logged when a placed file was measured to carry
// none of the languages its profile requires (ADR 0022 §4). Unlike a quality
// mismatch this one DOES blocklist the release, because it is not an
// inference: every audio track declared its language and none of them is
// wanted. Without it the item is wanted again the moment the import lands,
// the same release is the best candidate again, and the loop is bounded by
// nothing — a title whose releases are all untagged and all in the wrong
// language would re-download on every scheduled search forever.
const HistoryLanguageMismatch = "language_mismatch"

// recordLanguageMismatch blocklists a release whose file, once measured,
// carries none of the profile's required languages, and says so in history.
// Only a KNOWN absence counts (every track tagged, none wanted); an untagged
// track is no evidence, and the file simply keeps the item from being
// hunted, per §3.
func (s *Service) recordLanguageMismatch(ctx context.Context, itemID int64, dest, releaseTitle, indexer string,
	profile quality.Profile, info mediainfo.Info) bool {
	if len(profile.Languages) == 0 || releaseTitle == "" {
		return false
	}
	langs, known := info.AudioLanguages()
	if !known || language.Satisfies(langs, profile.Languages) {
		return false
	}
	reason := fmt.Sprintf("no %s audio: the file carries %s only",
		language.DisplayList(profile.Languages), language.DisplayList(langs))
	s.log.Warn("import: file does not carry a required audio language; blocklisting the release",
		"release", releaseTitle, "file", filepath.Base(dest), "why", reason, "facts", info.Summary())
	_ = s.db.AddHistory(ctx, HistoryLanguageMismatch, itemID, releaseTitle, map[string]any{
		"reason": reason,
		"file":   filepath.Base(dest),
		"facts":  info.Summary(),
	})
	// Only a release that came from an indexer can be blocklisted; a manual
	// import's "release" is the folder somebody pointed at.
	if indexer == "" {
		return true
	}
	if err := s.db.AddBlocklist(ctx, itemID, releaseTitle, indexer, reason); err != nil {
		s.log.Warn("import: could not blocklist the release", "release", releaseTitle, "err", err)
	}
	return true
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
