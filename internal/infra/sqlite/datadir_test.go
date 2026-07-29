package sqlite

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckDataDirCreatesAndProbes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "data")
	if err := CheckDataDir(dir); err != nil {
		t.Fatalf("CheckDataDir: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("data dir not created: %v", err)
	}
	// The probe must not survive.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".monarr-write-probe") {
			t.Errorf("write probe left behind: %s", e.Name())
		}
	}
}

// TestCheckDataDirUnwritable is the regression for the root-owned bind mount:
// the directory exists but this process cannot create files in it. The error
// must name the directory and both ownerships rather than surfacing as an
// opaque driver failure later.
func TestCheckDataDirUnwritable(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	err := CheckDataDir(dir)
	if err == nil {
		t.Fatal("expected an error for an unwritable data dir")
	}
	var dde *DataDirError
	if !errors.As(err, &dde) {
		t.Fatalf("error is not *DataDirError: %T", err)
	}
	msg := err.Error()
	for _, want := range []string{dir, "not writable", "uid"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
	}
}

// TestOpenReportsUnwritableDataDir proves the check is wired into Open, which
// is the path an operator actually hits at startup.
func TestOpenReportsUnwritableDataDir(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	db, err := Open(dir)
	if err == nil {
		_ = db.Close()
		t.Fatal("expected Open to fail on an unwritable data dir")
	}
	var dde *DataDirError
	if !errors.As(err, &dde) {
		t.Fatalf("Open error is not *DataDirError: %T (%v)", err, err)
	}
}

// The bind-mount diagnosis is the whole reason DataDirError exists, and the
// tests above skip under root — which is exactly how CI runs. These build
// the error value directly so the message is checked on every platform and
// every uid.
//
// The failure this prevents: someone edits the message, drops the chown
// hint or the uid, and the operator is back to an opaque driver error with
// nothing to act on.
func TestSqlite2DataDirErrorMessage(t *testing.T) {
	base := errors.New("permission denied")

	cases := []struct {
		name    string
		err     *DataDirError
		want    []string
		wantNot []string
	}{
		{
			// Root-owned dir, non-root process: the shape that earns the
			// long explanation and the chown command.
			name: "bind mount diagnosis",
			err: &DataDirError{
				Dir:     "/data",
				Err:     base,
				dirInfo: ownership{known: true, uid: 0, gid: 0, mode: 0o755},
				proc:    ownership{known: true, uid: 1000, gid: 1000},
			},
			want: []string{
				"/data is not writable",
				"by uid 1000 gid 1000",
				"owned by uid 0 gid 0",
				"permission denied",
				"Docker bind mount",
				"sudo install -d -o 1000 -g 1000",
				"the compose default is " + composeDefaultDataDir,
			},
		},
		{
			// Already the compose default: naming it again as a hint would
			// just be noise.
			name: "compose default omits the hint",
			err: &DataDirError{
				Dir:     composeDefaultDataDir,
				Err:     base,
				dirInfo: ownership{known: true, uid: 0, gid: 0, mode: 0o755},
				proc:    ownership{known: true, uid: 1000, gid: 1000},
			},
			want:    []string{"Docker bind mount", composeDefaultDataDir},
			wantNot: []string{"the compose default is"},
		},
		{
			// Running as root against a root-owned dir is some other
			// problem — guessing "bind mount" would send the operator to
			// chown a directory that is already correct.
			name: "root process gets no diagnosis",
			err: &DataDirError{
				Dir:     "/data",
				Err:     base,
				dirInfo: ownership{known: true, uid: 0, gid: 0, mode: 0o755},
				proc:    ownership{known: true, uid: 0, gid: 0},
			},
			want:    []string{"not writable", "permission denied"},
			wantNot: []string{"Docker bind mount"},
		},
		{
			// Nothing could be stat'd: still a usable message, just without
			// the ownership detail.
			name:    "unknown ownership",
			err:     &DataDirError{Dir: "/data", Err: base},
			want:    []string{"sqlite: data dir /data is not writable: permission denied"},
			wantNot: []string{"uid", "owned by", "Docker bind mount"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := tc.err.Error()
			for _, w := range tc.want {
				if !strings.Contains(msg, w) {
					t.Errorf("message missing %q:\n%s", w, msg)
				}
			}
			for _, w := range tc.wantNot {
				if strings.Contains(msg, w) {
					t.Errorf("message unexpectedly contains %q:\n%s", w, msg)
				}
			}
			// errors.Is has to reach the syscall error, because callers
			// match on fs.ErrPermission rather than on this type.
			if !errors.Is(tc.err, base) {
				t.Error("Unwrap did not expose the underlying error")
			}
		})
	}
}

// When MkdirAll itself fails there is no directory to stat, so the error has
// to report against the deepest ancestor that does exist — that is the one
// whose permissions blocked us. A path whose parent is a regular file fails
// this way for root too, so this runs everywhere.
func TestSqlite2CheckDataDirMkdirAllFails(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(blocker, "data", "deeper")

	err := CheckDataDir(dir)
	if err == nil {
		t.Fatal("expected CheckDataDir to fail when a path component is a file")
	}
	var dde *DataDirError
	if !errors.As(err, &dde) {
		t.Fatalf("error is not *DataDirError: %T (%v)", err, err)
	}
	if dde.Dir != dir {
		t.Errorf("Dir = %q, want the requested dir %q", dde.Dir, dir)
	}
	// nearestExisting walked up to the blocking file and stat'd it, so the
	// message can name an owner.
	if !dde.dirInfo.known {
		t.Error("dirInfo unknown; nearestExisting found nothing to stat")
	}
	if !dde.proc.known {
		t.Error("proc ownership unknown on a platform that exposes it")
	}
}

// nearestExisting must terminate at the filesystem root rather than looping
// when nothing on the path exists.
func TestSqlite2NearestExistingWalksUp(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "a", "b", "c")
	if got := nearestExisting(deep); got != root {
		t.Errorf("nearestExisting(%q) = %q, want %q", deep, got, root)
	}
	if got := nearestExisting(root); got != root {
		t.Errorf("nearestExisting on an existing dir = %q, want %q", got, root)
	}
}

// statOwner has to degrade to "unknown" rather than reporting a bogus uid
// when the path is gone — the error message is built from whatever it
// returns.
func TestSqlite2StatOwnerMissingPath(t *testing.T) {
	if got := statOwner(filepath.Join(t.TempDir(), "nope")); got.known {
		t.Errorf("statOwner on a missing path = %+v, want unknown", got)
	}
	if got := statOwner(t.TempDir()); !got.known {
		t.Error("statOwner on a real directory reported unknown")
	}
}
