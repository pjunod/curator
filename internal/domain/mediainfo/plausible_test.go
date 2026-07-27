package mediainfo_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/monarr-media/monarr/internal/domain/mediainfo"
	"github.com/monarr-media/monarr/internal/domain/quality"
)

// him is the file this whole check exists for: 500 MB of "HIM (2025) [Remux
// 2160p].mkv", a 96-minute feature, declaring a TrueHD track. 500 MB over 96
// minutes is 725 kbps overall — roughly one sixtieth of a real 2160p remux,
// and less than the TrueHD track alone would need.
//
// Everything measured here is what monarr actually recorded for the file. It
// sailed through the library with a REMUX 2160P badge and marked the movie
// satisfied, which is the bug: not that the grade was wrong, but that being
// wrong retired the want.
func him() mediainfo.Info {
	return mediainfo.Info{
		Container:   mediainfo.ContainerMKV,
		Video:       &mediainfo.VideoInfo{Codec: "hevc", Width: 3840, Height: 2160, BitDepth: 10},
		Audio:       []mediainfo.AudioInfo{{Codec: "truehd", Channels: 6}},
		DurationMS:  5_517_000, // 500 MB at 725 kbps
		BitrateKbps: 725,
	}
}

func TestHIMRegression(t *testing.T) {
	info := him()

	bad, ok := mediainfo.Implausible(info)
	if !ok {
		t.Fatalf("725 kbps 2160p with a declared TrueHD track passed as plausible")
	}
	if bad.Code != mediainfo.CodeAudioArithmetic {
		t.Errorf("code = %q, want %q (the audio contradiction is the strongest signal and should win)",
			bad.Code, mediainfo.CodeAudioArithmetic)
	}

	// The filename said Remux. The measurement has to be able to say no.
	if !mediainfo.Contradicts(info, quality.SourceRemux) {
		t.Error("a remux claim survived 725 kbps — declared lossless audio must not make the claim unfalsifiable")
	}

	// And the whole resolution must land on "not believed", not on the name.
	q, prov, conf := mediainfo.Resolve(info, quality.Quality{Source: quality.SourceRemux, Resolution: 2160},
		mediainfo.ProvenanceFilename)
	if prov != mediainfo.ProvenanceImplausible {
		t.Errorf("provenance = %q, want %q", prov, mediainfo.ProvenanceImplausible)
	}
	if q.Source != quality.SourceUnknown {
		t.Errorf("source = %q, want unknown — an implausible file supports no verdict", q.Source)
	}
	if q.Resolution != 2160 {
		t.Errorf("resolution = %d, want 2160 — the one fact worth keeping", q.Resolution)
	}
	if prov.Verified(conf) {
		t.Error("an implausible file must not count as source-verified; that is what keeps the item wanted")
	}
}

func TestImplausible(t *testing.T) {
	cases := []struct {
		name string
		info mediainfo.Info
		want string // "" = plausible
		why  string
	}{
		{
			name: "TrueHD cannot fit in a 725 kbps file",
			info: him(),
			want: mediainfo.CodeAudioArithmetic,
		},
		{
			name: "DTS cannot fit in a 400 kbps file",
			info: vid(1920, 1080, 400, "dts"),
			want: mediainfo.CodeAudioArithmetic,
		},
		{
			name: "2160p at 900 kbps carries no pixels",
			info: vid(3840, 2160, 900, "aac"),
			want: mediainfo.CodeBitrateFloor,
			why:  "below the 1.5 Mbps floor; AAC is cheap enough that arithmetic alone would miss it",
		},
		{
			name: "1080p at 200 kbps carries no pixels",
			info: vid(1920, 1080, 200, "aac"),
			want: mediainfo.CodeBitrateFloor,
		},
		{
			name: "a real 2160p remux",
			info: vid(3840, 2160, 60_000, "truehd", "ac3"),
			why:  "the case the floors must never touch",
		},
		{
			name: "a lean but real 2160p web encode",
			info: vid(3840, 2160, 4_500, "eac3"),
			why:  "sparse animated content genuinely ships this low",
		},
		{
			name: "an aggressively compressed 1080p x265 encode",
			info: vid(1920, 1080, 1_600, "aac"),
			why:  "unpleasant, common, and real — the floor sits under it on purpose",
		},
		{
			name: "a 720p encode at the low end",
			info: vid(1280, 720, 800, "ac3"),
		},
		{
			name: "a 480p DVD rip",
			info: vid(720, 480, 1_200, "ac3"),
		},
		{
			name: "eight commentary tracks do not raise the floor",
			info: vid(1920, 1080, 3_000, "ac3", "ac3", "ac3", "ac3", "ac3", "ac3", "ac3", "ac3"),
			why:  "audioFloor takes the max, not the sum, precisely so this stays plausible",
		},
		{
			name: "an unmeasured file is not implausible, it is unmeasured",
			info: mediainfo.Info{Container: mediainfo.ContainerMKV},
		},
		{
			name: "no duration means no bitrate to judge",
			info: mediainfo.Info{
				Container: mediainfo.ContainerMKV,
				Video:     &mediainfo.VideoInfo{Codec: "hevc", Width: 3840, Height: 2160},
				Audio:     []mediainfo.AudioInfo{{Codec: "truehd"}},
			},
			why: "benefit of the doubt: a header we could not finish reading is not evidence of fraud",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bad, ok := mediainfo.Implausible(tc.info)
			switch {
			case tc.want == "" && ok:
				t.Errorf("wrongly implausible (%s): %s\nwhy this must pass: %s", bad.Code, bad.Reason, tc.why)
			case tc.want != "" && !ok:
				t.Errorf("wrongly plausible, want code %q\nwhy: %s", tc.want, tc.why)
			case tc.want != "" && bad.Code != tc.want:
				t.Errorf("code = %q, want %q (%s)", bad.Code, tc.want, bad.Reason)
			}
		})
	}
}

func TestDurationImplausible(t *testing.T) {
	full := func(ms int64) mediainfo.Info {
		i := vid(1920, 1080, 8_000, "ac3")
		i.DurationMS = ms
		return i
	}

	cases := []struct {
		name    string
		info    mediainfo.Info
		runtime int
		want    bool
		why     string
	}{
		{
			name: "a 90 second sample of a 96 minute film",
			info: full(90_000), runtime: 96, want: true,
		},
		{
			name: "a download truncated at a third",
			info: full(1_920_000), runtime: 96, want: true,
		},
		{
			name: "the feature itself",
			info: full(5_760_000), runtime: 96, want: false,
		},
		{
			name: "a theatrical cut measured against an extended runtime",
			info: full(7_200_000), runtime: 150, want: false,
			why: "80% of stated — runtime metadata describes a cut, not necessarily this one",
		},
		{
			name: "a double episode against a per-episode average",
			info: full(2_640_000), runtime: 22, want: false,
			why: "only the short direction is checked; long files have innocent explanations",
		},
		{
			name: "no runtime known",
			info: full(90_000), runtime: 0, want: false,
			why: "skip the rule rather than fail it",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, got := mediainfo.DurationImplausible(tc.info, tc.runtime)
			if got != tc.want {
				t.Errorf("implausible = %v, want %v\nwhy: %s", got, tc.want, tc.why)
			}
		})
	}
}

// TestTruncationAgainstTheCorpus checks the detector against files whose
// truthful state is known by construction.
//
// The corpus is built by cutting real ffmpeg output down to a 64 KiB header
// window (scripts/gen-mediainfo-fixtures.sh records each original's size in
// SIZES.txt), so every one of those fixtures genuinely IS a truncated file and
// has to be reported as one. mkv-720p-h264-aac-whole.mkv is the control: same
// content, same encoder, never cut.
//
// This is the rule that separates "somebody assembled a fake" from "the
// download stopped early", and those want opposite responses — blocklist the
// release, or go look at the transfer. Getting it backwards sends the user to
// the wrong place, so it is pinned against files whose answer is not a matter
// of opinion.
func TestTruncationAgainstTheCorpus(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"mkv-1080p-h264-ac3.mkv", true},          // 64 KiB of a 1,623,514-byte file
		{"mkv-2160p-hevc-hdr10-truehd.mkv", true}, // 64 KiB of a 1,596,577-byte file
		{"mkv-1080p-av1-opus.mkv", true},          // 64 KiB of a 285,484-byte file
		{"mkv-720p-h264-aac-whole.mkv", false},    // the control: complete
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, err := os.Open(filepath.Join("testdata", c.name))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			st, err := f.Stat()
			if err != nil {
				t.Fatal(err)
			}
			info, _ := mediainfo.Probe(f, st.Size())
			if got := info.Truncated(); got != c.want {
				t.Fatalf("Truncated() = %v, want %v (declares %d bytes, file is %d)",
					got, c.want, info.DeclaredBytes, info.SizeBytes)
			}
			bad, implausible := mediainfo.Implausible(info)
			if c.want {
				if !implausible || bad.Code != mediainfo.CodeTruncated {
					t.Errorf("a cut-short file must read as %q, got %q/%v",
						mediainfo.CodeTruncated, bad.Code, implausible)
				}
			} else if implausible {
				t.Errorf("the whole control file was called implausible: %s", bad.Reason)
			}
		})
	}
}
