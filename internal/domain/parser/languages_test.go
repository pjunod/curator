package parser_test

import (
	"reflect"
	"testing"

	"github.com/pjunod/monarr/internal/domain/parser"
)

// TestParseLanguages pins the ADR 0022 reading of language tags: only the
// tag section of the name is read, silence means English, MULTi/DUAL mean
// "several", and the compound source tags never leak a "DL".
func TestParseLanguages(t *testing.T) {
	cases := []struct {
		release string
		want    []string
	}{
		{"Just.Friends.2005.1080p.WEB-DL.DD5.1.H.264-GRP", []string{"en"}},
		{"Just.Friends.2005.GERMAN.DL.1080p.BluRay.x264-GRP", []string{"de", "mul"}},
		{"Just.Friends.2005.German.AC3.1080p.WEBRip.x264-GRP", []string{"de"}},
		{"Just.Friends.2005.MULTi.1080p.WEB-DL.x264-GRP", []string{"mul"}},
		{"Just.Friends.2005.TRUEFRENCH.1080p.BluRay.x264-GRP", []string{"fr"}},
		{"Just.Friends.2005.FRENCH.ENGLISH.1080p.BluRay.x264-GRP", []string{"en", "fr"}},
		{"Just.Friends.2005.1080p.BluRay.x264.DUAL-AUDIO-GRP", []string{"mul"}},
		{"Just Friends (2005) [1080p] [BluRay] [ITA ENG]", []string{"en", "it"}},
		// The title is never read: a language in it is a title.
		{"The.French.Connection.1971.1080p.BluRay.x264-GRP", []string{"en"}},
		{"German.Concentration.Camps.Factual.Survey.2014.1080p.WEB-DL", []string{"en"}},
		// Subtitle tags say nothing about the audio.
		{"Just.Friends.2005.VOSTFR.1080p.WEB-DL.x264-GRP", []string{"en"}},
		{"Just.Friends.2005.1080p.WEB-DL.NLSUBS.x264-GRP", []string{"en"}},
		// WEB-DL never yields a DL.
		{"Show.S01E02.1080p.WEB.DL.x264-GRP", []string{"en"}},
		{"Show.S01E02.German.DL.1080p.WEB.h264-GRP", []string{"de", "mul"}},
		// Anime dual audio.
		{"[Group] Show - 03 [Dual Audio][1080p][HEVC]", []string{"mul"}},
		// Ordinary words that happen to be ISO codes are not tags.
		{"Show.S01E02.Fin.1080p.WEB-DL.x264-GRP", []string{"en"}},
		{"Show.S01E02.May.Day.720p.HDTV.x264-GRP", []string{"en"}},
	}
	for _, tc := range cases {
		got := parser.Parse(tc.release).Languages
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: Languages = %v, want %v", tc.release, got, tc.want)
		}
	}
}
