package acquisition

import (
	"context"
	"errors"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"os"
	"path/filepath"
	"testing"
)

func TestRestartCancellationReconciliationWaitsForActualOwnership(t *testing.T) {
	svc, db, itemID := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	var keys []sqlite.ImportAttemptKey
	for _, name := range []string{"quiet", "locked", "publication"} {
		dl := insertImportable(t, db, itemID, "/source/"+name)
		a, e := db.EnsureImportAttempt(ctx, dl.ID, dl.GenerationKey)
		if e != nil {
			t.Fatal(e)
		}
		if name == "publication" {
			if e = db.SealImportAttempt(ctx, a.ImportAttemptKey, []sqlite.ImportAttemptFile{{Source: "/source/publication", Kind: "media"}}, false); e != nil {
				t.Fatal(e)
			}
			epoch := a.Epoch
			p := sqlite.Placement{ID: "reconcile-publication", DownloadID: dl.ID, SourceGeneration: dl.GenerationKey, ImportEpoch: &epoch, Source: "/source/publication", Target: "/library/result", State: "prepared"}
			if e = db.PreparePlacement(ctx, p); e != nil {
				t.Fatal(e)
			}
			if e = db.AuthorizeImportPlacement(ctx, p); e != nil {
				t.Fatal(e)
			}
		}
		if _, e = db.RequestImportCancellation(ctx, a.ImportAttemptKey); e != nil {
			t.Fatal(e)
		}
		keys = append(keys, a.ImportAttemptKey)
	}
	unlock := svc.lockDownload(keys[1].DownloadID)
	svc.reconcileImportCancellations(ctx)
	for i, key := range keys {
		a, e := db.GetImportAttempt(ctx, key.DownloadID, key.GenerationKey)
		want := "pending"
		if i == 0 {
			want = "cancelled"
		}
		if e != nil || a.Outcome != want {
			t.Fatalf("ownership %d %+v %v", i, a, e)
		}
	}
	unlock()
	svc.reconcileImportCancellations(ctx)
	a, e := db.GetImportAttempt(ctx, keys[1].DownloadID, keys[1].GenerationKey)
	if e != nil || a.Outcome != "cancelled" {
		t.Fatalf("quiescent pending attempt not reconciled: %+v %v", a, e)
	}
	newer, e := db.BeginImportRetry(ctx, a.DownloadID, a.GenerationKey)
	if e != nil {
		t.Fatal(e)
	}
	svc.finishCancelledImport(ctx, keys[1])
	fresh, e := db.GetImportAttempt(ctx, a.DownloadID, a.GenerationKey)
	if e != nil || fresh.Epoch != newer.Epoch || fresh.Outcome != "" {
		t.Fatalf("stale reconciliation finalized retry %+v %v", fresh, e)
	}
}

func TestCleanedDeliverySourceReuseRequiresCurrentPhysicalAndLogicalReceipt(t *testing.T) {
	for _, change := range []string{"none", "same-byte-new-inode", "logical-owner", "missing-target", "changed-source"} {
		t.Run(change, func(t *testing.T) {
			svc, db, itemID := setup(t, nil, &fakeClient{})
			ctx := context.Background()
			root := t.TempDir()
			source := filepath.Join(root, "source.mkv")
			target := filepath.Join(root, "target.mkv")
			data := []byte("immutable media bytes")
			for _, path := range []string{source, target} {
				if e := os.WriteFile(path, data, 0600); e != nil {
					t.Fatal(e)
				}
			}
			dl := insertImportable(t, db, itemID, source)
			a, e := db.EnsureImportAttempt(ctx, dl.ID, dl.GenerationKey)
			if e != nil {
				t.Fatal(e)
			}
			item, e := db.GetMediaItemFull(ctx, itemID)
			if e != nil {
				t.Fatal(e)
			}
			digest, size, identity, e := fileDigestSnapshot(ctx, target)
			if e != nil {
				t.Fatal(e)
			}
			epoch := a.Epoch
			p := sqlite.Placement{ID: "prior-receipt", ImportEpoch: &epoch, DownloadID: dl.ID, SourceGeneration: dl.GenerationKey, SourceTransfer: dl.Transfer, ItemID: itemID, Source: source, Target: target, Temporary: filepath.Join(root, "stage"), SHA256: digest, Size: size, PublishedFileGeneration: identity, State: "prepared"}
			if e = db.SealImportAttempt(ctx, a.ImportAttemptKey, []sqlite.ImportAttemptFile{{Source: source, Kind: "media"}}, false); e != nil {
				t.Fatal(e)
			}
			if e = db.PreparePlacement(ctx, p); e != nil {
				t.Fatal(e)
			}
			if e = db.AuthorizeImportPlacement(ctx, p); e != nil {
				t.Fatal(e)
			}
			fid, e := db.CommitPlacement(ctx, p)
			if e != nil {
				t.Fatal(e)
			}
			retry, e := db.BeginImportRetry(ctx, dl.ID, dl.GenerationKey)
			if e != nil {
				t.Fatal(e)
			}
			if e = db.SealImportAttempt(ctx, retry.ImportAttemptKey, []sqlite.ImportAttemptFile{{Source: source, Kind: "media"}}, false); e != nil {
				t.Fatal(e)
			}
			switch change {
			case "same-byte-new-inode":
				if e = os.Rename(target, target+".old"); e != nil {
					t.Fatal(e)
				}
				if e = os.WriteFile(target, data, 0600); e != nil {
					t.Fatal(e)
				}
			case "logical-owner":
				if _, e = db.W.ExecContext(ctx, `UPDATE media_files SET file_generation='another-owner' WHERE id=?`, fid); e != nil {
					t.Fatal(e)
				}
			case "missing-target":
				if e = os.Remove(target); e != nil {
					t.Fatal(e)
				}
			case "changed-source":
				if e = os.WriteFile(source, []byte("changed source"), 0600); e != nil {
					t.Fatal(e)
				}
			}
			if change != "changed-source" {
				if e = os.Remove(source); e != nil {
					t.Fatal(e)
				}
			}
			bound := withImportAttempt(context.WithValue(ctx, deliveryAttemptPlanKey{}, true), retry.ImportAttemptKey)
			bound = withExpectedSourceDigests(bound, map[string]string{source: digest})
			result, reused, e := svc.reuseCommittedImport(bound, dl, item, importScope{Dest: root, Transfer: dl.Transfer}, source)
			if change == "none" {
				if e != nil || !reused || result.Path != target {
					t.Fatalf("cleaned accepted source not reused: %+v %v %v", result, reused, e)
				}
			} else if e == nil || reused {
				t.Fatalf("changed receipt reused (%s): %+v %v %v", change, result, reused, e)
			}
			if change == "logical-owner" && !errors.Is(e, sqlite.ErrPlacementGenerationChanged) {
				t.Fatalf("logical fence lost: %v", e)
			}
		})
	}
}
