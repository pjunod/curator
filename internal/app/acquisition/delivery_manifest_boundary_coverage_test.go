package acquisition

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

func TestDeliveryImportRejectsMalformedManifestBeforeAckOrLibraryMutation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func([]ports.RunnerFile) ([]ports.RunnerFile, string, string, string)
	}{
		{
			name: "empty manifest",
			mutate: func([]ports.RunnerFile) ([]ports.RunnerFile, string, string, string) {
				return nil, "", "", ""
			},
		},
		{
			name: "oversized manifest",
			mutate: func(files []ports.RunnerFile) ([]ports.RunnerFile, string, string, string) {
				return make([]ports.RunnerFile, deliveryMaxFiles+1), "", "", ""
			},
		},
		{
			name: "malformed manifest digest",
			mutate: func(files []ports.RunnerFile) ([]ports.RunnerFile, string, string, string) {
				return files, "not-a-sha256", "", ""
			},
		},
		{
			name: "relative publication path",
			mutate: func(files []ports.RunnerFile) ([]ports.RunnerFile, string, string, string) {
				digest, _ := runnerManifestDigest(files)
				return files, digest, "relative/publication", ""
			},
		},
		{
			name: "relative mapped path",
			mutate: func(files []ports.RunnerFile) ([]ports.RunnerFile, string, string, string) {
				digest, _ := runnerManifestDigest(files)
				return files, digest, "", "relative/mapped"
			},
		},
		{
			name: "unsafe relative member",
			mutate: func(files []ports.RunnerFile) ([]ports.RunnerFile, string, string, string) {
				files[0].Path = "../escape.mkv"
				digest, _ := runnerManifestDigest(files)
				return files, digest, "", ""
			},
		},
		{
			name: "duplicate member",
			mutate: func(files []ports.RunnerFile) ([]ports.RunnerFile, string, string, string) {
				files = append(files, files[0])
				digest, _ := runnerManifestDigest(files)
				return files, digest, "", ""
			},
		},
		{
			name: "overlong member path",
			mutate: func(files []ports.RunnerFile) ([]ports.RunnerFile, string, string, string) {
				files[0].Path = strings.Repeat("x", deliveryMaxPathBytes+1)
				digest, _ := runnerManifestDigest(files)
				return files, digest, "", ""
			},
		},
		{
			name: "negative size",
			mutate: func(files []ports.RunnerFile) ([]ports.RunnerFile, string, string, string) {
				files[0].SizeBytes = -1
				digest, _ := runnerManifestDigest(files)
				return files, digest, "", ""
			},
		},
		{
			name: "total size bound",
			mutate: func(_ []ports.RunnerFile) ([]ports.RunnerFile, string, string, string) {
				files := []ports.RunnerFile{
					{Path: "one.mkv", SizeBytes: deliveryMaxTotalBytes, SHA256: sha256Hex([]byte("one"))},
					{Path: "two.mkv", SizeBytes: 1, SHA256: sha256Hex([]byte("x"))},
				}
				digest, _ := runnerManifestDigest(files)
				return files, digest, "", ""
			},
		},
		{
			name: "malformed member sha256",
			mutate: func(files []ports.RunnerFile) ([]ports.RunnerFile, string, string, string) {
				files[0].SHA256 = "xyz"
				digest, _ := runnerManifestDigest(files)
				return files, digest, "", ""
			},
		},
		{
			name: "canonical digest mismatch",
			mutate: func(files []ports.RunnerFile) ([]ports.RunnerFile, string, string, string) {
				files[0].Path = "changed.mkv"
				return files, sha256Hex([]byte("not the canonical manifest")), "", ""
			},
		},
		{
			name: "missing manifest member",
			mutate: func(files []ports.RunnerFile) ([]ports.RunnerFile, string, string, string) {
				files[0].Path = "missing.mkv"
				digest, _ := runnerManifestDigest(files)
				return files, digest, "", ""
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			protocol := &deliveryImportClient{}
			svc, db, dl, cfg, publication := deliveryImportFixture(t, protocol)
			receiptID := receiptIDFor(t, db, dl.ID)
			original, err := db.DeliveryReceiptByID(ctx, receiptID)
			if err != nil {
				t.Fatal(err)
			}
			files, manifestDigest, publicationPath, mappedPath := tc.mutate(append([]ports.RunnerFile(nil), original.Delivery.Files...))
			if tc.name == "oversized manifest" {
				// Count is the first guard; populate valid entries so the failure
				// cannot accidentally depend on malformed zero-value members.
				files = make([]ports.RunnerFile, deliveryMaxFiles+1)
				for i := range files {
					files[i] = ports.RunnerFile{Path: "item-" + itoa(int64(i)) + ".mkv", SizeBytes: 1, SHA256: sha256Hex([]byte("x"))}
				}
				manifestDigest, _ = runnerManifestDigest(files)
			}
			filesJSON, err := json.Marshal(files)
			if err != nil {
				t.Fatal(err)
			}
			_, err = db.W.ExecContext(ctx, `UPDATE runner_delivery_receipts SET files_json=?,manifest_digest=?,publication_path=CASE WHEN ?='' THEN publication_path ELSE ? END,mapped_path=CASE WHEN ?='' THEN mapped_path ELSE ? END WHERE id=?`, string(filesJSON), manifestDigest, publicationPath, publicationPath, mappedPath, mappedPath, receiptID)
			if err != nil {
				t.Fatal(err)
			}
			if err := svc.runDeliveryImport(ctx, dl, cfg, receiptID); err == nil {
				t.Fatal("malformed accepted manifest reached import")
			}
			if protocol.accepts != 0 {
				t.Fatalf("invalid manifest was acknowledged %d times", protocol.accepts)
			}
			current, err := db.DeliveryReceiptByID(ctx, receiptID)
			if err != nil || current.AckState == "accepted" || current.State == "imported" {
				t.Fatalf("invalid manifest changed durable acceptance/import state: %+v err=%v", current, err)
			}
			libraryFiles, err := db.ListFilesForItem(ctx, dl.MediaItemID)
			if err != nil || len(libraryFiles) != 0 {
				t.Fatalf("invalid manifest mutated library inventory: files=%+v err=%v", libraryFiles, err)
			}
			if _, err := os.Stat(filepath.Join(publication, epRelease+".mkv")); err != nil {
				t.Fatalf("invalid manifest cleanup removed its original payload: %v", err)
			}
		})
	}
}

func TestDeliveryManifestRecheckRejectsPostVerificationFilesystemChanges(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, publication, source string)
	}{
		{
			name: "member missing",
			mutate: func(t *testing.T, _, source string) {
				t.Helper()
				if err := os.Remove(source); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "member replaced by symlink",
			mutate: func(t *testing.T, _, source string) {
				t.Helper()
				held := source + ".held"
				if err := os.Rename(source, held); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Remove(held) })
				if err := os.Symlink(held, source); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "member replaced by directory",
			mutate: func(t *testing.T, _, source string) {
				t.Helper()
				held := source + ".held"
				if err := os.Rename(source, held); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Remove(held) })
				if err := os.Mkdir(source, 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "same-size bytes changed",
			mutate: func(t *testing.T, _, source string) {
				t.Helper()
				if err := os.WriteFile(source, []byte("VIDEO"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "member size changed",
			mutate: func(t *testing.T, _, source string) {
				t.Helper()
				if err := os.WriteFile(source, []byte("videos"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "same-content new generation",
			mutate: func(t *testing.T, _, source string) {
				t.Helper()
				held := source + ".held"
				if err := os.Rename(source, held); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Remove(held) })
				if err := os.WriteFile(source, []byte("video"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "publication directory replaced",
			mutate: func(t *testing.T, publication, _ string) {
				t.Helper()
				held := publication + ".held"
				if err := os.Rename(publication, held); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.RemoveAll(held) })
				if err := os.Mkdir(publication, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(publication, epRelease+".mkv"), []byte("video"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			svc, db, dl, cfg, publication := deliveryImportFixture(t, &deliveryImportClient{})
			receipt, err := db.DeliveryReceiptByID(ctx, receiptIDFor(t, db, dl.ID))
			if err != nil {
				t.Fatal(err)
			}
			verified, err := svc.verifyDeliveryManifest(ctx, cfg, receipt)
			if err != nil || len(verified) != 1 {
				t.Fatalf("verify valid accepted manifest: files=%+v err=%v", verified, err)
			}
			source := filepath.Join(publication, epRelease+".mkv")
			tc.mutate(t, publication, source)
			if err := svc.recheckDeliveryFiles(ctx, receipt, verified); err == nil {
				t.Fatal("post-verification filesystem change crossed the import boundary")
			}
			if files, err := db.ListFilesForItem(ctx, dl.MediaItemID); err != nil || len(files) != 0 {
				t.Fatalf("recheck failure mutated library files: files=%+v err=%v", files, err)
			}
		})
	}
}

func TestDeliveryImportRechecksAcceptedFilesBeforePlacement(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{acceptEntered: make(chan struct{}, 1), acceptRelease: make(chan struct{})}
	svc, db, dl, cfg, publication := deliveryImportFixture(t, protocol)
	receiptID := receiptIDFor(t, db, dl.ID)
	finished := make(chan error, 1)
	go func() { finished <- svc.runDeliveryImport(ctx, dl, cfg, receiptID) }()
	released, completed := false, false
	defer func() {
		if !released {
			close(protocol.acceptRelease)
		}
		if !completed {
			<-finished
		}
	}()
	select {
	case <-protocol.acceptEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("import never reached the post-verification Runner acceptance boundary")
	}
	source := filepath.Join(publication, epRelease+".mkv")
	held := source + ".verified-original"
	if err := os.Rename(source, held); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	close(protocol.acceptRelease)
	released = true
	importErr := <-finished
	completed = true
	if importErr == nil || !strings.Contains(importErr.Error(), "changed after verification") {
		t.Fatalf("import accepted a same-content new file generation after manifest verification: %v", importErr)
	}
	if files, err := db.ListFilesForItem(ctx, dl.MediaItemID); err != nil || len(files) != 0 {
		t.Fatalf("post-acceptance identity change placed library media: files=%+v err=%v", files, err)
	}
	if got, err := os.ReadFile(source); err != nil || string(got) != "video" {
		t.Fatalf("changed source generation was modified during refusal: %q err=%v", got, err)
	}
	if got, err := os.ReadFile(held); err != nil || string(got) != "video" {
		t.Fatalf("original verified source bytes were lost during refusal: %q err=%v", got, err)
	}
	receipt, err := db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil || receipt.State == "imported" {
		t.Fatalf("post-verification mutation did not remain pending review: %+v err=%v", receipt, err)
	}
}

func TestVerifyDeliveryManifestRejectsNonUTF8Path(t *testing.T) {
	ctx := context.Background()
	svc, db, dl, cfg, _ := deliveryImportFixture(t, &deliveryImportClient{})
	receipt, err := db.DeliveryReceiptByID(ctx, receiptIDFor(t, db, dl.ID))
	if err != nil {
		t.Fatal(err)
	}
	receipt.Delivery.Files[0].Path = string([]byte{0xff, '.', 'm', 'k', 'v'})
	receipt.Delivery.ManifestDigest = strings.Repeat("a", 64)
	if _, err := svc.verifyDeliveryManifest(ctx, cfg, receipt); err == nil || !strings.Contains(err.Error(), "unsafe relative path") {
		t.Fatalf("invalid UTF-8 manifest path crossed verification: %v", err)
	}
}

func TestVerifyDeliveryManifestRejectsUntrustedFilesystemMembers(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, source string)
		pin    bool
	}{
		{
			name: "member absent",
			mutate: func(t *testing.T, source string) {
				t.Helper()
				if err := os.Remove(source); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "member symlink",
			mutate: func(t *testing.T, source string) {
				t.Helper()
				held := source + ".verified-original"
				if err := os.Rename(source, held); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Remove(held) })
				if err := os.Symlink(held, source); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "member directory",
			mutate: func(t *testing.T, source string) {
				t.Helper()
				held := source + ".verified-original"
				if err := os.Rename(source, held); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Remove(held) })
				if err := os.Mkdir(source, 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "manifest size changed",
			mutate: func(t *testing.T, source string) {
				t.Helper()
				if err := os.WriteFile(source, []byte("videos"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "manifest content changed",
			mutate: func(t *testing.T, source string) {
				t.Helper()
				if err := os.WriteFile(source, []byte("VIDEO"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "previously observed generation replaced",
			pin:  true,
			mutate: func(t *testing.T, source string) {
				t.Helper()
				held := source + ".verified-original"
				if err := os.Rename(source, held); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Remove(held) })
				if err := os.WriteFile(source, []byte("video"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			svc, db, dl, cfg, publication := deliveryImportFixture(t, &deliveryImportClient{})
			receipt, err := db.DeliveryReceiptByID(ctx, receiptIDFor(t, db, dl.ID))
			if err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(publication, epRelease+".mkv")
			if tc.pin {
				initial, err := svc.verifyDeliveryManifest(ctx, cfg, receipt)
				if err != nil || len(initial) != 1 {
					t.Fatalf("initial verification failed: files=%+v err=%v", initial, err)
				}
				manifest := []sqlite.DeliveryManifestFile{{Path: initial[0].relative, SizeBytes: initial[0].size, SHA256: initial[0].digest, FileGeneration: initial[0].generation}}
				if err := db.EnsureDeliveryFileOutcomes(ctx, receipt.ID, manifest, nil, false); err != nil {
					t.Fatal(err)
				}
			}
			tc.mutate(t, source)
			if _, err := svc.verifyDeliveryManifest(ctx, cfg, receipt); err == nil {
				t.Fatal("untrusted filesystem member crossed manifest verification")
			}
			if files, err := db.ListFilesForItem(ctx, dl.MediaItemID); err != nil || len(files) != 0 {
				t.Fatalf("manifest verification failure mutated library inventory: files=%+v err=%v", files, err)
			}
		})
	}
}
