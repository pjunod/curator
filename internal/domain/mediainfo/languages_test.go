package mediainfo_test

import (
	"reflect"
	"testing"

	"github.com/pjunod/monarr/internal/domain/mediainfo"
)

// TestAudioLanguages pins the ADR 0022 reading of a track list: canonical
// codes, and "known" only when every track declared one.
func TestAudioLanguages(t *testing.T) {
	cases := []struct {
		name  string
		audio []mediainfo.AudioInfo
		langs []string
		known bool
	}{
		{"no audio", nil, nil, false},
		{"one untagged track", []mediainfo.AudioInfo{{Codec: "aac"}}, nil, false},
		{"und is untagged", []mediainfo.AudioInfo{{Codec: "aac", Language: "und"}}, nil, false},
		{"German only, tagged", []mediainfo.AudioInfo{{Codec: "ac3", Language: "ger"}}, []string{"de"}, true},
		{"German and English, mixed spellings",
			[]mediainfo.AudioInfo{{Codec: "ac3", Language: "deu"}, {Codec: "aac", Language: "en-US"}},
			[]string{"de", "en"}, true},
		{"German tagged, second track untagged: not known",
			[]mediainfo.AudioInfo{{Codec: "ac3", Language: "ger"}, {Codec: "aac"}},
			[]string{"de"}, false},
		{"duplicates collapse",
			[]mediainfo.AudioInfo{{Codec: "truehd", Language: "eng"}, {Codec: "ac3", Language: "eng"}},
			[]string{"en"}, true},
	}
	for _, tc := range cases {
		info := mediainfo.Info{Audio: tc.audio}
		langs, known := info.AudioLanguages()
		if !reflect.DeepEqual(langs, tc.langs) || known != tc.known {
			t.Errorf("%s: AudioLanguages = %v,%v want %v,%v", tc.name, langs, known, tc.langs, tc.known)
		}
	}
}

// TestSummaryNamesTheLanguage: the facts pill is where somebody finds out
// their "1080p WEB-DL" is the German dub.
func TestSummaryNamesTheLanguage(t *testing.T) {
	info := mediainfo.Info{
		Container:  mediainfo.ContainerMKV,
		Video:      &mediainfo.VideoInfo{Codec: "h264", Width: 1920, Height: 1080, BitDepth: 8},
		Audio:      []mediainfo.AudioInfo{{Codec: "ac3", Channels: 6, Language: "ger"}},
		DurationMS: 90 * 60 * 1000, BitrateKbps: 5000,
	}
	if got, want := info.Summary(), "1080p · H.264 · AC-3 · German · 5.0 Mbps"; got != want {
		t.Errorf("Summary = %q, want %q", got, want)
	}
	info.Audio = append(info.Audio, mediainfo.AudioInfo{Codec: "aac", Channels: 2})
	if got, want := info.Summary(), "1080p · H.264 · AC-3 · German/undeclared · 5.0 Mbps"; got != want {
		t.Errorf("Summary = %q, want %q", got, want)
	}
	info.Audio = []mediainfo.AudioInfo{{Codec: "aac", Channels: 2}}
	if got, want := info.Summary(), "1080p · H.264 · AAC · 5.0 Mbps"; got != want {
		t.Errorf("fully untagged must stay quiet: Summary = %q, want %q", got, want)
	}
}
