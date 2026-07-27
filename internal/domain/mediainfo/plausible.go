package mediainfo

import (
	"fmt"

	"github.com/monarr-media/monarr/internal/domain/quality"
)

// Plausibility asks a different question from source inference. InferSource
// asks "which kind of real media is this"; this file asks whether the
// measurement could describe real media AT ALL.
//
// The distinction is not academic. Every band in inference.go is a lower-bound
// ladder that bottoms out at SourceUnknown, and a shrug is a fine answer to
// "Blu-ray or WEB-DL?". It is a terrible answer to "is this 500 MB file really
// a 96-minute 2160p remux", because a shrug lets the FILENAME win: Resolve
// accepts any claim the measurements do not actively contradict, and nothing
// in Contradicts could look at 725 kbps and object.
//
// So this is the floor under the floor. It does not classify. It reports that
// a measurement is self-refuting, and it is deliberately set far below any
// real encode — a false positive here costs somebody a file they wanted, so
// the thresholds are chosen to make one nearly impossible rather than to
// catch every marginal case. Everything that survives these checks still has
// to get past inference on its merits.
//
// Nothing here deletes anything. An implausible file stays on disk and stays
// visible; what it loses is the right to be believed, and with it the right to
// mark an item satisfied (ADR 0013 §5 cuts both ways — a measurement must not
// evict a good file, and a bad file must not retire a want).

// Implausibility codes. Persisted nowhere: these are recomputed from the
// stored measurement on demand, so tuning a threshold re-grades every file in
// the library without a migration.
const (
	// CodeBitrateFloor: the overall bitrate is below anything that could
	// carry this many pixels, by any codec, at any quality.
	CodeBitrateFloor = "bitrate_below_floor"
	// CodeAudioArithmetic: the declared audio track alone needs more bitrate
	// than the whole file has. The headers describe a file this is not.
	CodeAudioArithmetic = "audio_exceeds_file"
	// CodeDurationShort: the file is far shorter than the runtime the
	// metadata provider gives for this item — a sample, a truncated payload,
	// or the wrong feature entirely.
	CodeDurationShort = "duration_short"
)

// Implausibility is one reason a measurement cannot be believed.
type Implausibility struct {
	Code   string `json:"code"`
	Reason string `json:"reason"`
}

func because(code, format string, args ...any) Implausibility {
	return Implausibility{Code: code, Reason: fmt.Sprintf(format, args...)}
}

// bitrateFloorByTier is the overall bitrate, in kbps, below which no honest
// file of that resolution exists.
//
// These are NOT sourceBands.webLow. That value is a classification boundary —
// the bottom of the streaming band, above which "this is a web rip" is the
// best explanation. These are impossibility thresholds, set roughly a factor
// of three below the lowest real-world encode anyone ships, because the cost
// of the two mistakes is not symmetric: classifying a file wrongly shows a
// slightly odd badge, while calling a real file fake sends monarr hunting for
// a replacement to something the user already has.
//
// For reference, the low end of things that genuinely exist: a 2160p AV1 web
// encode of sparse animated content bottoms out around 4-5 Mbps, and a
// heavily-compressed 1080p x265 encode around 1.5 Mbps. The floors sit well
// underneath both.
var bitrateFloorByTier = map[int]int64{
	Res2160: 1_500,
	Res1080: 600,
	Res720:  300,
	Res480:  150,
}

// audioFloorByCodec is the lowest overall bitrate, in kbps, that a file
// containing this audio codec can possibly have — the codec's own floor, with
// nothing left over for video.
//
// The values are the bottom of each format's real range, not its typical one.
// TrueHD in the wild runs 1.5-5 Mbps; 1,000 is used here because the check
// only needs to be unarguable, not tight.
var audioFloorByCodec = map[string]int64{
	"truehd": 1_000,
	"mlp":    1_000,
	"pcm":    1_000,
	"flac":   400,
	"alac":   400,
	"dts":    700,
	"eac3":   96,
	"ac3":    96,
	"aac":    32,
	"opus":   32,
	"mp3":    32,
	"vorbis": 32,
}

// audioFloor returns the highest per-codec floor among the declared tracks.
//
// The highest rather than the SUM, deliberately. Summing is arithmetically
// more correct — a file really does have to carry all its tracks — but it
// raises the threshold in proportion to how many audio tracks a file declares,
// which turns a legitimate release with eight commentary tracks into a
// suspect. Taking the maximum keeps the check unfalsifiable in the direction
// that matters: whatever else is in there, the file must at minimum afford its
// most expensive track.
func (i Info) audioFloor() (int64, string) {
	var worst int64
	var codec string
	for _, a := range i.Audio {
		if f := audioFloorByCodec[a.Codec]; f > worst {
			worst, codec = f, a.Codec
		}
	}
	return worst, codec
}

// minJudgeableDurationMS is how long a file has to be before its overall
// bitrate means anything.
//
// Overall bitrate is size ÷ duration, and on a very short file the numerator
// is mostly fixed cost: container headers, an index, a single intra-coded
// keyframe that a two-hour feature amortizes over 170,000 frames. Below a
// minute that ratio describes the container, not the content, and judging it
// produces exactly the wrong answer in both directions.
//
// Nothing escapes through this gap. A file short enough to dodge these rules
// is, by construction, a file whose duration is nowhere near a feature's —
// which is what DurationImplausible is for. The two rules cover each other.
const minJudgeableDurationMS = 60_000

// Implausible reports whether a measurement refutes itself, using only the
// measurement — no metadata, no filename, no profile. Returns the reason and
// true when the file cannot be what it measures as.
//
// It returns false for anything it cannot judge. An unmeasured file is not
// implausible, it is unmeasured, and those are handled apart (ProvenanceFailed
// already means "on disk, unreadable"). A file with no duration has no
// bitrate to test, a file under a minute has no meaningful one, and a file
// whose resolution tier is unrecognized has no floor to compare against; all
// of them get the benefit of the doubt.
func Implausible(i Info) (Implausibility, bool) {
	if !i.Measured() || i.BitrateKbps <= 0 || i.DurationMS < minJudgeableDurationMS {
		return Implausibility{}, false
	}

	// 1. The declared audio cannot fit in the file. This is the strongest
	// signal available, because it needs no threshold tuning at all: it is a
	// contradiction between two things the file says about itself, and it is
	// what a fabricated header looks like from the inside.
	if floor, codec := i.audioFloor(); floor > 0 && i.BitrateKbps <= floor {
		return because(CodeAudioArithmetic,
			"declares a %s track, which alone needs at least %s, but the whole file is only %s",
			Display(codec), formatBitrate(floor), formatBitrate(i.BitrateKbps)), true
	}

	// 2. Not enough bitrate to carry the pixels, whatever the codec.
	if floor, ok := bitrateFloorByTier[i.ResolutionTier()]; ok && i.BitrateKbps < floor {
		return because(CodeBitrateFloor,
			"%s at %s is below the %s floor for that resolution — no encoder produces this",
			i.Summary(), formatBitrate(i.BitrateKbps), formatBitrate(floor)), true
	}

	return Implausibility{}, false
}

// durationShortFraction is how much of the expected runtime a file has to
// carry to be taken seriously. Half is generous on purpose: runtime metadata
// describes A cut, not necessarily THIS cut, and a theatrical file measured
// against an extended runtime should not be condemned for it. Nothing
// legitimate is half the length of the film it claims to be.
const durationShortFraction = 0.5

// DurationImplausible compares a measured duration against the runtime the
// metadata provider gives for the item. Separate from Implausible because it
// needs a fact from outside the file, and the app layer is where the two meet.
//
// Only the SHORT direction is checked. A file longer than its stated runtime
// is usually a double episode, a director's cut, or a provider whose runtime
// field is simply wrong — none of which is a reason to distrust the bytes.
// A file half the length of the feature is a sample, a truncated download, or
// a different film.
func DurationImplausible(i Info, expectedRuntimeMin int) (Implausibility, bool) {
	if !i.Measured() || i.DurationMS <= 0 || expectedRuntimeMin <= 0 {
		return Implausibility{}, false
	}
	expectedMS := int64(expectedRuntimeMin) * 60_000
	if float64(i.DurationMS) >= float64(expectedMS)*durationShortFraction {
		return Implausibility{}, false
	}
	return because(CodeDurationShort,
		"runs %s but the feature is %d minutes long",
		formatMinutes(i.DurationMS), expectedRuntimeMin), true
}

// CodeSizeTooSmall is the pre-download twin of the rules above: an advertised
// size that cannot hold what the release name claims. It is a separate code
// because it is reached from a different place and means something slightly
// weaker — nobody has read a byte, this is arithmetic on a number a tracker
// supplied.
const CodeSizeTooSmall = "size_too_small"

// SizeImplausible judges a release before anything is downloaded, using the
// three numbers available at that point: the size a tracker advertises, the
// quality the name claims, and the runtime the metadata provider gives.
//
// The whole ADR 0013 apparatus starts at import, which is correct — a name is
// all there is to judge by beforehand, so the grab decision is rightly made on
// it. But the name is not the ONLY thing available beforehand. A tracker also
// states a size, and size over runtime is a bitrate, and a bitrate can be
// checked against the claim for free. Downloading 500 MB to discover it is not
// a 2160p remux is a round trip nobody needed.
//
// For a season pack the runtime is per-episode while the size covers many, so
// the computed bitrate comes out far too high and the rule simply never fires.
// That is the right failure direction: this can miss, it must not misfire.
func SizeImplausible(claimed quality.Quality, sizeBytes int64, runtimeMin int) (Implausibility, bool) {
	if sizeBytes <= 0 || runtimeMin <= 0 {
		return Implausibility{}, false
	}
	tier := claimed.Resolution
	kbps := sizeBytes * 8 / (int64(runtimeMin) * 60) / 1000
	if kbps <= 0 {
		return Implausibility{}, false
	}

	// A remux is a lossless copy of the disc's own streams. That is not a
	// quality setting, it is a definition, and it puts a hard number under the
	// claim: below the bottom of the ENCODE band the word cannot be true.
	if claimed.Source == quality.SourceRemux {
		if bands, ok := bandsByTier[tier]; ok && kbps < bands.bdLow {
			return because(CodeSizeTooSmall,
				"%s over %d minutes is %s — a %s remux starts around %s",
				humanSize(sizeBytes), runtimeMin, formatBitrate(kbps),
				resolutionName(tier), formatBitrate(bands.bdLow)), true
		}
	}

	if floor, ok := bitrateFloorByTier[tier]; ok && kbps < floor {
		return because(CodeSizeTooSmall,
			"%s over %d minutes is %s, below the %s floor for %s",
			humanSize(sizeBytes), runtimeMin, formatBitrate(kbps),
			formatBitrate(floor), resolutionName(tier)), true
	}
	return Implausibility{}, false
}

func resolutionName(tier int) string {
	if tier <= 0 {
		return "that resolution"
	}
	return fmt.Sprintf("%dp", tier)
}

func humanSize(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%d MB", b/(1<<20))
	default:
		return fmt.Sprintf("%d KB", b/1024)
	}
}

// Check runs every plausibility rule, including the ones needing metadata.
// expectedRuntimeMin may be 0 when the item's runtime is unknown, which skips
// the duration rule rather than failing it.
func Check(i Info, expectedRuntimeMin int) (Implausibility, bool) {
	if bad, ok := Implausible(i); ok {
		return bad, true
	}
	return DurationImplausible(i, expectedRuntimeMin)
}

func formatMinutes(ms int64) string {
	mins := ms / 60_000
	if mins < 1 {
		return fmt.Sprintf("%d seconds", ms/1000)
	}
	return fmt.Sprintf("%d minutes", mins)
}
