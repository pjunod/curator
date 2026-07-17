package naming

import "testing"

func TestFolderName(t *testing.T) {
	cases := []struct {
		title string
		year  int
		want  string
	}{
		{"The Matrix", 1999, "The Matrix (1999)"},
		{"Alien: Romulus", 2024, "Alien - Romulus (2024)"},
		{"What If...?", 2021, "What If (2021)"}, // trailing dots stripped for Windows/SMB
		{"AC/DC: Let There Be Rock", 1980, "AC+DC - Let There Be Rock (1980)"},
		{"Unknown Year", 0, "Unknown Year"},
		{"   spaced    out  ", 2020, "spaced out (2020)"},
		{"***", 0, "Untitled"},
	}
	for _, c := range cases {
		if got := FolderName(c.title, c.year); got != c.want {
			t.Errorf("FolderName(%q, %d) = %q, want %q", c.title, c.year, got, c.want)
		}
	}
}
