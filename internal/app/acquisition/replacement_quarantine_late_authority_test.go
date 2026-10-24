package acquisition

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReplacementQuarantineRecoveryRefusesLatePathAuthorityChanges(t *testing.T) {
	for _, scenario := range []string{"foreign_source_reappeared", "parent_directory_replaced", "foreign_source_before_final_unlink"} {
		t.Run(scenario, func(t *testing.T) {
			_, db, ctx, op, reviewed, source := reviewedBadReplacementFixture(t)
			parent := filepath.Dir(source)
			quarantineName := replacementQuarantinePrefix + op.ID + "-" + itoa(reviewed.MediaFileID)
			quarantine := filepath.Join(parent, quarantineName)
			if err := os.Link(source, quarantine); err != nil {
				t.Fatal(err)
			}
			parentInfo, err := os.Stat(parent)
			if err != nil {
				t.Fatal(err)
			}
			quarantineInfo, err := os.Stat(quarantine)
			if err != nil {
				t.Fatal(err)
			}
			sibling := filepath.Join(parent, "unrelated.keep")
			if err := os.WriteFile(sibling, []byte("sibling remains reviewable"), 0o600); err != nil {
				t.Fatal(err)
			}
			foreign := []byte("different object at reviewed source path")

			// The interrupted quarantine is recovered from persisted review data,
			// not the pre-crash service or operation value.
			reopened := reopenLifecycleDB(t, db)
			db = reopened
			svc := New(reopened, nil, nil, nil, nil)
			op, err = reopened.GetReplacementOperation(ctx, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			persistedFiles, err := reopened.ListReplacementFiles(ctx, op.ID)
			if err != nil || len(persistedFiles) != 1 {
				t.Fatalf("restart replacement file review=%+v err=%v", persistedFiles, err)
			}
			reviewed = persistedFiles[0]

			switch scenario {
			case "foreign_source_reappeared":
				if err := os.Remove(source); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(source, foreign, 0o600); err != nil {
					t.Fatal(err)
				}
				err = svc.removeVerifiedReplacementQuarantine(ctx, op, reviewed, parent, reviewed.StorageIdentity,
					".", filepath.Base(source), quarantineName, parentInfo, quarantineInfo)
				if err == nil || !strings.Contains(err.Error(), "source path changed while quarantine recovery was pending") {
					t.Fatalf("recovery accepted a new object at the reviewed source path: %v", err)
				}
			case "parent_directory_replaced":
				heldParent := parent + ".held-reviewed-parent"
				if err := os.Rename(parent, heldParent); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(parent, 0o755); err != nil {
					_ = os.Rename(heldParent, parent)
					t.Fatal(err)
				}
				defer func() {
					_ = os.RemoveAll(parent)
					_ = os.Rename(heldParent, parent)
				}()
				sibling = filepath.Join(heldParent, "unrelated.keep")
				if err := os.WriteFile(filepath.Join(parent, "new-parent.keep"), []byte("replacement parent"), 0o600); err != nil {
					t.Fatal(err)
				}
				err = svc.removeVerifiedReplacementQuarantine(ctx, op, reviewed, parent, reviewed.StorageIdentity,
					".", filepath.Base(source), quarantineName, parentInfo, quarantineInfo)
				if err == nil || !strings.Contains(err.Error(), "quarantine directory changed before unlink") {
					t.Fatalf("recovery accepted a replacement parent directory: %v", err)
				}
				if got, readErr := os.ReadFile(filepath.Join(parent, "new-parent.keep")); readErr != nil || string(got) != "replacement parent" {
					t.Fatalf("replacement parent sentinel changed: %q err=%v", got, readErr)
				}
				if got, readErr := os.ReadFile(filepath.Join(heldParent, quarantineName)); readErr != nil || string(got) != "bytes" {
					t.Fatalf("reviewed quarantine was altered under the held parent: %q err=%v", got, readErr)
				}
			case "foreign_source_before_final_unlink":
				if err := os.Remove(source); err != nil {
					t.Fatal(err)
				}
				if err := verifyReplacementQuarantineDigest(ctx, parent, ".", quarantineName, reviewed, quarantineInfo); err != nil {
					t.Fatalf("pre-finalize quarantine proof: %v", err)
				}
				if err := os.WriteFile(source, foreign, 0o600); err != nil {
					t.Fatal(err)
				}
				err = svc.finalizeReplacementQuarantine(ctx, op, reviewed, parent, reviewed.StorageIdentity,
					".", filepath.Base(source), quarantineName, quarantineInfo)
				if err == nil || !strings.Contains(err.Error(), "source path reappeared before final unlink") {
					t.Fatalf("finalization removed reviewed bytes after a foreign source reappeared: %v", err)
				}
			}

			if scenario != "parent_directory_replaced" {
				if got, err := os.ReadFile(source); err != nil || string(got) != string(foreign) {
					t.Fatalf("late source object was removed or changed: %q err=%v", got, err)
				}
				if got, err := os.ReadFile(quarantine); err != nil || string(got) != "bytes" {
					t.Fatalf("reviewed quarantine was removed or changed: %q err=%v", got, err)
				}
			}
			if got, err := os.ReadFile(sibling); err != nil || string(got) != "sibling remains reviewable" {
				t.Fatalf("unrelated sibling changed during cleanup refusal: %q err=%v", got, err)
			}
			if _, err := db.GetMediaFileCustodyByPath(ctx, source); err != nil {
				t.Fatalf("refused cleanup discarded library ownership: %v", err)
			}
		})
	}
}

func TestReplacementQuarantineDigestChangeAfterReviewRetainsEvidenceAndOwnership(t *testing.T) {
	_, db, ctx, op, reviewed, source := reviewedBadReplacementFixture(t)
	parent := filepath.Dir(source)
	quarantineName := replacementQuarantinePrefix + op.ID + "-" + itoa(reviewed.MediaFileID)
	quarantine := filepath.Join(parent, quarantineName)
	if err := os.Link(source, quarantine); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(quarantine)
	if err != nil {
		t.Fatal(err)
	}
	mutated := bytes.Repeat([]byte("x"), int(reviewed.Size))
	file, err := os.OpenFile(quarantine, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(mutated); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(quarantine, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(mutated, []byte("bytes")) {
		t.Fatal("fixture mutation unexpectedly matches the reviewed payload")
	}

	reopened := reopenLifecycleDB(t, db)
	restarted := New(reopened, nil, nil, nil, nil)
	persistedOp, err := reopened.GetReplacementOperation(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	persistedFiles, err := reopened.ListReplacementFiles(ctx, op.ID)
	if err != nil || len(persistedFiles) != 1 {
		t.Fatalf("restart replacement file review=%+v err=%v", persistedFiles, err)
	}
	parentInfo, err := os.Stat(parent)
	if err != nil {
		t.Fatal(err)
	}
	changedInfo, err := os.Stat(quarantine)
	if err != nil {
		t.Fatal(err)
	}
	err = restarted.removeVerifiedReplacementQuarantine(ctx, persistedOp, persistedFiles[0], parent,
		persistedFiles[0].StorageIdentity, ".", filepath.Base(source), quarantineName, parentInfo, changedInfo)
	if err == nil || !strings.Contains(err.Error(), "replacement quarantine bytes changed after review") {
		t.Fatalf("same-object digest change was accepted during restart recovery: %v", err)
	}
	if _, err := os.Lstat(source); !os.IsNotExist(err) {
		t.Fatalf("source name should remain unlinked at the post-unlink verification boundary: %v", err)
	}
	got, readErr := os.ReadFile(quarantine)
	if readErr != nil || !bytes.Equal(got, mutated) {
		t.Fatalf("recovery removed or changed quarantine evidence: bytes=%q err=%v", got, readErr)
	}
	if _, err := reopened.GetMediaFileCustodyByPath(ctx, source); err != nil {
		t.Fatalf("failed digest proof discarded library ownership: %v", err)
	}
}
