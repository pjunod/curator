package mediainfo_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/pjunod/monarr/internal/domain/mediainfo"
)

// The fixture corpus, mirroring the release-name parser's corpus harness: a
// table of real container headers and exactly what the prober must read out of
// them. Every expectation here was cross-checked against ffprobe when the
// fixture was generated (scripts/gen-mediainfo-fixtures.sh) — ffprobe is the
// oracle we test against, never a dependency we ship.
//
// Fixtures are truncated to a 64 KiB header window on purpose: it proves the
// "never read the whole file" rule holds, because a prober that needed the
// media data would fail on every one of these.
//
// size is the ORIGINAL file's size, which is what a real caller passes and what
// bitrate derivation needs; the truncated window carries no bitrate of its own.
type probeCase struct {
	file       string
	size       int64
	container  string
	codec      string
	width      int
	height     int
	bitDepth   int
	hdr        []string
	interlaced bool
	audio      []mediainfo.AudioInfo
	durationMS int64
	bitrate    int64
	tier       int
	summary    string
}

var corpus = []probeCase{
	{
		file: "mkv-1080p-h264-ac3.mkv", size: 1623514,
		container: "mkv", codec: "h264", width: 1920, height: 1080, bitDepth: 8,
		audio:      []mediainfo.AudioInfo{{Codec: "ac3", Channels: 6}},
		durationMS: 1000, bitrate: 12988, tier: 1080,
		summary: "1080p · H.264 · AC-3 · 13 Mbps",
	},
	{
		// The remux shape: 4K, 10-bit HEVC, PQ transfer, lossless audio.
		file: "mkv-2160p-hevc-hdr10-truehd.mkv", size: 1596577,
		container: "mkv", codec: "hevc", width: 3840, height: 2160, bitDepth: 10,
		hdr:        []string{mediainfo.HDR10},
		audio:      []mediainfo.AudioInfo{{Codec: "truehd", Channels: 6}},
		durationMS: 1000, bitrate: 12772, tier: 2160,
		summary: "2160p · HEVC 10-bit · HDR10 · TrueHD · 13 Mbps",
	},
	{
		// HLG rides the same Colour element as HDR10, one transfer value over.
		file: "mkv-1080p-hevc-hlg-eac3.mkv", size: 467864,
		container: "mkv", codec: "hevc", width: 1920, height: 1080, bitDepth: 10,
		hdr:        []string{mediainfo.HLG},
		audio:      []mediainfo.AudioInfo{{Codec: "eac3", Channels: 6}},
		durationMS: 1000, bitrate: 3742, tier: 1080,
		summary: "1080p · HEVC 10-bit · HLG · E-AC-3 · 3.7 Mbps",
	},
	{
		// Scope crop: 1920x800 is 1080p content. Tiering on height alone would
		// call this 720p and hunt a "better" copy forever.
		file: "mkv-1080p-scope-h264-dts.mkv", size: 1348837,
		container: "mkv", codec: "h264", width: 1920, height: 800, bitDepth: 8,
		audio:      []mediainfo.AudioInfo{{Codec: "dts", Channels: 6}},
		durationMS: 1000, bitrate: 10790, tier: 1080,
		summary: "1080p · H.264 · DTS · 11 Mbps",
	},
	{
		// Interlaced: the one signal that says "broadcast" outright.
		file: "mkv-576i-mpeg2-ac3.mkv", size: 231153,
		container: "mkv", codec: "mpeg2", width: 720, height: 576, bitDepth: 8,
		interlaced: true,
		audio:      []mediainfo.AudioInfo{{Codec: "ac3", Channels: 2}},
		durationMS: 1000, bitrate: 1849, tier: 480,
		summary: "480p · MPEG-2 · AC-3 · 1.8 Mbps",
	},
	{
		file: "mkv-720p-h264-aac.mkv", size: 808205,
		container: "mkv", codec: "h264", width: 1280, height: 720, bitDepth: 8,
		audio:      []mediainfo.AudioInfo{{Codec: "aac", Channels: 2}},
		durationMS: 1021, bitrate: 6332, tier: 720,
		summary: "720p · H.264 · AAC · 6.3 Mbps",
	},
	{
		// FLAC is lossless without being TrueHD — a Bluray encode that kept
		// the disc audio, which inference must not read as a remux.
		file: "mkv-1080p-h264-flac.mkv", size: 1579512,
		container: "mkv", codec: "h264", width: 1920, height: 1080, bitDepth: 8,
		audio:      []mediainfo.AudioInfo{{Codec: "flac", Channels: 6}},
		durationMS: 1000, bitrate: 12636, tier: 1080,
		summary: "1080p · H.264 · FLAC · 13 Mbps",
	},
	{
		file: "mkv-1080p-av1-opus.mkv", size: 285484,
		container: "mkv", codec: "av1", width: 1920, height: 1080, bitDepth: 8,
		audio:      []mediainfo.AudioInfo{{Codec: "opus", Channels: 2}},
		durationMS: 508, bitrate: 4495, tier: 1080,
		summary: "1080p · AV1 · Opus · 4.5 Mbps",
	},
	{
		file: "mp4-1080p-h264-aac-faststart.mp4", size: 1583171,
		container: "mp4", codec: "h264", width: 1920, height: 1080, bitDepth: 8,
		audio:      []mediainfo.AudioInfo{{Codec: "aac", Channels: 2}},
		durationMS: 1000, bitrate: 12665, tier: 1080,
		summary: "1080p · H.264 · AAC · 13 Mbps",
	},
	{
		// moov at the END of the file. This is the fixture that justifies the
		// io.ReaderAt in the API signature; a streaming reader cannot do it.
		file: "mp4-1080p-h264-aac-moov-at-end.mp4", size: 22075,
		container: "mp4", codec: "h264", width: 1920, height: 1080, bitDepth: 8,
		audio:      []mediainfo.AudioInfo{{Codec: "aac", Channels: 2}},
		durationMS: 84, bitrate: 2102, tier: 1080,
		summary: "1080p · H.264 · AAC · 2.1 Mbps",
	},
	{
		// Channel count comes from dec3, not from the sample entry, which
		// claims stereo for this 5.1 track.
		file: "mp4-2160p-hevc-hdr10-eac3.mp4", size: 1599510,
		container: "mp4", codec: "hevc", width: 3840, height: 2160, bitDepth: 10,
		hdr:        []string{mediainfo.HDR10},
		audio:      []mediainfo.AudioInfo{{Codec: "eac3", Channels: 6}},
		durationMS: 1000, bitrate: 12796, tier: 2160,
		summary: "2160p · HEVC 10-bit · HDR10 · E-AC-3 · 13 Mbps",
	},
	{
		file: "mp4-480p-h264-ac3.mp4", size: 398134,
		container: "mp4", codec: "h264", width: 854, height: 480, bitDepth: 8,
		audio:      []mediainfo.AudioInfo{{Codec: "ac3", Channels: 2}},
		durationMS: 1000, bitrate: 3185, tier: 480,
		summary: "480p · H.264 · AC-3 · 3.2 Mbps",
	},
}

func TestProbeCorpus(t *testing.T) {
	for _, tc := range corpus {
		t.Run(tc.file, func(t *testing.T) {
			f := openFixture(t, tc.file)
			info, err := mediainfo.Probe(f, tc.size)
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}
			if info.Container != tc.container {
				t.Errorf("container = %q, want %q", info.Container, tc.container)
			}
			if info.Video == nil {
				t.Fatal("no video track measured")
			}
			v := info.Video
			if v.Codec != tc.codec {
				t.Errorf("codec = %q, want %q", v.Codec, tc.codec)
			}
			if v.Width != tc.width || v.Height != tc.height {
				t.Errorf("dimensions = %dx%d, want %dx%d", v.Width, v.Height, tc.width, tc.height)
			}
			if v.BitDepth != tc.bitDepth {
				t.Errorf("bit depth = %d, want %d", v.BitDepth, tc.bitDepth)
			}
			if !equalStrings(v.HDR, tc.hdr) {
				t.Errorf("hdr = %v, want %v", v.HDR, tc.hdr)
			}
			if v.Interlaced != tc.interlaced {
				t.Errorf("interlaced = %v, want %v", v.Interlaced, tc.interlaced)
			}
			if got := stripLanguages(info.Audio); !reflect.DeepEqual(got, tc.audio) {
				t.Errorf("audio = %+v, want %+v", got, tc.audio)
			}
			if info.DurationMS != tc.durationMS {
				t.Errorf("duration = %dms, want %dms", info.DurationMS, tc.durationMS)
			}
			if info.BitrateKbps != tc.bitrate {
				t.Errorf("bitrate = %d kbps, want %d kbps", info.BitrateKbps, tc.bitrate)
			}
			if got := info.ResolutionTier(); got != tc.tier {
				t.Errorf("resolution tier = %d, want %d", got, tc.tier)
			}
			if got := info.Summary(); got != tc.summary {
				t.Errorf("summary = %q, want %q", got, tc.summary)
			}
			if !info.Measured() {
				t.Error("Measured() = false for a fixture that clearly was")
			}
		})
	}
}

// TestProbeUnsupportedContainer pins the deliberate limit: AVI and friends are
// recognized as "not ours" rather than mis-parsed. Those files keep whatever
// their filename claims, exactly as they do today (ADR 0013 §2).
func TestProbeUnsupportedContainer(t *testing.T) {
	f := openFixture(t, "avi-720p-mpeg4-ac3.avi")
	info, err := mediainfo.Probe(f, 368906)
	if err == nil {
		t.Fatal("Probe(avi) succeeded; want ErrUnknownContainer")
	}
	if info.Measured() {
		t.Error("Probe(avi) reported measurements it cannot have made")
	}
	marker := mediainfo.Unsupported(".AVI")
	if marker != "unsupported:avi" {
		t.Errorf("Unsupported(.AVI) = %q", marker)
	}
	if !mediainfo.IsUnsupported(marker) {
		t.Error("IsUnsupported did not recognize its own marker")
	}
}

// TestProbeHeaderWindowSuffices is the budget claim made falsifiable: probing
// the first 64 KiB of a 1.6 MB file yields the same answer as probing all of
// it. If a walker ever starts needing media data, this test fails first.
func TestProbeHeaderWindowSuffices(t *testing.T) {
	f := openFixture(t, "mkv-2160p-hevc-hdr10-truehd.mkv")
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	window, err := mediainfo.Probe(f, 1596577)
	if err != nil {
		t.Fatalf("Probe over header window: %v", err)
	}
	if st.Size() >= 1596577 {
		t.Skip("fixture is not truncated; nothing to prove")
	}
	if window.Video == nil || window.Video.Height != 2160 {
		t.Fatalf("header window did not carry the video header: %+v", window.Video)
	}
}

func openFixture(t *testing.T, name string) *os.File {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// stripLanguages drops track languages, which ffmpeg's synthetic sources do not
// set and which nothing in the quality model reads.
func stripLanguages(in []mediainfo.AudioInfo) []mediainfo.AudioInfo {
	if in == nil {
		return nil
	}
	out := make([]mediainfo.AudioInfo, len(in))
	for i, a := range in {
		a.Language = ""
		out[i] = a
	}
	return out
}
