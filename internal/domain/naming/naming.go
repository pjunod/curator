// Package naming renders on-disk names. Phase 1 needs only the library
// folder name for a newly added item; the full token-compatible Renamer
// (blueprint §4.3) arrives in Phase 2.
package naming

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// illegal characters for cross-platform folder names.
var folderReplacer = strings.NewReplacer(
	"/", "+", "\\", "+", ":", " -", "*", "", "?", "",
	"\"", "", "<", "", ">", "", "|", "", "\x00", "",
)

// Render substitutes {Token} placeholders in a template. Unknown tokens
// render as empty. Numeric tokens support zero-padding via "{Token:00}".
// The token vocabulary is Sonarr/Radarr-compatible where the concepts
// overlap ({Series Title}, {Movie Title}, {Release Year}, {Quality Full},
// {season:00}, {episode:00}, {Episode Title}).
func Render(template string, tokens map[string]string) string {
	var b strings.Builder
	rest := template
	for {
		open := strings.IndexByte(rest, '{')
		if open < 0 {
			b.WriteString(rest)
			break
		}
		closing := strings.IndexByte(rest[open:], '}')
		if closing < 0 {
			b.WriteString(rest)
			break
		}
		b.WriteString(rest[:open])
		token := rest[open+1 : open+closing]
		rest = rest[open+closing+1:]

		name, pad, _ := strings.Cut(token, ":")
		val := tokens[name]
		if pad != "" && len(val) < len(pad) {
			val = strings.Repeat("0", len(pad)-len(val)) + val
		}
		b.WriteString(val)
	}
	// Collapse artifacts from empty tokens.
	out := strings.Join(strings.Fields(b.String()), " ")
	out = strings.ReplaceAll(out, "( )", "")
	out = strings.TrimSpace(strings.TrimSuffix(out, "-"))
	return out
}

// Default templates, upstream-compatible in spirit.
const (
	// MovieFileTemplate → "The Matrix (1999) [Bluray-1080p]"
	MovieFileTemplate = "{Movie Title} ({Release Year}) [{Quality Full}]"
	// EpisodeFileTemplate → "Show - S01E02 - Pilot [WEB-DL 1080p]"
	EpisodeFileTemplate = "{Series Title} - S{season:00}E{episode:00} - {Episode Title} [{Quality Full}]"
	// SeasonFolderTemplate → "Season 1"
	SeasonFolderTemplate = "Season {season}"
	// BookFileTemplate → "Project Hail Mary - Andy Weir" (Calibre-style)
	BookFileTemplate = "{Book Title} - {Author Name}"
)

// BookFolder renders the Calibre-friendly library location for a book:
// "Author Name/Book Title" (ADR 0006). Both segments are sanitized
// independently; an unknown author lands under "Unknown Author".
func BookFolder(author, title string) string {
	a := FolderName(author, 0)
	if author == "" {
		a = "Unknown Author"
	}
	return a + string(os.PathSeparator) + FolderName(title, 0)
}

// BookFileName renders the default book file base name (no extension):
// "Book Title - Author Name", degrading to the bare title with no author.
func BookFileName(author, title string) string {
	if author == "" {
		return SafeFileName(title)
	}
	return SafeFileName(Render(BookFileTemplate, map[string]string{
		"Book Title": title, "Author Name": author,
	}))
}

// SafeFileName strips characters that break filesystems, preserving more
// punctuation than FolderName (files keep brackets).
func SafeFileName(name string) string {
	name = strings.NewReplacer("/", "+", "\\", "+", ":", " -", "*", "", "?", "",
		"\"", "", "<", "", ">", "", "|", "", "\x00", "").Replace(name)
	name = strings.Join(strings.Fields(name), " ")
	return strings.Trim(name, ". ")
}

// FolderName renders the default library folder for a media item:
// "Title (Year)" — or just "Title" when the year is unknown — with
// filesystem-hostile characters stripped and whitespace collapsed.
func FolderName(title string, year int) string {
	name := folderReplacer.Replace(title)
	name = strings.Join(strings.Fields(name), " ") // collapse runs of spaces
	name = strings.Trim(name, ". ")                // trailing dots break Windows/SMB
	if name == "" {
		name = "Untitled"
	}
	if year > 0 {
		return fmt.Sprintf("%s (%d)", name, year)
	}
	return name
}

// FolderTag renders the provider-id suffix that tells two same-named items
// apart on disk: "{tmdb-123}", "{tvdb-456}", "{imdb-tt0000001}". The brace
// form is Plex's documented folder hint and Radarr's {tmdb-…} naming token,
// so a disambiguated folder is also an unambiguous one to every scanner
// that reads the library after Monarr.
func FolderTag(provider, id string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	id = strings.TrimSpace(id)
	if provider == "" || id == "" {
		return ""
	}
	return "{" + provider + "-" + id + "}"
}

// folderTagRE matches a trailing provider-id hint in either of the two
// conventions a library is likely to hold: Plex/Radarr "{tmdb-123}" and
// Jellyfin "[tmdbid-123]".
//
// It also reads back the two forms Monarr itself writes beyond the provider
// hint: "{monarr-12}" for items no provider backs, and a trailing counter
// ("{tmdb-1} (2)") for the rare case where even the tagged name was taken.
var folderTagRE = regexp.MustCompile(`(?i)\s*[\{\[](tmdb|tvdb|imdb|olid|monarr)(?:id)?-([a-z0-9]+)[\}\]](?:\s*\(\d+\))?\s*$`)

// ParseFolderTag splits a trailing provider-id hint off a folder name.
// "Leviticus (2022) {tmdb-123}" → ("Leviticus (2022)", "tmdb", "123", true).
// A name without one comes back unchanged with ok false.
func ParseFolderTag(name string) (rest, provider, id string, ok bool) {
	m := folderTagRE.FindStringSubmatchIndex(name)
	if m == nil {
		return name, "", "", false
	}
	return strings.TrimSpace(name[:m[0]]),
		strings.ToLower(name[m[2]:m[3]]),
		strings.ToLower(name[m[4]:m[5]]), true
}
