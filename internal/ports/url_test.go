package ports

import "testing"

func TestNormalizeURL(t *testing.T) {
	for in, want := range map[string]string{
		"192.168.4.7":                 "http://192.168.4.7",
		"192.168.4.7:6789":            "http://192.168.4.7:6789",
		"  nzbget.local:6789/ ":       "http://nzbget.local:6789",
		"http://192.168.4.7:6789":     "http://192.168.4.7:6789",
		"https://api.drunkenslug.com": "https://api.drunkenslug.com",
		"https://x.example/":          "https://x.example",
		"":                            "",
	} {
		if got := NormalizeURL(in); got != want {
			t.Errorf("NormalizeURL(%q) = %q, want %q", in, got, want)
		}
	}
}
