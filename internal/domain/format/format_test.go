package format

import "testing"

func TestScoreAndMatches(t *testing.T) {
	formats := []CustomFormat{
		{Name: "x265", Pattern: `\b(x265|hevc)\b`, Score: 50},
		{Name: "Bad Group", Pattern: `-JUNK$`, Score: -100},
		{Name: "Broken", Pattern: `([`, Score: 999}, // invalid regex scores nothing
	}
	if got := Score("Show.S01E01.1080p.HEVC-GRP", formats); got != 50 {
		t.Errorf("hevc score = %d", got)
	}
	if got := Score("Movie.2024.720p.x264-JUNK", formats); got != -100 {
		t.Errorf("junk score = %d", got)
	}
	if got := Score("Movie.2024.x265-JUNK", formats); got != -50 {
		t.Errorf("combined score = %d", got)
	}
	if m := Matches("Movie.x265-JUNK", formats); len(m) != 2 {
		t.Errorf("matches = %v", m)
	}
	if got := Score("nothing here", formats); got != 0 {
		t.Errorf("no-match score = %d", got)
	}
}
