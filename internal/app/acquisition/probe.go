package acquisition

import (
	"context"
	"path/filepath"

	"github.com/pjunod/monarr/internal/domain/mediainfo"
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
