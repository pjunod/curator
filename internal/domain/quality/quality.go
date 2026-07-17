// Package quality is the shared quality model (blueprint §3: one
// implementation for movies, TV, and later books): sources × resolutions,
// a global rank for upgrade ordering, and profiles with an allowed set,
// cutoff, and upgrade policy.
package quality

import (
	"fmt"
	"strconv"
	"strings"
)

// Source is where the release came from, worst to best (roughly).
type Source string

// Known sources.
const (
	SourceUnknown  Source = "unknown"
	SourceCAM      Source = "cam"
	SourceTelesync Source = "telesync"
	SourceDVD      Source = "dvd"
	SourceHDTV     Source = "hdtv"
	SourceWEBRip   Source = "webrip"
	SourceWEBDL    Source = "webdl"
	SourceBluray   Source = "bluray"
	SourceRemux    Source = "remux"
)

// Quality is a (source, resolution) pair. Resolution 0 = unknown/SD.
type Quality struct {
	Source     Source
	Resolution int // 480, 720, 1080, 2160; 0 unknown
}

// String renders "webdl-1080" (stable, used for DB storage).
func (q Quality) String() string {
	return string(q.Source) + "-" + strconv.Itoa(q.Resolution)
}

// Display renders a human name like "WEB-DL 1080p".
func (q Quality) Display() string {
	name := map[Source]string{
		SourceUnknown: "Unknown", SourceCAM: "CAM", SourceTelesync: "Telesync",
		SourceDVD: "DVD", SourceHDTV: "HDTV", SourceWEBRip: "WEBRip",
		SourceWEBDL: "WEB-DL", SourceBluray: "Bluray", SourceRemux: "Remux",
	}[q.Source]
	if name == "" {
		name = string(q.Source)
	}
	if q.Resolution > 0 {
		return fmt.Sprintf("%s %dp", name, q.Resolution)
	}
	return name
}

// FromString parses the String() form; unknown input yields the zero value.
func FromString(s string) Quality {
	src, res, ok := strings.Cut(s, "-")
	if !ok {
		return Quality{Source: SourceUnknown}
	}
	r, _ := strconv.Atoi(res)
	return Quality{Source: Source(src), Resolution: r}
}

var sourceRank = map[Source]int{
	SourceUnknown: 0, SourceCAM: 1, SourceTelesync: 2, SourceDVD: 3,
	SourceHDTV: 4, SourceWEBRip: 5, SourceWEBDL: 6, SourceBluray: 7, SourceRemux: 8,
}

var resolutionRank = map[int]int{0: 0, 480: 1, 720: 2, 1080: 3, 2160: 4}

// Rank orders qualities globally: resolution dominates, source breaks ties.
// (A 1080p HDTV beats a 720p Bluray, matching upstream intuition.)
func Rank(q Quality) int {
	return resolutionRank[q.Resolution]*16 + sourceRank[q.Source]
}

// Better reports whether a ranks strictly above b.
func Better(a, b Quality) bool { return Rank(a) > Rank(b) }

// Profile governs what to grab and when to upgrade (blueprint §4.2).
// Allowed is ordered worst→best; Cutoff is the "good enough" point.
type Profile struct {
	ID              int64
	Name            string
	Allowed         []Quality
	Cutoff          Quality
	UpgradesAllowed bool
}

// IsAllowed reports whether q is in the profile's allowed set. Unknown-source
// qualities match an allowed entry on resolution alone (scanned files often
// lack source info).
func (p Profile) IsAllowed(q Quality) bool {
	for _, a := range p.Allowed {
		if a == q {
			return true
		}
		if q.Source == SourceUnknown && q.Resolution != 0 && a.Resolution == q.Resolution {
			return true
		}
	}
	return false
}

// MeetsCutoff reports whether q is at or above the cutoff.
func (p Profile) MeetsCutoff(q Quality) bool { return Rank(q) >= Rank(p.Cutoff) }

// DefaultProfiles are seeded at migration time; IDs are stable.
func DefaultProfiles() []Profile {
	any := []Quality{
		{SourceHDTV, 480}, {SourceDVD, 480}, {SourceHDTV, 720}, {SourceWEBRip, 720},
		{SourceWEBDL, 720}, {SourceBluray, 720}, {SourceHDTV, 1080}, {SourceWEBRip, 1080},
		{SourceWEBDL, 1080}, {SourceBluray, 1080}, {SourceRemux, 1080},
		{SourceWEBDL, 2160}, {SourceBluray, 2160}, {SourceRemux, 2160},
	}
	hd := []Quality{
		{SourceHDTV, 1080}, {SourceWEBRip, 1080}, {SourceWEBDL, 1080},
		{SourceBluray, 1080}, {SourceRemux, 1080},
	}
	uhd := []Quality{
		{SourceWEBDL, 2160}, {SourceBluray, 2160}, {SourceRemux, 2160},
	}
	return []Profile{
		{ID: 1, Name: "Any", Allowed: any, Cutoff: Quality{SourceWEBDL, 1080}, UpgradesAllowed: true},
		{ID: 2, Name: "HD-1080p", Allowed: hd, Cutoff: Quality{SourceWEBDL, 1080}, UpgradesAllowed: true},
		{ID: 3, Name: "Ultra-HD", Allowed: uhd, Cutoff: Quality{SourceWEBDL, 2160}, UpgradesAllowed: true},
	}
}
