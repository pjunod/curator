//go:build linux

package acquisition

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStorageMountIdentityIgnoresTransientIDsAndBindTargets(t *testing.T) {
	first := "36 25 0:42 /media/library /mnt/library rw,relatime - nfs4 nas:/exports rw\n"
	remounted := "901 800 0:77 /media/library /mnt/library rw,relatime - nfs4 nas:/exports rw\n"
	bindAlias := "902 800 0:77 /media/library /downloads rw,relatime - nfs4 nas:/exports rw\n"
	identity := func(mountinfo, path string) string {
		t.Helper()
		mount, err := storageMountForPath(strings.NewReader(mountinfo), path)
		if err != nil {
			t.Fatal(err)
		}
		return mount.identity()
	}
	base := identity(first, "/mnt/library/movie/file.mkv")
	if got := identity(remounted, "/mnt/library/movie/file.mkv"); got != base {
		t.Fatalf("routine remount changed storage identity: %q != %q", got, base)
	}
	if got := identity(bindAlias, "/downloads/movie/file.mkv"); got != base {
		t.Fatalf("bind alias changed storage identity: %q != %q", got, base)
	}
}

func TestStorageMountIdentityDetectsMissingOrDifferentStorage(t *testing.T) {
	configured := "36 25 0:42 /media/library /mnt/library rw,relatime - nfs4 nas:/exports rw\n"
	localFallback := "29 1 8:1 / / rw,relatime - ext4 /dev/root rw\n"
	wrongMount := "37 25 8:9 / /mnt/library rw,relatime - ext4 /dev/replacement rw\n"
	for name, mountinfo := range map[string]string{"missing mount": localFallback, "different source": wrongMount} {
		t.Run(name, func(t *testing.T) {
			observed, err := storageMountForPath(strings.NewReader(mountinfo), "/mnt/library/movie")
			if err != nil {
				t.Fatal(err)
			}
			want, err := storageMountForPath(strings.NewReader(configured), "/mnt/library/movie")
			if err != nil {
				t.Fatal(err)
			}
			if observed.identity() == want.identity() {
				t.Fatalf("unexpectedly accepted %s", name)
			}
		})
	}
}

func TestStorageMountParserChoosesDeepestMountAndUnescapesFields(t *testing.T) {
	mountinfo := "1 0 8:1 / / rw - ext4 /dev/root rw\n" +
		"2 1 0:42 / /mnt/media\\040archive rw - nfs4 nas:/exports\\040one rw\n"
	mount, err := storageMountForPath(strings.NewReader(mountinfo), "/mnt/media archive/movie")
	if err != nil {
		t.Fatal(err)
	}
	if mount.mountpoint != "/mnt/media archive" || mount.root != "/" || mount.source != "nas:/exports one" {
		t.Fatalf("mount fields were not selected and decoded: %+v", mount)
	}
}

func TestStorageMountParserUsesVisibleMountIDForStackedMounts(t *testing.T) {
	mountinfo := "40 25 0:42 /first /mnt/library rw - nfs4 nas:/hidden rw\n" +
		"41 25 0:43 /second /mnt/library rw - nfs4 nas:/visible rw\n"
	mount, err := storageMountForPathMountID(strings.NewReader(mountinfo), "/mnt/library/movie", 41)
	if err != nil {
		t.Fatal(err)
	}
	if mount.source != "nas:/visible" {
		t.Fatalf("selected hidden stacked mount: %+v", mount)
	}
	if _, err := storageMountForPath(strings.NewReader(mountinfo), "/mnt/library/movie"); err == nil {
		t.Fatal("ambiguous stacked mount passed without a visible mount ID")
	}
}

func TestExpectedStorageIdentityRequiresAnExistingDirectory(t *testing.T) {
	root := t.TempDir()
	if _, err := expectedStorageIdentity(root); err != nil {
		t.Fatalf("existing directory did not resolve to storage: %v", err)
	}
	missing := filepath.Join(root, "missing")
	if err := os.Mkdir(missing, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(missing); err != nil {
		t.Fatal(err)
	}
	if _, err := expectedStorageIdentity(missing); err == nil {
		t.Fatal("absent root resolved to its ancestor mount")
	}
}
