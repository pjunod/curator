package acquisition

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// processIO reads this process's rchar/wchar (bytes passed to read/write
// system calls, cache hits included) from /proc.
func processIO(t *testing.T) (read, written int64) {
	t.Helper()
	f, err := os.Open("/proc/self/io")
	if err != nil {
		t.Skip("no /proc/self/io on this platform")
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ": ")
		if !ok {
			continue
		}
		n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		switch k {
		case "rchar":
			read = n
		case "wchar":
			written = n
		}
	}
	return read, written
}

// Placement copies each source byte once (or hardlinks without reading it).
// Cleanup independently verifies the current source against its manifest,
// adding one required read before unlink. Bound the complete lifecycle to
// those reads so another redundant full-payload read remains a regression.
func TestPublicationImportAndCleanupBoundTheirPayloadReads(t *testing.T) {
	body := string(bytes.Repeat([]byte("monarr-io-"), (16<<20)/10))
	size := int64(len(body))
	for _, tc := range []struct {
		name    string
		library func(t *testing.T) string // "" keeps the fixture's library
	}{
		{name: "same filesystem: a link and one cleanup verification"},
		{name: "another filesystem: copy plus cleanup verification", library: func(t *testing.T) string {
			dir, err := os.MkdirTemp("/dev/shm", "monarr-library-")
			if err != nil {
				t.Skip("no /dev/shm to put the library on another filesystem")
			}
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			return filepath.Join(dir, "Test Show (2020)")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setupV2(t, nil)
			if tc.library != nil {
				lib := tc.library(t)
				var st1, st2 syscall.Stat_t
				if syscall.Stat(filepath.Dir(lib), &st1) != nil || syscall.Stat(f.root, &st2) != nil || st1.Dev == st2.Dev {
					t.Skip("the library is not on another filesystem here")
				}
				if _, err := f.db.W.ExecContext(f.ctx, `UPDATE media_items SET path=? WHERE id=?`, lib, f.item); err != nil {
					t.Fatal(err)
				}
			}
			dl := f.grab(1, epOne)
			pub := f.publish("Test.Show.S01E01.job-501.runner-io", "io", map[string]string{epOne + ".mkv": body})
			source, err := os.Stat(filepath.Join(pub.Path, epOne+".mkv"))
			if err != nil {
				t.Fatal(err)
			}
			r0, w0 := processIO(t)
			f.complete(dl, pub)
			r1, w1 := processIO(t)
			p, files := f.publicationFiles(pub.Path)
			placed := files[epOne+".mkv"]
			if placed.Outcome != "placed" || p.State != "removed" {
				t.Fatalf("publication %s (%s) file %+v, want placed and cleaned up", p.State, p.Error, placed)
			}
			lib, err := os.Stat(placed.LibraryPath)
			if err != nil || lib.Size() != size {
				t.Fatalf("library file %v (%v)", lib, err)
			}
			read, written := r1-r0, w1-w0
			slack := int64(2 << 20) // database pages, logs, a probe's header read
			if tc.library == nil {
				if !os.SameFile(source, lib) {
					t.Fatal("a same-filesystem placement copied instead of linking")
				}
				if read < size || read > size*11/10+slack {
					t.Fatalf("a linked import and cleanup read %d bytes, want one %d-byte verification plus overhead", read, size)
				}
				return
			}
			if written < size {
				t.Fatalf("wrote %d bytes, want the %d-byte copy", written, size)
			}
			if read < size*2 || read > size*21/10+slack {
				t.Fatalf("read %d bytes for a %d-byte import and cleanup (%.2f:1), want copy plus verification at most 2.1:1", read, size, float64(read)/float64(size))
			}
		})
	}
}
