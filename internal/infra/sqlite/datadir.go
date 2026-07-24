package sqlite

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// composeDefaultDataDir is the host path deploy/docker-compose.example.yml
// mounts at /data — named only to make the error message concrete for the
// deployment nearly everyone runs.
const composeDefaultDataDir = "/srv/monarr"

// DataDirError reports a data dir Monarr cannot write to, with enough
// ownership detail for the operator to fix it in one command.
//
// The overwhelmingly common cause is a Docker bind mount whose host
// directory did not exist: the daemon creates the missing source path as
// root:root, the container runs as an unprivileged user, and SQLite then
// fails to create the database file with an error that says nothing about
// ownership. Hence this type — the failure has to explain itself.
type DataDirError struct {
	Dir     string
	Err     error
	dirInfo ownership // zero if the directory could not be stat'd
	proc    ownership
}

func (e *DataDirError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "sqlite: data dir %s is not writable", e.Dir)
	if e.proc.known {
		fmt.Fprintf(&b, " by uid %d gid %d", e.proc.uid, e.proc.gid)
	}
	if e.dirInfo.known {
		fmt.Fprintf(&b, " (owned by uid %d gid %d, mode %s)",
			e.dirInfo.uid, e.dirInfo.gid, e.dirInfo.mode)
	}
	fmt.Fprintf(&b, ": %v", e.Err)

	// Only volunteer the bind-mount diagnosis when the shape matches:
	// a root-owned directory and a non-root process.
	if e.dirInfo.known && e.proc.known && e.dirInfo.uid == 0 && e.proc.uid != 0 {
		fmt.Fprintf(&b, "\n\nThis is almost always a Docker bind mount whose host directory "+
			"did not exist — the daemon created it as root, and Monarr runs as uid %d.",
			e.proc.uid)
		// The host path is deliberately a placeholder: from inside the
		// container we know only the mount point, and printing a guessed
		// host path as fact is how people chown the wrong directory.
		hint := ""
		if e.Dir != composeDefaultDataDir {
			hint = " (the compose default is " + composeDefaultDataDir + ")"
		}
		fmt.Fprintf(&b, "\nFix it on the HOST, not in the container — on whichever host "+
			"directory you mounted at %s%s:"+
			"\n    sudo install -d -o %d -g %d -m 0755 HOST_DIR\n"+
			"then start Monarr again. deploy/bootstrap.sh does this for every path in your .env.",
			e.Dir, hint, e.proc.uid, e.proc.gid)
	}
	return b.String()
}

// Unwrap exposes the underlying syscall error to errors.Is/As.
func (e *DataDirError) Unwrap() error { return e.Err }

// CheckDataDir creates the data dir if needed and proves it is writable by
// this process, so an ownership problem fails at startup with an actionable
// message instead of surfacing later as an opaque driver error.
func CheckDataDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		// MkdirAll failed — report against the deepest existing ancestor,
		// which is the directory whose permissions actually blocked us.
		return &DataDirError{
			Dir:     dir,
			Err:     err,
			dirInfo: statOwner(nearestExisting(dir)),
			proc:    procOwner(),
		}
	}
	f, err := os.CreateTemp(dir, ".monarr-write-probe-*")
	if err != nil {
		return &DataDirError{
			Dir:     dir,
			Err:     err,
			dirInfo: statOwner(dir),
			proc:    procOwner(),
		}
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		return &DataDirError{Dir: dir, Err: err, dirInfo: statOwner(dir), proc: procOwner()}
	}
	if err := os.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return &DataDirError{Dir: dir, Err: err, dirInfo: statOwner(dir), proc: procOwner()}
	}
	return nil
}

// nearestExisting walks up from dir to the first path that exists, so the
// error names the directory that actually denied us.
func nearestExisting(dir string) string {
	for p := filepath.Clean(dir); ; {
		if _, err := os.Stat(p); err == nil {
			return p
		}
		parent := filepath.Dir(p)
		if parent == p {
			return p
		}
		p = parent
	}
}

// ownership is the uid/gid/mode triple used in error messages. known is
// false on platforms that do not expose it.
type ownership struct {
	known bool
	uid   int
	gid   int
	mode  fs.FileMode
}
