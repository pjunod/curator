// Package mediainfo measures what is actually inside a media file, instead of
// believing what its name claims (ADR 0013).
//
// The probe is native Go with no runtime dependency: monarr ships as a single
// static binary in a distroless image, so shelling out to ffprobe is not an
// option we have. MKV (EBML) and MP4/M4V headers carry everything the quality
// model needs — resolution, codec, bit depth, HDR transfer, interlacing, audio
// codecs, duration — and both put it near the front of the file (or, for MP4,
// in a `moov` box that may sit at the very end; that is why the API takes an
// io.ReaderAt rather than an io.Reader).
//
// Two hard rules, both load-bearing over a multi-TB NFS library:
//
//   - Never read the whole file. Every walker runs against a byte budget
//     (8 MiB for MKV, 32 MiB for MP4) and stops at it.
//   - Never panic. This package eats arbitrary bytes off a user's disk; it is
//     fuzzed for exactly that reason. Every read is bounds-checked and every
//     malformed structure degrades to "I could not tell", never to a crash.
//
// Like domain/parser, this is a pure package: stdlib imports only, a committed
// fixture corpus, and a table-driven test that pins every field it extracts.
package mediainfo

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Container markers stored in Info.Container.
const (
	ContainerMKV = "mkv"
	ContainerMP4 = "mp4"
)

// Unsupported renders the Container marker for a file the native prober does
// not deep-parse ("unsupported:avi"). ext may carry a leading dot.
func Unsupported(ext string) string {
	return "unsupported:" + strings.ToLower(strings.TrimPrefix(ext, "."))
}

// IsUnsupported reports whether c is an Unsupported marker.
func IsUnsupported(c string) bool { return strings.HasPrefix(c, "unsupported:") }

// nativeExtensions are the containers the walkers in this package deep-parse.
var nativeExtensions = map[string]bool{
	".mkv": true, ".mka": true, ".mks": true, ".webm": true,
	".mp4": true, ".m4v": true, ".mov": true,
}

// IsNativeContainer reports whether an extension is one monarr parses itself.
//
// This is deliberately keyed on the EXTENSION rather than on whether a probe
// succeeded, because the two answer different questions. An .avi will never
// parse here and re-reading it every scan buys nothing. An .mkv that fails to
// parse is a different animal entirely — truncated, corrupt, still
// downloading, or sitting behind a permission monarr did not have — and every
// one of those is a condition that gets fixed, so it is worth trying again.
//
// Conflating them is how "the probe failed once" became permanent.
func IsNativeContainer(ext string) bool {
	return nativeExtensions[strings.ToLower(ext)]
}

// Errors returned by Probe. All of them come back alongside a partially
// populated Info — callers record what was learned and note the rest as
// unmeasured rather than throwing the whole record away.
var (
	// ErrUnknownContainer means the leading bytes matched neither EBML nor an
	// ISO base-media brand. Callers decorate Container via Unsupported(ext).
	ErrUnknownContainer = errors.New("mediainfo: unrecognized container")
	// ErrMalformed means the container was recognized but its structure did
	// not survive parsing (truncated mid-header, nonsense lengths).
	ErrMalformed = errors.New("mediainfo: malformed container structure")
	// ErrBudget means the walker hit its byte cap before finding the headers.
	ErrBudget = errors.New("mediainfo: header search exceeded byte budget")
)

// VideoInfo is the measured video track (the first one; media files with two
// video tracks are cover-art edge cases and the first is the feature).
type VideoInfo struct {
	Codec      string   `json:"codec"`             // "h264" | "hevc" | "av1" | "mpeg2" | "vc1" | "vp9" | ...
	Width      int      `json:"width"`             // coded width in pixels
	Height     int      `json:"height"`            // coded height in pixels
	BitDepth   int      `json:"bitDepth"`          // 8 unless the codec record says otherwise
	HDR        []string `json:"hdr,omitempty"`     // "hdr10" | "hlg" | "dv", sorted, deduped
	Interlaced bool     `json:"interlaced"`        // container says the content is interlaced
	Profile    string   `json:"profile,omitempty"` // codec profile string when cheap to read
}

// AudioInfo is one measured audio track.
type AudioInfo struct {
	Codec    string `json:"codec"` // "truehd" | "dts" | "eac3" | "ac3" | "aac" | "flac" | "pcm" | "opus" | "mp3" | ...
	Channels int    `json:"channels"`
	Language string `json:"language,omitempty"`
}

// Info is everything a probe measured. A zero Video pointer means no video
// track was found (or the header could not be read that far).
type Info struct {
	Container   string      `json:"container"`
	Video       *VideoInfo  `json:"video,omitempty"`
	Audio       []AudioInfo `json:"audio,omitempty"`
	DurationMS  int64       `json:"durationMs"`
	BitrateKbps int64       `json:"bitrateKbps"` // overall: size*8/duration, not per-track
	WritingApp  string      `json:"writingApp,omitempty"`

	// DeclaredBytes is how long the container says it is, when it says so at
	// all (0 = it did not). A Matroska Segment and an ISO-BMFF box both carry
	// an explicit length, and a muxer writes it knowing the finished size.
	//
	// It is worth storing because it answers a question nothing else can. A
	// file can be far too small for what it claims and be either a fake — a
	// real remux's header stitched onto nothing — or a real download that got
	// cut off. Those look identical from every other angle: same headers, same
	// track list, same chapters, same declared duration, because all of that
	// lives at the front of the file. The difference is that a truncated file
	// still says how long it was SUPPOSED to be, and that number is bigger
	// than the file.
	//
	// One is a bad release to blocklist. The other is a broken transfer to go
	// fix. Telling somebody which one they have is the whole point.
	DeclaredBytes int64 `json:"declaredBytes,omitempty"`
	// SizeBytes is the file's actual length at probe time, kept alongside
	// DeclaredBytes so the comparison survives into storage.
	SizeBytes int64 `json:"sizeBytes,omitempty"`
}

// Truncated reports whether the container declares more bytes than the file
// contains — proof, not inference, that the file is incomplete.
//
// The slack is one EBML/box header's worth. A muxer may finish a hair short of
// its own reservation without anything being wrong, and this claim is strong
// enough that it should only be made when it is not close.
func (i Info) Truncated() bool {
	const slack = 4096
	return i.DeclaredBytes > 0 && i.SizeBytes > 0 && i.DeclaredBytes > i.SizeBytes+slack
}

// MissingBytes is how much of the file never arrived, or 0 when it is whole.
func (i Info) MissingBytes() int64 {
	if !i.Truncated() {
		return 0
	}
	return i.DeclaredBytes - i.SizeBytes
}

// Probe reads container and stream headers from r and returns what it measured.
// size is the file's size on disk and is used only to derive the overall
// bitrate. Probe never reads the whole file.
//
// It returns partial information with a non-nil error rather than failing
// wholesale wherever it can: a file whose video track parsed but whose audio
// list did not is still worth recording.
func Probe(r io.ReaderAt, size int64) (Info, error) {
	if r == nil || size <= 0 {
		return Info{}, ErrUnknownContainer
	}
	head := make([]byte, 16)
	n, err := r.ReadAt(head, 0)
	if n < 8 {
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return Info{}, fmt.Errorf("%w: %v", ErrUnknownContainer, err)
	}
	head = head[:n]

	var info Info
	switch {
	case len(head) >= 4 && head[0] == 0x1A && head[1] == 0x45 && head[2] == 0xDF && head[3] == 0xA3:
		info, err = probeMatroska(r, size)
		info.Container = ContainerMKV
	case len(head) >= 8 && string(head[4:8]) == "ftyp":
		info, err = probeMP4(r, size)
		info.Container = ContainerMP4
	default:
		return Info{}, ErrUnknownContainer
	}

	info.finalize(size)
	return info, err
}

// finalize derives everything that depends on the file as a whole rather than
// on one header field, and normalizes the slices so equal files compare equal.
func (i *Info) finalize(size int64) {
	i.SizeBytes = size
	if i.DurationMS > 0 && size > 0 {
		// Overall bitrate in kbit/s: bytes → bits, ms → s, bit/s → kbit/s.
		i.BitrateKbps = size * 8 / i.DurationMS
	}
	if i.Video != nil {
		if i.Video.BitDepth <= 0 {
			i.Video.BitDepth = 8
		}
		i.Video.HDR = normalizeHDR(i.Video.HDR)
	}
	if len(i.Audio) == 0 {
		i.Audio = nil
	}
	i.WritingApp = strings.TrimSpace(i.WritingApp)
}

// HDR formats, in the order they are reported.
const (
	HDR10 = "hdr10"
	HLG   = "hlg"
	DV    = "dv"
)

func normalizeHDR(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := in[:0]
	for _, h := range in {
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil
	}
	return out
}

// Resolution tiers. These are the values quality.Quality.Resolution uses.
const (
	Res2160 = 2160
	Res1080 = 1080
	Res720  = 720
	Res480  = 480
)

// ResolutionTier maps measured pixel dimensions onto monarr's resolution
// vocabulary (plan §4.2). It tiers on width OR height because scope crops make
// height alone a trap: a 2.39:1 feature is 1920x800 and is 1080p content, and
// a pillarboxed 4K master can be 2880x2160.
//
// When a probe succeeded this tier IS the file's resolution. Filename tokens
// never override it — there is nothing about resolution a name can know that
// the pixels do not.
func (i Info) ResolutionTier() int {
	if i.Video == nil {
		return 0
	}
	w, h := i.Video.Width, i.Video.Height
	switch {
	case w >= 3000 || h >= 1600:
		return Res2160
	case w >= 1700 || h >= 900:
		return Res1080
	case w >= 1150 || h >= 650:
		return Res720
	case h > 0:
		return Res480
	}
	return 0
}

// Measured reports whether the probe learned enough to be worth trusting:
// a video track with real dimensions. Everything downstream keys off this.
func (i Info) Measured() bool {
	return i.Video != nil && i.Video.Width > 0 && i.Video.Height > 0
}

// Audio family membership, used by source inference (plan §4.3) and by nothing
// else. Kept here because it is a fact about codecs, not about policy.
var (
	losslessAudio = map[string]bool{"truehd": true, "flac": true, "pcm": true, "mlp": true, "alac": true}
	discAudio     = map[string]bool{"dts": true}
	webAudio      = map[string]bool{"eac3": true, "aac": true, "opus": true, "vorbis": true}
)

// HasLosslessAudio reports whether any audio track is a lossless format —
// the strongest single signal that a file came off a disc rather than a CDN.
func (i Info) HasLosslessAudio() bool {
	for _, a := range i.Audio {
		if losslessAudio[a.Codec] {
			return true
		}
	}
	return false
}

// HasDiscAudio reports whether any audio track is in the DTS family.
func (i Info) HasDiscAudio() bool {
	for _, a := range i.Audio {
		if discAudio[a.Codec] {
			return true
		}
	}
	return false
}

// AudioAllWebOrNeutral reports whether every audio track is a format a
// streaming service actually ships (or a neutral one that says nothing).
func (i Info) AudioAllWebOrNeutral() bool {
	if len(i.Audio) == 0 {
		return false
	}
	for _, a := range i.Audio {
		if losslessAudio[a.Codec] || discAudio[a.Codec] {
			return false
		}
	}
	return true
}

// HasWebAudio reports whether any track is a positively web-flavored codec
// (as opposed to only neutral ones like AC-3, which prove nothing).
func (i Info) HasWebAudio() bool {
	for _, a := range i.Audio {
		if webAudio[a.Codec] {
			return true
		}
	}
	return false
}

// BestAudio returns the most interesting audio codec for display: the highest
// ranked one present, so a TrueHD+AC3 file reads as TrueHD.
func (i Info) BestAudio() string {
	rank := map[string]int{
		"truehd": 9, "mlp": 9, "pcm": 8, "flac": 8, "alac": 7, "dts": 6,
		"eac3": 5, "ac3": 4, "opus": 3, "aac": 2, "vorbis": 2, "mp3": 1,
	}
	best, bestRank := "", -1
	for _, a := range i.Audio {
		if r := rank[a.Codec]; r > bestRank {
			best, bestRank = a.Codec, r
		}
	}
	return best
}

// displayNames render measured facts the way people write them.
var displayNames = map[string]string{
	"h264": "H.264", "hevc": "HEVC", "av1": "AV1", "vp9": "VP9", "vp8": "VP8",
	"mpeg2": "MPEG-2", "mpeg4": "MPEG-4", "vc1": "VC-1", "theora": "Theora",
	"truehd": "TrueHD", "mlp": "MLP", "dts": "DTS", "eac3": "E-AC-3", "ac3": "AC-3",
	"aac": "AAC", "flac": "FLAC", "pcm": "PCM", "opus": "Opus", "mp3": "MP3",
	"alac": "ALAC", "vorbis": "Vorbis",
	HDR10: "HDR10", HLG: "HLG", DV: "Dolby Vision",
}

// Display renders a name for a measured codec or HDR format ("hevc" → "HEVC").
func Display(token string) string {
	if d, ok := displayNames[token]; ok {
		return d
	}
	return strings.ToUpper(token)
}

// Summary renders the measured facts as the UI pill: "1080p · HEVC · HDR10 ·
// TrueHD · 23 Mbps". Empty when nothing was measured — callers fall back to
// the provenance badge in that case.
func (i Info) Summary() string {
	if !i.Measured() {
		return ""
	}
	parts := []string{fmt.Sprintf("%dp", i.ResolutionTier())}
	if c := i.Video.Codec; c != "" {
		name := Display(c)
		if i.Video.BitDepth > 8 {
			name = fmt.Sprintf("%s %d-bit", name, i.Video.BitDepth)
		}
		parts = append(parts, name)
	}
	for _, h := range i.Video.HDR {
		parts = append(parts, Display(h))
	}
	if a := i.BestAudio(); a != "" {
		parts = append(parts, Display(a))
	}
	if i.BitrateKbps > 0 {
		parts = append(parts, formatBitrate(i.BitrateKbps))
	}
	return strings.Join(parts, " · ")
}

func formatBitrate(kbps int64) string {
	if kbps >= 10_000 {
		return fmt.Sprintf("%d Mbps", (kbps+500)/1000)
	}
	if kbps >= 1_000 {
		return fmt.Sprintf("%.1f Mbps", float64(kbps)/1000)
	}
	return fmt.Sprintf("%d kbps", kbps)
}
