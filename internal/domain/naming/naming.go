// Package naming renders on-disk names. Phase 1 needs only the library
// folder name for a newly added item; the full token-compatible Renamer
// (blueprint §4.3) arrives in Phase 2.
package naming

import (
	"fmt"
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
)

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
