package acquisition

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pjunod/monarr/internal/infra/sqlite"
)

func TestActivityPriorRecoveryPreservesUnprovenAndReplacedObjects(t *testing.T) {
	for _, hazard := range []string{"unbound backup", "different backup path", "changed drive", "changed parent", "missing backup", "symlink backup", "changed bytes", "unrelated target"} {
		t.Run(hazard, func(t *testing.T) {
			parent := t.TempDir()
			target := filepath.Join(parent, "library.mkv")
			backup := writeFile(t, filepath.Join(parent, ".monarr-prior-owned-placement"), "prior Library bytes")
			digest, _, err := fileDigest(t.Context(), backup)
			if err != nil {
				t.Fatal(err)
			}
			storage, identity, err := placementParentIdentity(target)
			if err != nil {
				t.Fatal(err)
			}
			p := sqlite.Placement{ID: "owned-placement", Target: target, Backup: backup, PreviousSHA256: digest, StorageIdentity: storage, TargetParentIdentity: identity}
			switch hazard {
			case "unbound backup":
				p.PreviousSHA256 = ""
			case "different backup path":
				p.ID = "another-placement"
			case "changed drive":
				p.StorageIdentity = "different-volume"
			case "changed parent":
				p.TargetParentIdentity = "different-directory"
			case "missing backup":
				if err := os.Remove(backup); err != nil {
					t.Fatal(err)
				}
			case "symlink backup":
				replacement := writeFile(t, filepath.Join(parent, "unrelated"), "prior Library bytes")
				if err := os.Remove(backup); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(replacement, backup); err != nil {
					t.Fatal(err)
				}
			case "changed bytes":
				writeFile(t, backup, "unrelated bytes of a different file")
			case "unrelated target":
				writeFile(t, target, "new independent Library file")
			}
			before, statErr := os.Lstat(backup)
			if err := (&Service{}).restoreActivityPrior(t.Context(), p); err == nil {
				t.Fatal("unproven prior file was accepted")
			}
			if statErr == nil {
				after, err := os.Lstat(backup)
				if err != nil || !os.SameFile(before, after) {
					t.Fatalf("protected backup changed: %v", err)
				}
			}
			if hazard == "unrelated target" {
				got, err := os.ReadFile(target)
				if err != nil || string(got) != "new independent Library file" {
					t.Fatalf("independent Library target changed: %q, %v", got, err)
				}
			} else if _, err := os.Lstat(target); !os.IsNotExist(err) {
				t.Fatalf("unsafe target was installed: %v", err)
			}
		})
	}
}
