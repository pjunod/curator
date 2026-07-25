package mediainfo_test

import (
	"testing"

	"github.com/monarr-media/monarr/internal/domain/mediainfo"
	"github.com/monarr-media/monarr/internal/domain/quality"
)

// vid builds a measured Info from the handful of facts inference actually
// reads, so each case below reads as the claim it is making rather than as a
// struct literal.
func vid(width, height int, bitrateKbps int64, audio ...string) mediainfo.Info {
	info := mediainfo.Info{
		Container:   mediainfo.ContainerMKV,
		Video:       &mediainfo.VideoInfo{Codec: "h264", Width: width, Height: height, BitDepth: 8},
		BitrateKbps: bitrateKbps,
		DurationMS:  7_200_000,
	}
	for _, a := range audio {
		info.Audio = append(info.Audio, mediainfo.AudioInfo{Codec: a, Channels: 6})
	}
	return info
}

func interlaced(info mediainfo.Info) mediainfo.Info {
	info.Video.Interlaced = true
	return info
}

func muxedBy(info mediainfo.Info, app string) mediainfo.Info {
	info.WritingApp = app
	return info
}

// TestInferSource walks every rule in plan §4.3, in order, plus the shrug.
// The confidence matters as much as the verdict: medium and low never justify
// replacing a file, so a rule that quietly promoted itself to high would be a
// churn bug, not a cosmetic one.
func TestInferSource(t *testing.T) {
	cases := []struct {
		name       string
		info       mediainfo.Info
		want       quality.Source
		confidence mediainfo.Confidence
		why        string
	}{
		{
			name: "interlaced is broadcast, full stop",
			info: interlaced(vid(1920, 1080, 8_000, "ac3")),
			want: quality.SourceHDTV, confidence: mediainfo.ConfidenceHigh,
			why: "nothing but a broadcast source is interlaced",
		},
		{
			name: "4K with lossless audio at disc bitrate is a remux",
			info: vid(3840, 2160, 60_000, "truehd", "ac3"),
			want: quality.SourceRemux, confidence: mediainfo.ConfidenceHigh,
		},
		{
			name: "1080p with lossless audio at disc bitrate is a remux",
			info: vid(1920, 1080, 28_000, "truehd"),
			want: quality.SourceRemux, confidence: mediainfo.ConfidenceHigh,
		},
		{
			name: "lossless audio under the remux floor is an encode that kept it",
			info: vid(1920, 1080, 12_000, "flac"),
			want: quality.SourceBluray, confidence: mediainfo.ConfidenceMedium,
			why: "this is the case that must NOT be called a remux",
		},
		{
			name: "PCM counts as lossless too",
			info: vid(1920, 1080, 9_000, "pcm"),
			want: quality.SourceBluray, confidence: mediainfo.ConfidenceMedium,
		},
		{
			name: "MakeMKV signed it",
			info: muxedBy(vid(1920, 1080, 9_000, "ac3"), "MakeMKV v1.17.5"),
			want: quality.SourceRemux, confidence: mediainfo.ConfidenceHigh,
		},
		{
			name: "muxer match is case-insensitive and substring",
			info: muxedBy(vid(1920, 1080, 9_000, "ac3"), "libmakemkv v1.17.5 (linux)"),
			want: quality.SourceRemux, confidence: mediainfo.ConfidenceHigh,
		},
		{
			name: "disc bitrate without lossless audio: probably a remux, hedged",
			info: vid(3840, 2160, 45_000, "eac3"),
			want: quality.SourceRemux, confidence: mediainfo.ConfidenceMedium,
			why: "the strongest evidence is missing, so it must not read as high",
		},
		{
			name: "E-AC-3 at web bitrate is a web release",
			info: vid(1920, 1080, 5_000, "eac3"),
			want: quality.SourceWEBDL, confidence: mediainfo.ConfidenceMedium,
		},
		{
			name: "AAC at web bitrate is a web release",
			info: vid(1280, 720, 2_500, "aac"),
			want: quality.SourceWEBDL, confidence: mediainfo.ConfidenceMedium,
		},
		{
			name: "AC-3 alone proves nothing, so the same verdict comes in lower",
			info: vid(1920, 1080, 5_000, "ac3"),
			want: quality.SourceWEBDL, confidence: mediainfo.ConfidenceLow,
		},
		{
			name: "SD at web bitrate",
			info: vid(854, 480, 1_000, "aac"),
			want: quality.SourceWEBDL, confidence: mediainfo.ConfidenceMedium,
		},
		{
			name: "DTS at encode bitrate is a disc encode",
			info: vid(1920, 1080, 8_000, "dts"),
			want: quality.SourceBluray, confidence: mediainfo.ConfidenceLow,
			why: "DTS is not a streaming codec, so rule 6 cannot claim it",
		},
		{
			name: "no audio track at all, disc bitrate",
			info: vid(1920, 1080, 25_000),
			want: quality.SourceRemux, confidence: mediainfo.ConfidenceMedium,
		},
		{
			name: "starved bitrate below every band",
			info: vid(1920, 1080, 800, "aac"),
			want: quality.SourceUnknown, confidence: mediainfo.ConfidenceNone,
			why: "shrugging beats guessing",
		},
		{
			name: "no audio, no bitrate signal",
			info: vid(1920, 1080, 0),
			want: quality.SourceUnknown, confidence: mediainfo.ConfidenceNone,
		},
		{
			name: "nothing measured",
			info: mediainfo.Info{Container: mediainfo.ContainerMKV},
			want: quality.SourceUnknown, confidence: mediainfo.ConfidenceNone,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, conf := mediainfo.InferSource(tc.info)
			if got != tc.want || conf != tc.confidence {
				t.Errorf("InferSource = (%s, %s), want (%s, %s)%s",
					got, conf, tc.want, tc.confidence, suffix(tc.why))
			}
		})
	}
}

// TestContradicts pins the demotion of filenames to hints. A token stands
// unless the bytes make it impossible — being unable to disprove a claim is
// not grounds for overriding it.
func TestContradicts(t *testing.T) {
	cases := []struct {
		name  string
		info  mediainfo.Info
		token quality.Source
		want  bool
	}{
		{
			name: "REMUX in the name, lossy audio, encode bitrate",
			info: vid(1920, 1080, 5_000, "eac3"), token: quality.SourceRemux, want: true,
		},
		{
			name: "REMUX in the name, disc bitrate: plausible, let it stand",
			info: vid(1920, 1080, 28_000, "eac3"), token: quality.SourceRemux, want: false,
		},
		{
			name: "REMUX in the name with lossless audio: plausible",
			info: vid(1920, 1080, 9_000, "truehd"), token: quality.SourceRemux, want: false,
		},
		{
			name: "WEB-DL in the name with a TrueHD track: streaming never ships that",
			info: vid(1920, 1080, 9_000, "truehd"), token: quality.SourceWEBDL, want: true,
		},
		{
			name: "WEBRip in the name with FLAC: same story",
			info: vid(1920, 1080, 9_000, "flac"), token: quality.SourceWEBRip, want: true,
		},
		{
			name: "WEB-DL in the name with E-AC-3: exactly what it should be",
			info: vid(1920, 1080, 5_000, "eac3"), token: quality.SourceWEBDL, want: false,
		},
		{
			name: "Bluray has no measurable tell, so it is never contradicted",
			info: vid(1920, 1080, 900, "aac"), token: quality.SourceBluray, want: false,
		},
		{
			name: "HDTV has no measurable tell either",
			info: vid(1920, 1080, 28_000, "truehd"), token: quality.SourceHDTV, want: false,
		},
		{
			name: "no token to contradict",
			info: vid(1920, 1080, 5_000, "eac3"), token: quality.SourceUnknown, want: false,
		},
		{
			name: "nothing measured, so nothing to contradict with",
			info: mediainfo.Info{}, token: quality.SourceRemux, want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mediainfo.Contradicts(tc.info, tc.token); got != tc.want {
				t.Errorf("Contradicts = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestResolvePickOrder walks the four-way pick from plan §4.3. Resolution is
// measured in every branch where anything was measured at all — that half is
// not a judgement call and this test says so at every step.
func TestResolvePickOrder(t *testing.T) {
	cases := []struct {
		name     string
		info     mediainfo.Info
		hint     quality.Quality
		hintProv mediainfo.Provenance
		want     quality.Quality
		prov     mediainfo.Provenance
		conf     mediainfo.Confidence
		why      string
	}{
		{
			name:     "high-confidence measurement outranks any claim",
			info:     vid(1920, 1080, 28_000, "truehd"),
			hint:     quality.Quality{Source: quality.SourceWEBDL, Resolution: 2160},
			hintProv: mediainfo.ProvenanceFilename,
			want:     quality.Quality{Source: quality.SourceRemux, Resolution: 1080},
			prov:     mediainfo.ProvenanceProbe, conf: mediainfo.ConfidenceHigh,
			why: "the name claimed 2160p WEB-DL; the file is a 1080p remux",
		},
		{
			name:     "an uncontradicted claim beats a hedged measurement",
			info:     vid(1920, 1080, 5_000, "eac3"),
			hint:     quality.Quality{Source: quality.SourceBluray, Resolution: 1080},
			hintProv: mediainfo.ProvenanceFilename,
			want:     quality.Quality{Source: quality.SourceBluray, Resolution: 1080},
			prov:     mediainfo.ProvenanceFilename, conf: mediainfo.ConfidenceNone,
			why: "somebody looked at this file and said Bluray; nothing disproves it",
		},
		{
			name:     "a contradicted claim loses to a hedged measurement",
			info:     vid(1920, 1080, 5_000, "eac3"),
			hint:     quality.Quality{Source: quality.SourceRemux, Resolution: 1080},
			hintProv: mediainfo.ProvenanceFilename,
			want:     quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080},
			prov:     mediainfo.ProvenanceProbe, conf: mediainfo.ConfidenceMedium,
		},
		{
			name:     "the release claim carries its own provenance",
			info:     vid(1920, 1080, 5_000, "eac3"),
			hint:     quality.Quality{Source: quality.SourceBluray, Resolution: 1080},
			hintProv: mediainfo.ProvenanceRelease,
			want:     quality.Quality{Source: quality.SourceBluray, Resolution: 1080},
			prov:     mediainfo.ProvenanceRelease, conf: mediainfo.ConfidenceNone,
		},
		{
			name:     "no verdict and no claim: the resolution is still fact",
			info:     vid(1920, 1080, 800, "aac"),
			hint:     quality.Quality{},
			hintProv: mediainfo.ProvenanceFilename,
			want:     quality.Quality{Source: quality.SourceUnknown, Resolution: 1080},
			prov:     mediainfo.ProvenanceProbe, conf: mediainfo.ConfidenceNone,
			why: "an unknown source at a known resolution is a real, useful state",
		},
		{
			name:     "nothing measured but the name said something",
			info:     mediainfo.Info{},
			hint:     quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080},
			hintProv: mediainfo.ProvenanceFilename,
			want:     quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080},
			prov:     mediainfo.ProvenanceFilename, conf: mediainfo.ConfidenceNone,
		},
		{
			name:     "nothing measured and nothing claimed",
			info:     mediainfo.Info{},
			hint:     quality.Quality{},
			hintProv: mediainfo.ProvenanceFilename,
			want:     quality.Quality{Source: quality.SourceUnknown},
			prov:     mediainfo.ProvenanceFailed, conf: mediainfo.ConfidenceNone,
			why: "this is the only state that means on-disk-but-unverified",
		},
		{
			name:     "the measured resolution always wins over a claimed one",
			info:     vid(3840, 2160, 45_000, "eac3"),
			hint:     quality.Quality{Source: quality.SourceBluray, Resolution: 1080},
			hintProv: mediainfo.ProvenanceFilename,
			want:     quality.Quality{Source: quality.SourceBluray, Resolution: 2160},
			prov:     mediainfo.ProvenanceFilename, conf: mediainfo.ConfidenceNone,
			why: "source from the uncontradicted name, resolution from the pixels",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q, prov, conf := mediainfo.Resolve(tc.info, tc.hint, tc.hintProv)
			if q != tc.want || prov != tc.prov || conf != tc.conf {
				t.Errorf("Resolve = (%v, %s, %s), want (%v, %s, %s)%s",
					q, prov, conf, tc.want, tc.prov, tc.conf, suffix(tc.why))
			}
		})
	}
}

// TestProvenanceVerified pins the don't-churn predicate. Getting this wrong in
// the permissive direction means monarr replaces files on guesses, which ADR
// 0013 §5 calls the one unforgivable move for a tool sharing a disk with a
// user's collection.
func TestProvenanceVerified(t *testing.T) {
	cases := []struct {
		prov mediainfo.Provenance
		conf mediainfo.Confidence
		want bool
	}{
		{mediainfo.ProvenanceManual, mediainfo.ConfidenceNone, true},
		{mediainfo.ProvenanceFilename, mediainfo.ConfidenceNone, true},
		{mediainfo.ProvenanceRelease, mediainfo.ConfidenceNone, true},
		{mediainfo.ProvenanceProbe, mediainfo.ConfidenceHigh, true},
		{mediainfo.ProvenanceProbe, mediainfo.ConfidenceMedium, false},
		{mediainfo.ProvenanceProbe, mediainfo.ConfidenceLow, false},
		{mediainfo.ProvenanceProbe, mediainfo.ConfidenceNone, false},
		{mediainfo.ProvenanceFailed, mediainfo.ConfidenceNone, false},
		{mediainfo.ProvenanceUnknown, mediainfo.ConfidenceNone, false},
	}
	for _, tc := range cases {
		if got := tc.prov.Verified(tc.conf); got != tc.want {
			t.Errorf("Provenance(%q).Verified(%q) = %v, want %v", tc.prov, tc.conf, got, tc.want)
		}
	}
	for _, p := range []mediainfo.Provenance{
		mediainfo.ProvenanceProbe, mediainfo.ProvenanceFilename, mediainfo.ProvenanceRelease,
		mediainfo.ProvenanceManual, mediainfo.ProvenanceFailed, mediainfo.ProvenanceUnknown,
	} {
		if !p.Valid() {
			t.Errorf("%q is written by this codebase but reports invalid", p)
		}
		if p.Label() == "" {
			t.Errorf("%q has no UI label", p)
		}
	}
	if mediainfo.Provenance("nonsense").Valid() {
		t.Error("an unknown provenance reported valid")
	}
}

func suffix(why string) string {
	if why == "" {
		return ""
	}
	return " — " + why
}
