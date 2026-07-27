package mediainfo

import (
	"strings"

	"github.com/monarr-media/monarr/internal/domain/quality"
)

// Source inference: measurements in, a verdict and a confidence out.
//
// Resolution is measured. Source is not — no byte in an MKV says "this came
// off a Blu-ray" — so it has to be reasoned about from the things that ARE
// measurable: whether the audio is lossless (streaming services do not ship
// TrueHD), how much bitrate the file spends per pixel, whether the video is
// interlaced (only broadcast is), and what muxer signed it.
//
// The verdict is always accompanied by a confidence, and the confidence is
// load-bearing rather than decorative: a medium or low verdict is shown to the
// user but never permitted to trigger a replacement (ADR 0013 §5). Being wrong
// here should cost a slightly odd badge, never somebody's 17 GB file.

// sourceBands are the overall-bitrate thresholds per resolution tier, in kbps.
// Starting values from plan §4.3, expected to be tuned against real libraries —
// which is what the confidence levels buy us room to do safely.
type sourceBands struct {
	remuxFloor int64 // at or above this, only a remux plausibly explains it
	bdLow      int64 // a Blu-ray encode lives between bdLow and remuxFloor
	webLow     int64 // a streaming rip lives between webLow and webHigh
	webHigh    int64
}

var bandsByTier = map[int]sourceBands{
	Res2160: {remuxFloor: 40_000, bdLow: 15_000, webLow: 4_000, webHigh: 30_000},
	Res1080: {remuxFloor: 20_000, bdLow: 6_000, webLow: 2_000, webHigh: 12_000},
	Res720:  {remuxFloor: 10_000, bdLow: 3_000, webLow: 1_000, webHigh: 6_000},
	Res480:  {remuxFloor: 8_000, bdLow: 2_000, webLow: 500, webHigh: 4_000},
}

// muxerSignatures are writing-app strings that identify a tool whose whole
// purpose is lossless disc extraction. If MakeMKV wrote it, it is a remux.
var muxerSignatures = []string{"makemkv"}

// InferSource returns the most defensible source verdict for a measured file,
// and how much to trust it. It returns SourceUnknown when the measurements do
// not support any verdict — shrugging is a valid answer and a much better one
// than a confident guess.
//
// Rules are evaluated in order, first match wins (plan §4.3).
func InferSource(info Info) (quality.Source, Confidence) {
	if !info.Measured() {
		return quality.SourceUnknown, ConfidenceNone
	}
	bands, ok := bandsByTier[info.ResolutionTier()]
	if !ok {
		return quality.SourceUnknown, ConfidenceNone
	}
	bitrate := info.BitrateKbps
	lossless := info.HasLosslessAudio()

	// 1. Interlacing is the one signal with no alternative explanation.
	// Nothing but a broadcast source is interlaced in 2026.
	if info.Video.Interlaced {
		return quality.SourceHDTV, ConfidenceHigh
	}

	// 2. Lossless audio at disc bitrate: a remux, and nothing else looks like
	// this. Streaming has never shipped TrueHD or a PCM track.
	if lossless && bitrate >= bands.remuxFloor {
		return quality.SourceRemux, ConfidenceHigh
	}

	// 3. Lossless audio at a bitrate a remux could not be: an encode that kept
	// the disc's audio track. Common, and specifically NOT a remux.
	if lossless {
		return quality.SourceBluray, ConfidenceMedium
	}

	// 4. The muxer signed its work.
	if hasMuxerSignature(info.WritingApp) {
		return quality.SourceRemux, ConfidenceHigh
	}

	// 5. Disc-level bitrate with no lossless track. Probably a remux whose
	// audio was swapped, but the strongest evidence is missing, so: medium.
	if bitrate >= bands.remuxFloor {
		return quality.SourceRemux, ConfidenceMedium
	}

	// 6. Web-shaped audio at web-shaped bitrate. E-AC-3 or Opus is a positive
	// signal; AC-3 alone is merely the absence of a contrary one, so it earns
	// the same verdict at lower confidence.
	if info.AudioAllWebOrNeutral() && bitrate >= bands.webLow && bitrate <= bands.webHigh {
		if info.HasWebAudio() {
			return quality.SourceWEBDL, ConfidenceMedium
		}
		return quality.SourceWEBDL, ConfidenceLow
	}

	// 7. Bitrate in encode territory, nothing else to go on.
	if bitrate >= bands.bdLow && bitrate < bands.remuxFloor {
		return quality.SourceBluray, ConfidenceLow
	}

	// 8. Say so.
	return quality.SourceUnknown, ConfidenceNone
}

func hasMuxerSignature(app string) bool {
	if app == "" {
		return false
	}
	lower := strings.ToLower(app)
	for _, sig := range muxerSignatures {
		if strings.Contains(lower, sig) {
			return true
		}
	}
	return false
}

// Contradicts reports whether the measurements actively disagree with a source
// a filename claimed. This is the demotion of names to hints made concrete: a
// token is accepted unless the bytes say it cannot be true.
//
// Only two claims are checkable this way, and both are checkable because they
// make a promise about the audio:
//
//   - "remux" promises the disc's own streams. No lossless track and a bitrate
//     an encode could produce means the word was decoration.
//   - "webdl"/"webrip" promise a streaming source, which has never shipped a
//     lossless audio track. One present means the name is wrong.
//
// Everything else (bluray vs hdtv vs dvd) has no measurable tell, so the token
// stands — being unable to disprove a claim is not grounds for overriding it.
func Contradicts(info Info, token quality.Source) bool {
	if !info.Measured() || token == quality.SourceUnknown || token == "" {
		return false
	}
	bands, ok := bandsByTier[info.ResolutionTier()]
	if !ok {
		return false
	}
	lossless := info.HasLosslessAudio()
	switch token {
	case quality.SourceRemux:
		// Below the bottom of the ENCODE band a remux is impossible whatever
		// the audio headers say — and the audio headers are exactly what a
		// forged file gets right. A declared track is not a delivered track:
		// monarr reads a codec ID out of the container, and writing "TrueHD"
		// there costs nothing.
		//
		// This clause exists because its absence is how a 500 MB file named
		// "HIM (2025) [Remux 2160p].mkv" kept the word Remux. Lossless audio
		// was treated as evidence FOR a remux and nowhere as something that
		// could itself be false, so declaring TrueHD made the claim
		// unfalsifiable — the one property a check must never have.
		if info.BitrateKbps > 0 && info.BitrateKbps < bands.bdLow {
			return true
		}
		// Above that the bands are genuinely fuzzy, so the original rule
		// stands: lossless audio earns the benefit of the doubt between
		// bdLow and remuxFloor.
		return !lossless && info.BitrateKbps < bands.remuxFloor
	case quality.SourceWEBDL, quality.SourceWEBRip:
		return lossless
	}
	return false
}
