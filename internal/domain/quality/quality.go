// Package quality is the shared quality model (blueprint §3: one
// implementation for movies, TV, and books): sources × resolutions, a global
// rank for upgrade ordering, and profiles expressed as a target, an optional
// floor, and an upgrades switch (ADR 0014).
package quality

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/pjunod/monarr/internal/domain/hdr"
	"github.com/pjunod/monarr/internal/domain/language"
)

// Source is where the release came from, worst to best (roughly).
// For books (ADR 0006) the "source" axis carries the file FORMAT instead —
// same model, second vocabulary; Resolution stays 0 for all book formats.
type Source string

// BookType is the format family a book profile curates. It is deliberately
// not a MediaKind: ebooks and audiobooks share metadata, folders, and the
// standalone book wantable, while search categories and file handling need
// to know which edition the selected profile is asking for.
type BookType string

const (
	BookTypeEbook     BookType = "ebook"
	BookTypeAudiobook BookType = "audiobook"
)

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
	// Audiobook formats share a family; their total ordering keeps profile
	// upgrades deterministic while the profile itself remains the authority.
	SourcePDF  Source = "pdf"
	SourceMOBI Source = "mobi"
	SourceAZW3 Source = "azw3"
	SourceEPUB Source = "epub"
	SourceCBZ  Source = "cbz"
	SourceCBR  Source = "cbr"
	SourceDJVU Source = "djvu"
	SourceMP3  Source = "mp3"
	SourceWMA  Source = "wma"
	SourceAAC  Source = "aac"
	SourceOGG  Source = "ogg"
	SourceOPUS Source = "opus"
	SourceM4A  Source = "m4a"
	SourceM4B  Source = "m4b"
	SourceFLAC Source = "flac"
	SourceWAV  Source = "wav"
)

// IsBookFormat reports whether s is one of the book-format sources.
func IsBookFormat(s Source) bool {
	return IsEbookFormat(s) || IsAudiobookFormat(s)
}

// IsEbookFormat reports whether s is a written-book format.
func IsEbookFormat(s Source) bool {
	switch s {
	case SourcePDF, SourceMOBI, SourceAZW3, SourceEPUB,
		SourceCBZ, SourceCBR, SourceDJVU:
		return true
	}
	return false
}

// IsAudiobookFormat reports whether s is a narrated-book format.
func IsAudiobookFormat(s Source) bool {
	switch s {
	case SourceMP3, SourceWMA, SourceAAC, SourceOGG, SourceOPUS,
		SourceM4A, SourceM4B, SourceFLAC, SourceWAV:
		return true
	}
	return false
}

// BookTypeForSource returns the book family a format belongs to. The empty
// value means the source is video or unknown.
func BookTypeForSource(s Source) BookType {
	switch {
	case IsEbookFormat(s):
		return BookTypeEbook
	case IsAudiobookFormat(s):
		return BookTypeAudiobook
	default:
		return ""
	}
}

// ValidBookType reports whether t is an API-selectable book family.
func ValidBookType(t BookType) bool {
	return t == BookTypeEbook || t == BookTypeAudiobook
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
		SourceEPUB: "EPUB", SourceMP3: "MP3", SourceWMA: "WMA",
		SourceCBZ: "CBZ", SourceCBR: "CBR", SourceDJVU: "DjVu",
		SourceAAC: "AAC", SourceOGG: "OGG", SourceOPUS: "Opus",
		SourceM4A: "M4A", SourceM4B: "M4B", SourceFLAC: "FLAC",
		SourceWAV: "WAV",
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
	// Newly supported native containers tie the top existing ebook rank so
	// enabling them does not reorder any established format preference.
	SourceCBZ: 4, SourceCBR: 4, SourceDJVU: 4,
	SourceMP3: 1, SourceWMA: 2, SourceAAC: 3, SourceOGG: 4,
	SourceOPUS: 5, SourceM4A: 6, SourceM4B: 7, SourceFLAC: 8, SourceWAV: 9,
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
	// DownloadPriority is passed to a capable download client when this
	// profile supplies the item or copy's effective priority.
	DownloadPriority int
	// Languages is the audio-language requirement (ADR 0022): canonical
	// codes, any one of which a file must carry on an audio track. Empty
	// means no opinion, which is every profile that existed before the
	// rule did.
	//
	// It is a requirement, not a preference, and it is not a rank. A file
	// without any of these languages does not satisfy the profile whatever
	// its resolution says — the German-only WEB-DL 1080p of an English film
	// is not "at the target", it is the wrong film for this library — and
	// a release that offers the language is an upgrade over it at any
	// acceptable quality.
	Languages []string
	// HDROrder is the preferred HDR presentation, best first (ADR 0026 §4).
	// Empty means no opinion, which is every profile that existed before it.
	// It breaks ties after resolution and source: a 2160p SDR release still
	// outranks a 1080p Dolby Vision one under a 2160p target.
	HDROrder []hdr.Tier
	// HDRReject lists tiers this profile never takes, such as Dolby Vision
	// profile 5 for a library played on devices without Dolby Vision.
	HDRReject []hdr.Tier
	// Preflight says how a release's HDR is measured before it is grabbed
	// (ADR 0026 §6). It applies whenever the profile has an HDR order or
	// refusal list — the only time a measurement can change a decision.
	Preflight Preflight
}

// Preflight is the pre-flight probe policy of a profile.
type Preflight struct {
	// Top is how many of the best-ranked releases to probe; 0 means the
	// default, ProbeAll every release the profile would take.
	Top int
	// Bytes is how much of each release's head to read; 0 means the default.
	Bytes int64
	// SkipUnreadable refuses a release whose head could not be read
	// (encrypted, compressed, missing articles) instead of ranking it by its
	// name below every measured one.
	SkipUnreadable bool
}

// Pre-flight defaults: five releases, 4 MiB each — the container headers and
// the first frame, where the Dolby Vision RPU lives.
const (
	DefaultProbeTop   = 5
	DefaultProbeBytes = 4 << 20
	ProbeAll          = 1000
	MaxProbeBytes     = 16 << 20
)

// Probes reports whether pre-flight probing applies to this profile, and how
// many releases and bytes each.
func (p Profile) Probes() (top int, bytes int64, ok bool) {
	if len(p.HDROrder) == 0 && len(p.HDRReject) == 0 {
		return 0, 0, false
	}
	top, bytes = p.Preflight.Top, p.Preflight.Bytes
	if top <= 0 {
		top = DefaultProbeTop
	}
	if bytes <= 0 {
		bytes = DefaultProbeBytes
	}
	if bytes > MaxProbeBytes {
		bytes = MaxProbeBytes
	}
	return top, bytes, true
}

// HDRRank is a tier's place in the HDR order: 0 is best, and a tier the order
// does not list ranks below every listed one. A partly-known tier ranks as the
// worst of what it could be (hdr.Tier.Possible), so not knowing whether a
// profile 7 release is FEL never ranks it with FEL. With no order, every tier
// ranks alike.
func (p Profile) HDRRank(t hdr.Tier) int {
	if len(p.HDROrder) == 0 {
		return 0
	}
	unlisted := len(p.HDROrder)
	worst := -1
	for _, cand := range t.Possible() {
		r := unlisted
		for i, x := range p.HDROrder {
			if x == cand {
				r = i
				break
			}
		}
		if r > worst {
			worst = r
		}
	}
	if worst < 0 {
		return unlisted
	}
	return worst
}

// HDRAcceptable reports whether a release's HDR is one the profile would take.
// Only sure evidence (measured, or stated outright in the name) refuses: an
// inference such as "DV on a web release is probably profile 5" ranks low but
// never rejects, because the pre-flight probe or the import can prove it
// wrong (ADR 0013 §5).
func (p Profile) HDRAcceptable(e hdr.Evidence) bool {
	if len(p.HDRReject) == 0 || e.Tier == hdr.TierUnknown {
		return true
	}
	possible := e.Tier.Possible()
	if !e.Sure() && len(possible) == 1 {
		return true
	}
	for _, cand := range possible {
		if !slices.Contains(p.HDRReject, cand) {
			return true
		}
	}
	return false
}

// HDRMet reports whether what is on disk is at the top of the HDR order. A
// tier that was never measured counts as met: the don't-churn rule on a new
// axis.
func (p Profile) HDRMet(current hdr.Tier) bool {
	if len(p.HDROrder) == 0 || current == hdr.TierUnknown {
		return true
	}
	return p.HDRRank(current) == 0
}

// Audio is what is known about the audio languages of what is on disk.
//
// Known carries the don't-churn rule (ADR 0013 §5) onto this axis. A track
// with no language tag is not evidence of anything: it may well be the
// English track. Only when every audio track declares a language, and none
// of them is one the profile wants, has the file been shown to lack it.
// Until then the requirement counts as met, and the item page says the
// language could not be determined rather than hunting a replacement for a
// file that might be perfect.
type Audio struct {
	// Languages are the canonical codes of the declared audio tracks.
	Languages []string
	// Known is true when there is at least one audio track and every one of
	// them declared a language — the absence of a language is then a fact.
	Known bool
}

// LanguageMet reports whether what is on disk satisfies the profile's
// language requirement. No requirement, or no proof of absence, is met.
func (p Profile) LanguageMet(a Audio) bool {
	if len(p.Languages) == 0 || !a.Known {
		return true
	}
	return language.Satisfies(a.Languages, p.Languages)
}

// LanguageAcceptable reports whether a release, judged by the languages its
// name advertises, could satisfy the requirement. A release name with no
// language tag is read as the original language of the work, which the
// parser records as English — the scene convention, and the one Radarr
// uses. Multi ("mul") satisfies anything.
func (p Profile) LanguageAcceptable(offered []string) bool {
	return language.Satisfies(offered, p.Languages)
}

// Satisfied is Met on every axis: the quality target is reached, the
// language requirement is met, and what is on disk is at the top of the HDR
// order (or its HDR was never measured). It is the question the wanted index
// asks.
func (p Profile) Satisfied(current Quality, sourceVerified bool, a Audio, h hdr.Tier) bool {
	return p.Met(current, sourceVerified) && p.LanguageMet(a) && p.HDRMet(h)
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
//
// releaseLangs are the languages the release name advertises, and audio is
// what the file on disk was measured to carry. When the file on disk fails
// the language requirement, any acceptable release that offers the language
// is an upgrade — a lower-ranked release included, because a file in the
// wrong language ranks below every file in the right one. When the language
// is fine (or unknowable), it is the quality rule of ADR 0014 unchanged.
//
// The HDR axis (ADR 0026 §4) comes last and never trades quality away: a
// release at no lower quality rank whose HDR ranks above a measured file below
// the top of the order is an upgrade. releaseHDR and currentHDR may be zero,
// which means unknown and leaves the ADR 0014 rule untouched.
func (p Profile) Upgrade(release Quality, releaseLangs []string, releaseHDR hdr.Evidence,
	current Quality, sourceVerified bool, audio Audio, currentHDR hdr.Tier) bool {
	if !p.Acceptable(release) || !p.LanguageAcceptable(releaseLangs) || !p.HDRAcceptable(releaseHDR) {
		return false
	}
	if !p.LanguageMet(audio) {
		return true
	}
	if Rank(release) > Rank(current) && !p.Met(current, sourceVerified) {
		return true
	}
	return !p.HDRMet(currentHDR) && Rank(release) >= Rank(current) &&
		releaseHDR.Tier != hdr.TierUnknown && p.HDRRank(releaseHDR.Tier) < p.HDRRank(currentHDR)
}

// Sentence renders the profile as the thing it now is — one true sentence,
// server-side, so every client says exactly the same thing about it.
func (p Profile) Sentence() string {
	s := "hunts the best release up to " + p.Target.Display() + ", then stops"
	if p.Floor != nil {
		s += "; never below " + p.Floor.Display()
	}
	if len(p.Languages) > 0 {
		s += "; " + language.DisplayList(p.Languages) + " audio required"
	}
	if len(p.HDROrder) > 0 {
		labels := make([]string, len(p.HDROrder))
		for i, t := range p.HDROrder {
			labels[i] = t.Label()
		}
		s += "; prefers " + strings.Join(labels, " › ")
	}
	if len(p.HDRReject) > 0 {
		labels := make([]string, len(p.HDRReject))
		for i, t := range p.HDRReject {
			labels[i] = t.Label()
		}
		s += "; never " + strings.Join(labels, ", ")
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
	{SourceCBZ, 0}, {SourceCBR, 0}, {SourceDJVU, 0},
	{SourceMP3, 0}, {SourceWMA, 0}, {SourceAAC, 0}, {SourceOGG, 0},
	{SourceOPUS, 0}, {SourceM4A, 0}, {SourceM4B, 0}, {SourceFLAC, 0},
	{SourceWAV, 0},
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
