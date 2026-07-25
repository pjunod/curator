// Package decision is the pure "grab this or not" engine (blueprint §4.3).
// Every rejection carries a machine-readable code surfaced in the UI —
// upstream's interactive-search rejection notes, kept structural.
package decision

import (
	"fmt"

	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/domain/quality"
)

// Rejection codes. These surface only in monarr's own UI — the compat shim
// never carried them — so they follow the profile vocabulary of ADR 0014
// rather than the allowed-list one they replaced.
const (
	CodeUnmonitored    = "unmonitored"
	CodeQualityUnknown = "quality_unknown"
	CodeAboveTarget    = "above_target"
	// CodeSourceNotAllowed covers sources a profile refuses outright rather
	// than by rank: a cinema recording under any real target, or a format
	// from the wrong family (an M4B offered against an Ebook profile).
	CodeSourceNotAllowed = "source_not_allowed"
	CodeBelowFloor       = "below_floor"
	CodeNotAnUpgrade     = "not_an_upgrade"
	CodeTargetMet        = "target_met"
	CodeUpgradesOff      = "upgrades_disabled"
	// CodeUnverified is the state ADR 0013 §5 named: files are on disk but
	// their quality could not be measured. Automation declines rather than
	// guess, because guessing here means deleting a file that might be
	// perfect. Interactive search still lists the candidate with this reason
	// attached, and a manual grab is never gated at all.
	CodeUnverified = "on_disk_unverified"
)

// Rejection is one machine-readable reason a release was declined.
type Rejection struct {
	Code   string `json:"code"`
	Reason string `json:"reason"`
}

// Decision is the engine's verdict for one release against one wantable.
type Decision struct {
	Accepted   bool        `json:"accepted"`
	IsUpgrade  bool        `json:"isUpgrade"`
	Rejections []Rejection `json:"rejections"`
}

func rejected(code, format string, args ...any) Decision {
	return Decision{Rejections: []Rejection{{Code: code, Reason: fmt.Sprintf(format, args...)}}}
}

// Decide evaluates a release quality against a wantable's profile and what is
// already on disk. Pure: no I/O, no clock.
//
// The order of the checks is the order of the questions: is this thing wanted
// at all, do we know what the release is, would this profile ever take it,
// and only then — is it better than what is already there.
func Decide(q quality.Quality, w domain.Wantable, profile quality.Profile) Decision {
	if !w.Monitored() {
		return rejected(CodeUnmonitored, "not monitored")
	}
	if (q.Source == quality.SourceUnknown || q.Source == "") && q.Resolution == 0 {
		return rejected(CodeQualityUnknown, "could not determine quality from the release name")
	}
	if !profile.Acceptable(q) {
		switch {
		case quality.IsScreenCapture(q.Source) && !quality.IsScreenCapture(profile.Target.Source):
			return rejected(CodeSourceNotAllowed,
				"%s is not allowed by profile %q: a recording of a screening is not a copy of the film",
				q.Display(), profile.Name)
		case !sameFamily(q, profile.Target):
			return rejected(CodeSourceNotAllowed, "%s is not allowed by profile %q, which targets %s",
				q.Display(), profile.Name, profile.Target.Display())
		case profile.Floor != nil && quality.Rank(q) < quality.Rank(*profile.Floor):
			return rejected(CodeBelowFloor, "%s is below the floor (%s) of profile %q",
				q.Display(), profile.Floor.Display(), profile.Name)
		default:
			return rejected(CodeAboveTarget, "%s is above the target (%s) of profile %q",
				q.Display(), profile.Target.Display(), profile.Name)
		}
	}

	current, have := w.CurrentQuality()
	if !have {
		// Nothing KNOWN on disk. That is two different situations, and
		// conflating them is the bug ADR 0013 exists to fix: a missing item
		// should be hunted, and an item whose files exist but could not be
		// measured must not be — replacing a file we cannot read means
		// deleting something that might already be perfect.
		if w.OnDisk() {
			return rejected(CodeUnverified,
				"files are on disk but their quality could not be determined")
		}
		return Decision{Accepted: true}
	}

	verified := w.SourceVerified()
	if profile.Met(current, verified) {
		if !verified {
			return rejected(CodeTargetMet,
				"already at the target resolution with %s (source unverified)", current.Display())
		}
		return rejected(CodeTargetMet, "target already met with %s", current.Display())
	}
	if !profile.UpgradesAllowed {
		return rejected(CodeUpgradesOff, "already have %s and upgrades are disabled", current.Display())
	}
	if !profile.Upgrade(q, current, verified) {
		return rejected(CodeNotAnUpgrade, "%s does not improve on %s", q.Display(), current.Display())
	}
	return Decision{Accepted: true, IsUpgrade: true}
}

// sameFamily reports whether two qualities are even comparable: video with
// video, ebooks with ebooks, audiobooks with audiobooks.
func sameFamily(a, b quality.Quality) bool {
	if quality.IsEbookFormat(a.Source) != quality.IsEbookFormat(b.Source) {
		return false
	}
	return quality.IsAudiobookFormat(a.Source) == quality.IsAudiobookFormat(b.Source)
}
