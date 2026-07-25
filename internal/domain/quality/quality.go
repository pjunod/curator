// Package quality is the shared quality model (blueprint §3: one
// implementation for movies, TV, and books): sources × resolutions, a global
// rank for upgrade ordering, and profiles expressed as a target, an optional
// floor, and an upgrades switch (ADR 0014).
package quality

import (
	"fmt"
	"strconv"
	"strings"
)

// Source is where the release came from, worst to best (roughly).
// For books (ADR 0006) the "source" axis carries the file FORMAT instead —
// same model, second vocabulary; Resolution stays 0 for all book formats.
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

	// Book formats (Phase 2.5). Ebooks worst→best: PDF < MOBI < AZW3 < EPUB.
	// Audiobooks: MP3 < M4B. The two families never compete in practice —
	// profiles keep them apart — but ranks are total for determinism.
	SourcePDF  Source = "pdf"
	SourceMOBI Source = "mobi"
	SourceAZW3 Source = "azw3"
	SourceEPUB Source = "epub"
	SourceMP3  Source = "mp3"
	SourceM4B  Source = "m4b"
)

// IsBookFormat reports whether s is one of the book-format sources.
func IsBookFormat(s Source) bool {
	return IsEbookFormat(s) || IsAudiobookFormat(s)
}

// IsEbookFormat reports whether s is a written-book format.
func IsEbookFormat(s Source) bool {
	switch s {
	case SourcePDF, SourceMOBI, SourceAZW3, SourceEPUB:
		return true
	}
	return false
}

// IsAudiobookFormat reports whether s is a narrated-book format.
func IsAudiobookFormat(s Source) bool {
	return s == SourceMP3 || s == SourceM4B
}

// IsScreenCapture reports whether s is a recording of a screening rather than
// a copy of the film: a camcorder in a cinema, or a screener tape.
//
// These are not "lower-quality versions" the way a 720p WEB-DL is a lower
// quality version — they are a different artifact, and nobody's profile wants
// one as a stand-in while waiting for the real thing. The old allowed-lists
// excluded them by simply never listing them; a target has to say it.
func IsScreenCapture(s Source) bool {
	return s == SourceCAM || s == SourceTelesync
}

// family groups sources that compete with each other. Two qualities in
// different families are not better or worse than one another — they are
// answers to different questions, and a profile that targets one must never
// accept the other (an M4B does not satisfy a want for an EPUB).
func family(s Source) int {
	switch {
	case IsEbookFormat(s):
		return 1
	case IsAudiobookFormat(s):
		return 2
	default:
		return 0 // video, including the unknown source
	}
}

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
		SourcePDF: "PDF", SourceMOBI: "MOBI", SourceAZW3: "AZW3",
		SourceEPUB: "EPUB", SourceMP3: "MP3", SourceM4B: "M4B",
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
	// Book formats rank at resolution 0; ordering within each family is what
	// matters (profiles keep ebooks and audiobooks apart).
	SourcePDF: 1, SourceMOBI: 2, SourceAZW3: 3, SourceEPUB: 4,
	SourceMP3: 5, SourceM4B: 6,
}

var resolutionRank = map[int]int{0: 0, 480: 1, 720: 2, 1080: 3, 2160: 4}

// Rank orders qualities globally: resolution dominates, source breaks ties.
// (A 1080p HDTV beats a 720p Bluray, matching upstream intuition.)
func Rank(q Quality) int {
	return resolutionRank[q.Resolution]*16 + sourceRank[q.Source]
}

// Better reports whether a ranks strictly above b.
func Better(a, b Quality) bool { return Rank(a) > Rank(b) }

// Profile is what the user wants on disk, in one shape (ADR 0014).
//
// The old model was Radarr's: an ordered allowed LIST plus a CUTOFF, two knobs
// encoding one intent. It produced a default profile named "Any" whose item
// page read "Any — upgrades until WEB-DL 1080p, then stops", a sentence the UI
// carried an apologetic comment to explain. Worse, the cutoff never bounded
// what got GRABBED: a missing item under "Any" could pull an 80 GB 2160p remux
// for a profile that would have declared itself finished at WEB-DL 1080p.
//
// A profile is now a target, an optional floor, and an upgrades switch. Three
// sentences are the whole specification, and they are the sentences the UI
// prints:
//
//	Hunt the best release at or below the target's resolution. While what's
//	on disk is below the target and upgrades are on, keep looking; once the
//	target is met, stop. Never grab below the floor.
type Profile struct {
	ID   int64
	Name string
	// Target is the point of the profile: what "done" looks like.
	Target Quality
	// Floor is optional. Below it, do not grab at all — the difference
	// between "I would rather have something" and "I would rather wait".
	Floor *Quality
	// UpgradesAllowed governs whether an item that already has a file keeps
	// being hunted. It has no bearing on missing items.
	UpgradesAllowed bool
}

// Met reports whether what is on disk satisfies the profile — the point at
// which hunting stops.
//
// A resolution above the target is met (nobody wants their 4K file replaced by
// the 1080p they asked for). At the target resolution, a source at or above
// the target's source is met.
//
// sourceVerified carries the don't-churn rule (ADR 0013 §5): when the measured
// resolution is right and only the SOURCE is a guess, the target counts as met
// anyway. Auto-replacing a possibly-perfect file on an inference monarr itself
// rates as medium confidence is the one unforgivable move for a tool sharing a
// disk with somebody's collection. The user's escape hatch stays open —
// interactive search grabs whatever they pick, with no decision gate.
func (p Profile) Met(current Quality, sourceVerified bool) bool {
	switch {
	case resolutionRank[current.Resolution] > resolutionRank[p.Target.Resolution]:
		return true
	case resolutionRank[current.Resolution] < resolutionRank[p.Target.Resolution]:
		return false
	}
	if !sourceVerified {
		return true
	}
	return sourceRank[current.Source] >= sourceRank[p.Target.Source]
}

// Acceptable reports whether a release is one this profile would ever take.
//
// The resolution cap is the fix for the asymmetry in ADR 0014: a profile
// cannot grab above the resolution it calls "done". Above the target's SOURCE
// at the target resolution is welcome — refusing a 1080p remux under a WEB-DL
// 1080p target helps nobody, and the thing people actually fear is surprise 4K
// downloads, which is a resolution problem.
//
// Release quality still comes from the release name; a candidate that has not
// been downloaded has nothing else to judge by.
func (p Profile) Acceptable(release Quality) bool {
	// Families never compete (ADR 0006): an EPUB is not a low-ranked video
	// release and an M4B is not a better EPUB — they are different axes
	// sharing one field. The old allowed-lists kept them apart by
	// construction; a target has to say so out loud.
	if family(release.Source) != family(p.Target.Source) {
		return false
	}
	// A cinema recording is never a stand-in for the film, at any resolution.
	if IsScreenCapture(release.Source) && !IsScreenCapture(p.Target.Source) {
		return false
	}
	if resolutionRank[release.Resolution] > resolutionRank[p.Target.Resolution] {
		return false
	}
	if p.Floor != nil && Rank(release) < Rank(*p.Floor) {
		return false
	}
	return true
}

// Upgrade reports whether a release is worth replacing what is on disk with.
func (p Profile) Upgrade(release, current Quality, sourceVerified bool) bool {
	return p.Acceptable(release) &&
		Rank(release) > Rank(current) &&
		!p.Met(current, sourceVerified)
}

// Sentence renders the profile as the thing it now is — one true sentence,
// server-side, so every client says exactly the same thing about it.
func (p Profile) Sentence() string {
	s := "hunts the best release up to " + p.Target.Display() + ", then stops"
	if p.Floor != nil {
		s += "; never below " + p.Floor.Display()
	}
	if !p.UpgradesAllowed {
		s += "; no upgrades once a file is present"
	}
	return s
}

// DefaultProfiles are seeded at migration time; IDs are stable because
// media_items, media_copies, and import lists reference them (ADR 0014 §4).
//
// "Any" is retired. A hunting profile with no opinion is not a thing, and what
// "Any" actually did — stop at WEB-DL 1080p — is what the 1080p profile now
// says it does.
func DefaultProfiles() []Profile {
	hd1080 := Quality{SourceHDTV, 1080}
	uhd := Quality{SourceWEBDL, 2160}
	return []Profile{
		{ID: 1, Name: "1080p", Target: Quality{SourceWEBDL, 1080}, UpgradesAllowed: true},
		{ID: 2, Name: "HD-1080p", Target: Quality{SourceWEBDL, 1080}, Floor: &hd1080, UpgradesAllowed: true},
		{ID: 3, Name: "4K", Target: Quality{SourceWEBDL, 2160}, Floor: &uhd, UpgradesAllowed: true},
		{ID: 4, Name: "Ebook", Target: Quality{SourceEPUB, 0}, UpgradesAllowed: true},
		{ID: 5, Name: "Audiobook", Target: Quality{SourceM4B, 0}, UpgradesAllowed: true},
	}
}

// EbookProfileID / AudiobookProfileID are the seeded book profile ids;
// library.Add defaults book items to the ebook profile.
const (
	EbookProfileID     int64 = 4
	AudiobookProfileID int64 = 5
)

// videoVocabulary and bookVocabulary are the full known quality sets, used to
// synthesize a Radarr-shaped allowed list from a target (ADR 0014 §6) and to
// populate profile editors. Rank order is derived, not hard-coded here.
var videoVocabulary = []Quality{
	{SourceDVD, 480}, {SourceHDTV, 480},
	{SourceHDTV, 720}, {SourceWEBRip, 720}, {SourceWEBDL, 720}, {SourceBluray, 720},
	{SourceHDTV, 1080}, {SourceWEBRip, 1080}, {SourceWEBDL, 1080},
	{SourceBluray, 1080}, {SourceRemux, 1080},
	{SourceWEBDL, 2160}, {SourceBluray, 2160}, {SourceRemux, 2160},
}

var bookVocabulary = []Quality{
	{SourcePDF, 0}, {SourceMOBI, 0}, {SourceAZW3, 0}, {SourceEPUB, 0},
	{SourceMP3, 0}, {SourceM4B, 0},
}

// Vocabulary returns every known quality on the axis a target lives on,
// ordered worst to best.
func Vocabulary(target Quality) []Quality {
	if IsBookFormat(target.Source) {
		return append([]Quality(nil), bookVocabulary...)
	}
	return append([]Quality(nil), videoVocabulary...)
}

// AllowedUnder synthesizes the set of qualities this profile would accept,
// worst to best.
//
// Nothing in monarr's own decision path needs this — Acceptable answers the
// question directly. It exists for the compat shim, whose consumers
// (Jellyseerr and friends) expect a Radarr-shaped allowed list plus a cutoff
// and have no concept of a target. Deriving it from Acceptable rather than
// listing it separately is what keeps the translation honest: the shim cannot
// describe a profile that does not behave the way it is described.
func (p Profile) AllowedUnder() []Quality {
	var out []Quality
	for _, q := range Vocabulary(p.Target) {
		if p.Acceptable(q) {
			out = append(out, q)
		}
	}
	return out
}
