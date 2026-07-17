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
