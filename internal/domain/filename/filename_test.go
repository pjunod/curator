package filename

import (
	"reflect"
	"testing"
)

func TestExtract(t *testing.T) {
	cases := []struct {
		in     string
		season int
		eps    []int
		ok     bool
	}{
		{"Show.Name.S01E02.1080p.mkv", 1, []int{2}, true},
		{"Show Name - s3e11 - Title.mp4", 3, []int{11}, true},
		{"Show.S01E01E02.mkv", 1, []int{1, 2}, true},
		{"Show.S01E01-03.mkv", 1, []int{1, 2, 3}, true},
		{"Show.S01E01.E02.mkv", 1, []int{1, 2}, true},
		{"Show.S2024E120.Daily.mkv", 24, []int{120}, false}, // 4-digit season not matched as SxxExx
		{"Show.1x05.HDTV.avi", 1, []int{5}, true},
		{"Show.01x05-06.avi", 1, []int{5, 6}, true},
		{"/library/Show/Season 1/Show.S01E09.mkv", 1, []int{9}, true},
		{"Movie.Name.2024.1080p.mkv", 0, nil, false},
		{"random-file.txt", 0, nil, false},
	}
	for _, c := range cases {
		got, ok := Extract(c.in)
		if c.in == "Show.S2024E120.Daily.mkv" {
			// Document current behavior: S2024 exceeds the 2-digit season
			// cap, so the SxxExx branch skips it entirely. Daily-show dates
			// are Phase 2 parser territory.
			if ok {
				t.Errorf("Extract(%q) matched unexpectedly: %+v", c.in, got)
			}
			continue
		}
		if ok != c.ok {
			t.Errorf("Extract(%q) ok = %v, want %v", c.in, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if got.Season != c.season || !reflect.DeepEqual(got.Episodes, c.eps) {
			t.Errorf("Extract(%q) = %+v, want S%d %v", c.in, got, c.season, c.eps)
		}
	}
}

func TestIsVideo(t *testing.T) {
	if !IsVideo("a/b/c.MKV") || !IsVideo("x.mp4") {
		t.Error("expected video extensions to match case-insensitively")
	}
	if IsVideo("x.srt") || IsVideo("x") {
		t.Error("non-video extensions must not match")
	}
}

func TestAudiobookExtensions(t *testing.T) {
	want := map[string]string{
		"chapter.MP3": "mp3", "book.m4b": "m4b", "book.m4a": "m4a",
		"book.aac": "aac", "book.flac": "flac", "book.ogg": "ogg",
		"book.opus": "opus", "book.wav": "wav", "book.wma": "wma",
	}
	for name, source := range want {
		if !IsBook(name) {
			t.Errorf("%s was not recognized as a book file", name)
		}
		if got := BookQualitySource(name); got != source {
			t.Errorf("BookQualitySource(%q) = %q, want %q", name, got, source)
		}
	}
}
