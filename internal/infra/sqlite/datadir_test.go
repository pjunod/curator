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
