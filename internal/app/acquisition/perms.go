package acquisition

import (
	"io/fs"
	"os"
	"strconv"
)

// Permissions monarr gives the files and folders it creates.
//
// This exists because it was silently wrong. `place` copies across
// filesystems via os.CreateTemp, which creates the file mode 0600 — owner
// read/write and nothing else. Every cross-filesystem import therefore landed
// as a file no media server could open and no other user could probe. The
// symptom is the worst kind: the import succeeds, the library page looks
// right, and the file simply will not play.
//
// Hard-linked imports have the same problem from the other direction: the file
// keeps whatever mode the download client gave it, which for a client running
// with a restrictive umask is equally unopenable.
//
// So monarr now sets the mode explicitly on everything it places, rather than
// inheriting whatever the previous owner happened to leave behind.
const (
	// DefaultFileMode is what a media file needs to be: readable by the
	// group and by other users, because the thing that plays it is almost
	// never the thing that downloaded it.
	DefaultFileMode fs.FileMode = 0o644
	// DefaultDirMode needs the execute bit to be traversable at all.
	DefaultDirMode fs.FileMode = 0o755
)

// Environment overrides, as octal strings ("0644", "664", "0640").
const (
	FileModeEnv = "MONARR_FILE_MODE"
	DirModeEnv  = "MONARR_DIR_MODE"
)

// fileMode returns the mode for imported media files.
func fileMode() fs.FileMode { return modeFromEnv(FileModeEnv, DefaultFileMode) }

// dirMode returns the mode for library folders monarr creates.
func dirMode() fs.FileMode { return modeFromEnv(DirModeEnv, DefaultDirMode) }

// modeFromEnv parses an octal mode, falling back to the default for anything
// it cannot make sense of. A typo in an env var must not produce a file nobody
// can read — the whole point of this file.
func modeFromEnv(key string, fallback fs.FileMode) fs.FileMode {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	v, err := strconv.ParseUint(raw, 8, 32)
	if err != nil || v == 0 || v > 0o777 {
		return fallback
	}
	return fs.FileMode(v)
}

// applyMode sets a path's permissions explicitly.
//
// os.Chmod is not subject to the process umask, which os.MkdirAll and
// os.OpenFile both are — a monarr running under umask 077 creates 0700
// folders however carefully it asks for 0755. Asking, then setting, is the
// only way to actually get the mode.
func applyMode(path string, mode fs.FileMode) error {
	return os.Chmod(path, mode)
}
