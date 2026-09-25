package naming

import "testing"

func TestFolderTag(t *testing.T) {
	for _, c := range []struct{ provider, id, want string }{
		{"tmdb", "123", "{tmdb-123}"},
		{"TVDB", " 456 ", "{tvdb-456}"},
		{"imdb", "tt0000001", "{imdb-tt0000001}"},
		{"", "1", ""},
		{"tmdb", "", ""},
	} {
		if got := FolderTag(c.provider, c.id); got != c.want {
			t.Errorf("FolderTag(%q, %q) = %q, want %q", c.provider, c.id, got, c.want)
		}
	}
}

func TestParseFolderTag(t *testing.T) {
	for _, c := range []struct {
		in, rest, provider, id string
		ok                     bool
	}{
		{"Leviticus (2022) {tmdb-123}", "Leviticus (2022)", "tmdb", "123", true},
		{"Leviticus (2022) [tmdbid-123]", "Leviticus (2022)", "tmdb", "123", true},
		{"Show {TVDB-99}", "Show", "tvdb", "99", true},
		{"Film (1999) {imdb-tt0137523}", "Film (1999)", "imdb", "tt0137523", true},
		{"Book {olid-OL1W}", "Book", "olid", "ol1w", true},
		{"Leviticus (2022)", "Leviticus (2022)", "", "", false},
		// Only a trailing hint counts; braces mid-name are part of the title.
		{"{tmdb-1} Something", "{tmdb-1} Something", "", "", false},
		{"Film {edition-Director's Cut}", "Film {edition-Director's Cut}", "", "", false},
	} {
		rest, provider, id, ok := ParseFolderTag(c.in)
		if rest != c.rest || provider != c.provider || id != c.id || ok != c.ok {
			t.Errorf("ParseFolderTag(%q) = %q, %q, %q, %v; want %q, %q, %q, %v",
				c.in, rest, provider, id, ok, c.rest, c.provider, c.id, c.ok)
		}
	}
	// Round trip: what FolderTag writes, ParseFolderTag reads back.
	rest, provider, id, ok := ParseFolderTag(FolderName("Leviticus", 2022) + " " + FolderTag("tmdb", "1002"))
	if !ok || rest != "Leviticus (2022)" || provider != "tmdb" || id != "1002" {
		t.Errorf("round trip = %q %q %q %v", rest, provider, id, ok)
	}
}
