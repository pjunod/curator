package acquisition

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pjunod/monarr/internal/ports"
)

func TestAnchoredDeliveryPathReadersRejectTraversalAndSymlinkComponents(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "safe"), 0o755); err != nil {
		t.Fatal(err)
	}
	regular := filepath.Join(rootPath, "safe", "video.mkv")
	if err := os.WriteFile(regular, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "outside.mkv"), []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(rootPath, "jump")); err != nil {
		t.Fatalf("create test symlink: %v", err)
	}
	base, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = base.Close() }()

	for _, rel := range []string{"../outside", filepath.Join("jump", ".")} {
		if got, err := openDeliveryDirectoryNoSymlinks(base, rel); err == nil {
			_ = got.Close()
			t.Errorf("directory path %q unexpectedly opened", rel)
		}
	}
	parent, file, named, err := openDeliveryFileNoSymlinks(base, filepath.Join("safe", "video.mkv"))
	if err != nil {
		t.Fatalf("valid anchored file did not open: %v", err)
	}
	if !named.Mode().IsRegular() {
		t.Errorf("anchored file metadata mode=%v", named.Mode())
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{
		filepath.Join("jump", "outside.mkv"),
		filepath.Join("safe", "missing.mkv"),
		"safe",
	} {
		parent, file, _, err := openDeliveryFileNoSymlinks(base, rel)
		if err == nil {
			_ = file.Close()
			_ = parent.Close()
			t.Errorf("unsafe/non-file path %q unexpectedly opened", rel)
		}
	}
}

var _ ports.DeliveryProtocol = (*deliveryImportClient)(nil)
