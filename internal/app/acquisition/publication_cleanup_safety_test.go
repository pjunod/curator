package acquisition

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

func TestPublicationManifestPersistenceFailureStopsImport(t *testing.T) {
	f := setupV2(t, nil)
	dl := f.grab(1, epOne)
	pub := f.publish("Test.Show.S01E01.job-401.runner-persist", "persist", map[string]string{epOne + ".mkv": "video"})
	id, err := f.db.InsertPublication(f.ctx, sqlite.PublicationWrite{DownloadID: dl.ID, Generation: pub.Generation, Kind: "completed", Path: pub.Path, Cohort: "live"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.W.ExecContext(f.ctx, `CREATE TRIGGER fail_manifest_record BEFORE UPDATE OF manifest_source ON publications WHEN NEW.manifest_source='file' BEGIN SELECT RAISE(FAIL,'injected manifest persistence failure'); END`); err != nil {
		t.Fatal(err)
	}
	err = f.svc.runPublicationImport(f.ctx, dl, f.cfg, id, false)
	if err == nil || !strings.Contains(err.Error(), "injected manifest persistence failure") {
		t.Fatalf("import returned %v, want provenance error", err)
	}
	p, files := f.publicationFiles(pub.Path)
	if p.State != "discovered" || !p.Held() || p.ManifestSource != "none" || len(files) != 0 || f.count(`SELECT COUNT(*) FROM media_files`) != 0 || !exists(filepath.Join(pub.Path, epOne+".mkv")) {
		t.Fatalf("failed provenance import placed or seeded files: publication %+v files %+v", p, files)
	}
	// Ending a publication is another path that can seed/discard files. It
	// must also stop at the same failed evidence write.
	f.svc.endPublication(f.ctx, dl, f.cfg, p, nil, "payload cannot import")
	p, files = f.publicationFiles(pub.Path)
	if p.State != "discovered" || !p.Held() || len(files) != 0 || !exists(filepath.Join(pub.Path, epOne+".mkv")) {
		t.Fatalf("ending bypassed provenance failure: publication %+v files %+v", p, files)
	}
	// Once storage recovers, the ordinary import can record its provenance
	// and finish without a new outcome or special recovery mechanism.
	if _, err := f.db.W.ExecContext(f.ctx, `DROP TRIGGER fail_manifest_record`); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.runPublicationImport(f.ctx, f.row(dl.ID), f.cfg, id, false); err != nil {
		t.Fatal(err)
	}
	p, _ = f.publicationFiles(pub.Path)
	if p.ManifestSource != "file" || f.count(`SELECT COUNT(*) FROM media_files`) != 1 {
		t.Fatalf("recovered import: %+v", p)
	}
}

func TestPublicationCleanupMissingManifestKeepsOnlySource(t *testing.T) {
	for _, source := range []string{"none", "file", "v1_record"} {
		t.Run(source, func(t *testing.T) {
			f := setupV2(t, nil)
			pub := f.importHeld("Test.Show.S01E01.job-402.runner-missing", "missing", map[string]string{epOne + ".mkv": "only good copy"})
			p, files := f.publicationFiles(pub.Path)
			if _, err := f.db.W.ExecContext(f.ctx, `UPDATE publications SET manifest_source=?,manifest_json='{}' WHERE id=?`, source, p.ID); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(files[epOne+".mkv"].LibraryPath); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(pub.Path, ports.ManifestFileName)); err != nil {
				t.Fatal(err)
			}
			f.sweep()
			p, files = f.publicationFiles(pub.Path)
			pf := files[epOne+".mkv"]
			if p.Error == "" || pf.Outcome != "placed" || !pf.RemovedAt.IsZero() || !exists(filepath.Join(pub.Path, pf.Path)) {
				t.Fatalf("cleanup without manifest destroyed evidence: publication %+v file %+v", p, pf)
			}
		})
	}
}

func TestPublicationCleanupLegacyManifestStillNeedsApproval(t *testing.T) {
	f := setupV2(t, nil)
	pub := f.importHeld("Test.Show.S01E01.job-403.runner-legacy", "legacy", map[string]string{epOne + ".mkv": "video"})
	p, _ := f.publicationFiles(pub.Path)
	raw, err := os.ReadFile(filepath.Join(pub.Path, ports.ManifestFileName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.W.ExecContext(f.ctx, `UPDATE publications SET manifest_source='v1_record',manifest_json=?,cohort='historical',cleanup_approved_at=0 WHERE id=?`, string(raw), p.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(pub.Path, ports.ManifestFileName)); err != nil {
		t.Fatal(err)
	}
	f.sweep()
	p, files := f.publicationFiles(pub.Path)
	if !files[epOne+".mkv"].RemovedAt.IsZero() || !exists(filepath.Join(pub.Path, epOne+".mkv")) {
		t.Fatal("legacy compatibility bypassed batch approval")
	}
	if _, err := f.svc.ApprovePublicationCleanup(f.ctx, []int64{p.ID}); err != nil {
		t.Fatal(err)
	}
	f.drainCleanup()
	p, _ = f.publicationFiles(pub.Path)
	if p.State != "removed" || exists(pub.Path) {
		t.Fatalf("approved valid stored legacy manifest did not drain: %+v", p)
	}
}

func TestPublicationCleanupRetainsChangedSourceBeforePass(t *testing.T) {
	for _, outcome := range []string{"placed", "discarded"} {
		for _, body := range []string{"different source bytes!", "short"} {
			t.Run(outcome+"/"+body, func(t *testing.T) {
				f := setupV2(t, nil)
				pub := f.importHeld("Test.Show.S01E01.job-404.runner-changed", "changed", map[string]string{epOne + ".mkv": "original manifest bytes"})
				p, files := f.publicationFiles(pub.Path)
				pf := files[epOne+".mkv"]
				if outcome == "discarded" {
					pf.Outcome = "discarded"
					pf.DiscardReason = "rejected"
					if err := f.db.SetPublicationFileOutcome(f.ctx, pf); err != nil {
						t.Fatal(err)
					}
				}
				source := filepath.Join(pub.Path, pf.Path)
				// Detach from any library hardlink before replacing the source object.
				if err := os.Remove(source); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(source, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
				lib, err := os.ReadFile(pf.LibraryPath)
				if err != nil || string(lib) != "original manifest bytes" {
					t.Fatalf("library control: %q %v", lib, err)
				}
				f.sweep()
				p, files = f.publicationFiles(pub.Path)
				if !files[pf.Path].RemovedAt.IsZero() || !exists(source) || !strings.Contains(p.Error, "source") {
					t.Fatalf("changed source deleted: publication %+v file %+v", p, files[pf.Path])
				}
			})
		}
	}
}

func TestPublicationCleanupRequiresValidManifest(t *testing.T) {
	for _, kind := range []string{"bad digest", "symlink", "missing generation", "malformed stored v1"} {
		t.Run(kind, func(t *testing.T) {
			f := setupV2(t, nil)
			pub := f.importHeld("Test.Show.S01E01.job-405.runner-invalid", "invalid", map[string]string{epOne + ".mkv": "video"})
			p, _ := f.publicationFiles(pub.Path)
			path := filepath.Join(pub.Path, ports.ManifestFileName)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "bad digest", "missing generation":
				var m ports.Manifest
				if err := json.Unmarshal(raw, &m); err != nil {
					t.Fatal(err)
				}
				if kind == "bad digest" {
					m.Digest = "not the manifest digest"
				} else {
					m.Generation = ""
				}
				raw, err = json.Marshal(m)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, raw, 0o644); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				target := filepath.Join(pub.Path, "saved-manifest.json")
				if err := os.Rename(path, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("saved-manifest.json", path); err != nil {
					t.Fatal(err)
				}
			case "malformed stored v1":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if _, err := f.db.W.ExecContext(f.ctx, `UPDATE publications SET manifest_source='v1_record',manifest_json='{' WHERE id=?`, p.ID); err != nil {
					t.Fatal(err)
				}
			}
			f.sweep()
			p, files := f.publicationFiles(pub.Path)
			if p.Error == "" || !files[epOne+".mkv"].RemovedAt.IsZero() || !exists(filepath.Join(pub.Path, epOne+".mkv")) {
				t.Fatalf("invalid manifest allowed unlink: publication %+v files %+v", p, files)
			}
		})
	}
}

func TestPublicationAdoptionPropagatesManifestPersistenceFailure(t *testing.T) {
	f := setupV2(t, nil)
	f.deferGrabs()
	dl := f.grab(1, epOne)
	f.observe(dl, terminalFact(dl, ports.RunnerTerminal{Outcome: ports.OutcomeFailed, Reason: "publish", Salvage: "pending"}, "history 1"))
	dir := f.runnerDir(epOne+".job-406.runner-adopt", "adopt", dl.Transfer, 406, map[string]string{epOne + ".mkv": "video"})
	if _, err := f.db.W.ExecContext(f.ctx, `CREATE TRIGGER fail_adoption_manifest BEFORE UPDATE OF manifest_source ON publications WHEN NEW.manifest_source='file' BEGIN SELECT RAISE(FAIL,'injected adoption persistence failure'); END`); err != nil {
		t.Fatal(err)
	}
	adopted, orphan, err := f.svc.adoptDirectory(f.ctx, dir)
	if err == nil || !strings.Contains(err.Error(), "injected adoption persistence failure") || adopted || orphan {
		t.Fatalf("adoption = %v %v %v, want persistence error", adopted, orphan, err)
	}
	p, files := f.publicationFiles(dir)
	if p.ManifestSource != "none" || len(files) != 0 || f.count(`SELECT COUNT(*) FROM media_files`) != 0 || !exists(filepath.Join(dir, epOne+".mkv")) {
		t.Fatalf("adoption continued without evidence: publication %+v files %+v", p, files)
	}
}

func TestPublicationCleanupRetainsSourceChangedAfterHash(t *testing.T) {
	f := setupV2(t, nil)
	pub := f.importHeld("Test.Show.S01E01.job-407.runner-race", "race", map[string]string{epOne + ".mkv": "original manifest bytes"})
	source := filepath.Join(pub.Path, epOne+".mkv")
	original := publicationUnlinkHook
	t.Cleanup(func() { publicationUnlinkHook = original })
	publicationUnlinkHook = func(_ sqlite.Publication, rel string) {
		if rel != epOne+".mkv" {
			return
		}
		if err := os.Remove(source); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(source, []byte("different source bytes!"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f.sweep()
	p, files := f.publicationFiles(pub.Path)
	body, err := os.ReadFile(source)
	if err != nil || string(body) != "different source bytes!" || !files[epOne+".mkv"].RemovedAt.IsZero() || !strings.Contains(p.Error, "changed after it was verified") {
		t.Fatalf("racing replacement not retained: body %q err %v publication %+v files %+v", body, err, p, files)
	}
}
