package acquisition

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPlaceMakesFilesReadable is the regression for files that imported
// "successfully" and then would not play.
//
// The copy path uses os.CreateTemp, which creates mode 0600 — owner only. A
// media server running as a different user (which is the normal arrangement)
// could not open them, and neither could monarr's own prober on a later pass.
// Nothing reported an error at any point.
func TestPlaceMakesFilesReadable(t *testing.T) {
	t.Run("copied across filesystems", func(t *testing.T) {
		src := filepath.Join(t.TempDir(), "source.mkv")
		if err := os.WriteFile(src, []byte("payload"), 0o600); err != nil {
			t.Fatal(err)
		}
		dest := filepath.Join(t.TempDir(), "Show", "Season 1", "out.mkv")
		// Force the copy path by making the link fail: a directory cannot be
		// hard-linked to, and os.Link across these temp dirs may still succeed,
		// so assert on the final mode either way.
		if err := place(src, dest); err != nil {
			t.Fatal(err)
		}
		assertMode(t, dest, 0o644)
	})

	t.Run("folders monarr creates are traversable", func(t *testing.T) {
		src := filepath.Join(t.TempDir(), "source.mkv")
		_ = os.WriteFile(src, []byte("payload"), 0o600)
		root := t.TempDir()
		dest := filepath.Join(root, "Show (2020)", "Season 1", "out.mkv")
		if err := place(src, dest); err != nil {
			t.Fatal(err)
		}
		assertMode(t, filepath.Join(root, "Show (2020)"), 0o755)
		assertMode(t, filepath.Join(root, "Show (2020)", "Season 1"), 0o755)
	})

	t.Run("re-placing an existing file still fixes its mode", func(t *testing.T) {
		src := filepath.Join(t.TempDir(), "source.mkv")
		_ = os.WriteFile(src, []byte("payload"), 0o600)
		dest := filepath.Join(t.TempDir(), "out.mkv")
		if err := os.WriteFile(dest, []byte("already here"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := place(src, dest); err != nil {
			t.Fatal(err)
		}
		assertMode(t, dest, 0o644)
	})

	t.Run("the mode is configurable", func(t *testing.T) {
		t.Setenv(FileModeEnv, "0640")
		src := filepath.Join(t.TempDir(), "source.mkv")
		_ = os.WriteFile(src, []byte("payload"), 0o600)
		dest := filepath.Join(t.TempDir(), "out.mkv")
		if err := place(src, dest); err != nil {
			t.Fatal(err)
		}
		assertMode(t, dest, 0o640)
	})

	t.Run("a nonsense override never produces an unreadable file", func(t *testing.T) {
		t.Setenv(FileModeEnv, "not-a-mode")
		if got := fileMode(); got != DefaultFileMode {
			t.Errorf("fileMode = %o, want the %o default", got, DefaultFileMode)
		}
		t.Setenv(FileModeEnv, "7777")
		if got := fileMode(); got != DefaultFileMode {
			t.Errorf("out-of-range mode accepted: %o", got)
		}
	})
}

// TestMissingDirsOnlyNamesWhatWillBeCreated pins the guard that keeps the
// chmod from walking up into folders monarr does not own — a root folder, or
// the filesystem root.
func TestMissingDirsOnlyNamesWhatWillBeCreated(t *testing.T) {
	root := t.TempDir()
	existing := filepath.Join(root, "already")
	if err := os.MkdirAll(existing, 0o700); err != nil {
		t.Fatal(err)
	}
	got := missingDirs(filepath.Join(existing, "a", "b"))
	want := []string{filepath.Join(existing, "a"), filepath.Join(existing, "a", "b")}
	if len(got) != len(want) {
		t.Fatalf("missingDirs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("missingDirs = %v, want %v (outermost first)", got, want)
		}
	}
	// The pre-existing folder keeps its own permissions: not ours to change.
	info, err := os.Stat(existing)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("an existing folder was chmodded to %o", info.Mode().Perm())
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != want {
		t.Errorf("%s is mode %o, want %o", filepath.Base(path), info.Mode().Perm(), want)
	}
}

// TestPlaceReplacesAStrangerAtTheDestination is the bug that made re-grabbing
// look like it did nothing.
//
// The destination name is rendered from the quality, so replacing a bad
// "Bluray 2160p" with a good "Bluray 2160p" produces the identical path.
// place() used to see a file there, conclude "already imported", and copy
// nothing — while removeExistingFiles skipped the same file as the one being
// kept. Nothing was replaced, and because the row is written from
// sizeOf(dest), the library reported the NEW release at the OLD file's size.
// A user watching a 500 MB runt refuse to become a 20 GB remux, however many
// times they grabbed it, was watching this.
func TestPlaceReplacesAStrangerAtTheDestination(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "lib", "Movie (2025) [Bluray 2160p].mkv")

	runt := filepath.Join(dir, "runt.mkv")
	if err := os.WriteFile(runt, bytes.Repeat([]byte{0xAA}, 512), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := place(runt, dest); err != nil {
		t.Fatalf("first place: %v", err)
	}

	real := filepath.Join(dir, "real.mkv")
	want := bytes.Repeat([]byte{0xBB}, 4096)
	if err := os.WriteFile(real, want, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := place(real, dest); err != nil {
		t.Fatalf("second place: %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("destination still holds %d bytes of the old file; want the %d-byte replacement",
			len(got), len(want))
	}

	// And no temp or link scaffolding is left lying in the library folder.
	entries, err := os.ReadDir(filepath.Dir(dest))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".monarr-") {
			t.Errorf("left %q behind in the library folder", e.Name())
		}
	}
}

// Re-placing the very same file is still a no-op, which is what keeps a
// retried import from re-copying 20 GB for nothing.
func TestPlaceIsIdempotentForTheSameFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(src, bytes.Repeat([]byte{0xCC}, 2048), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "lib", "Movie (2025) [Bluray 2160p].mkv")
	if err := place(src, dest); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if err := place(src, dest); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Error("re-placing the same file replaced it; a retried import should not re-copy")
	}
}
