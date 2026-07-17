// Package decision is the pure "grab this or not" engine (blueprint §4.3).
// Every rejection carries a machine-readable code surfaced in the UI —
// upstream's interactive-search rejection notes, kept structural.
package decision

import (
	"fmt"

	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/domain/quality"
)

// Rejection codes.
const (
	CodeUnmonitored     = "unmonitored"
	CodeQualityUnknown  = "quality_unknown"
	CodeQualityNotAllow = "quality_not_allowed"
	CodeNotAnUpgrade    = "not_an_upgrade"
	CodeAtCutoff        = "already_at_cutoff"
	CodeUpgradesOff     = "upgrades_disabled"
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

// Decide evaluates a release quality against a wantable's profile and what
// is already on disk. Pure: no I/O, no clock.
func Decide(q quality.Quality, w domain.Wantable, profile quality.Profile) Decision {
	if !w.Monitored() {
		return rejected(CodeUnmonitored, "not monitored")
	}
	if (q.Source == quality.SourceUnknown || q.Source == "") && q.Resolution == 0 {
		return rejected(CodeQualityUnknown, "could not determine quality from the release name")
	}
	if !profile.IsAllowed(q) {
		return rejected(CodeQualityNotAllow, "%s is not allowed by profile %q", q.Display(), profile.Name)
	}

	current, have := w.CurrentQuality()
	if !have {
		return Decision{Accepted: true}
	}

	// Something is on disk: this is upgrade territory.
	if !profile.UpgradesAllowed {
		return rejected(CodeUpgradesOff, "already have %s and upgrades are disabled", current.Display())
	}
	if profile.MeetsCutoff(current) {
		return rejected(CodeAtCutoff, "already at cutoff with %s", current.Display())
	}
	if !quality.Better(q, current) {
		return rejected(CodeNotAnUpgrade, "%s does not improve on %s", q.Display(), current.Display())
	}
	return Decision{Accepted: true, IsUpgrade: true}
}
