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

func TestMapRemotePath(t *testing.T) {
	maps := []PathMapping{
		{Remote: "/data/completed", Local: "/pool/downloads"},
		{Remote: "/other/", Local: "/pool/other"},
	}
	cases := map[string]string{
		"/data/completed/Show.S01E01":  "/pool/downloads/Show.S01E01",
		"/data/completed":              "/pool/downloads",
		"/data/completed-extra/x":      "/data/completed-extra/x", // component boundary
		"/other/thing":                 "/pool/other/thing",       // trailing slash tolerated
		"/untouched/path":              "/untouched/path",
		"/database/completed/Show.mkv": "/database/completed/Show.mkv",
	}
	for in, want := range cases {
		if got := MapRemotePath(maps, in); got != want {
			t.Errorf("MapRemotePath(%q) = %q, want %q", in, got, want)
		}
	}
	if got := MapRemotePath(nil, "/x/y"); got != "/x/y" {
		t.Errorf("nil maps: %q", got)
	}
}
