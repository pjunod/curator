package acquisition

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPartialCleanupDoesNotSettleAbsentPayloadDuringStorageOutage(t *testing.T) {
	ctx := context.Background()
	svc, db, itemID, root := completedFixture(t)
	identity := "nfs4\x00nas:/exports\x00/library"
	observedIdentity := identity
	available := true
	originalObserver := observeStorageIdentity
	observeStorageIdentity = func(string) (string, error) {
		if !available {
			return "", errors.New("simulated NAS unavailable")
		}
		return observedIdentity, nil
	}
	t.Cleanup(func() { observeStorageIdentity = originalObserver })
	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}

	parent := filepath.Join(root, "failed-release")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	missingPayload := filepath.Join(parent, "payload")
	dl := insertImportable(t, db, itemID, missingPayload)
	dl.State = "failed"
	if err := db.UpdateDownloadHandoff(ctx, dl); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE downloads SET cleanup_pending=1, payload_removed=0 WHERE id=? AND generation_key=?`, dl.ID, dl.GenerationKey); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(missingPayload); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture unexpectedly contains payload: %v", err)
	}

	available = false
	svc.cleanupPartialPayloads(ctx)
	retained, err := db.GetDownload(ctx, dl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retained.GenerationKey != dl.GenerationKey || !retained.CleanupPending || retained.PayloadRemoved {
		t.Fatalf("absent bytes on an unavailable mount falsely settled custody: %+v", retained)
	}

	available = true
	observedIdentity = "nfs4\x00different-nas:/exports\x00/library"
	svc.cleanupPartialPayloads(ctx)
	retained, err = db.GetDownload(ctx, dl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retained.GenerationKey != dl.GenerationKey || !retained.CleanupPending || retained.PayloadRemoved {
		t.Fatalf("absent bytes on a different storage identity falsely settled custody: %+v", retained)
	}

	// The original storage comes back without changing settings or adopting the
	// visible mountpoint. The next ordinary sweep can then settle its exact row.
	observedIdentity = identity
	svc.cleanupPartialPayloads(ctx)
	settled, err := db.GetDownload(ctx, dl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if settled.GenerationKey != dl.GenerationKey || settled.CleanupPending || !settled.PayloadRemoved {
		t.Fatalf("custody did not settle after the trusted storage returned: %+v", settled)
	}
}

func TestPartialCleanupCandidateRejectsStaleGenerationAndDeliveryOwnership(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
	}{
		{name: "reused id with a new generation"},
		{name: "delivery ownership changed after snapshot"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, db, itemID, root := completedFixture(t)
			payloadDir := filepath.Join(root, "stale-candidate")
			if err := os.MkdirAll(payloadDir, 0o755); err != nil {
				t.Fatal(err)
			}
			payload := filepath.Join(payloadDir, "partial.mkv")
			if err := os.WriteFile(payload, []byte("must remain in custody"), 0o644); err != nil {
				t.Fatal(err)
			}
			dl := insertImportable(t, db, itemID, payload)
			if err := db.UpdateDownloadHandoff(ctx, dl); err != nil {
				t.Fatal(err)
			}
			if _, err := db.W.ExecContext(ctx, `UPDATE downloads SET state='failed',cleanup_pending=1,payload_removed=0 WHERE id=? AND generation_key=?`, dl.ID, dl.GenerationKey); err != nil {
				t.Fatal(err)
			}
			candidate := partialCleanupOwner{DownloadID: dl.ID, GenerationKey: dl.GenerationKey}
			if tc.name == "reused id with a new generation" {
				// Recreate a row under the same integer ID and timestamp, as happens
				// after deletion/reuse, while preserving the stale queue snapshot.
				if _, err := db.W.ExecContext(ctx, `DELETE FROM downloads WHERE id=? AND generation_key=?`, dl.ID, dl.GenerationKey); err != nil {
					t.Fatal(err)
				}
				replacement := insertImportable(t, db, itemID, payload)
				if replacement.ID != dl.ID || replacement.GenerationKey == dl.GenerationKey {
					t.Fatalf("fixture did not reuse row identity with a fresh generation: old=%+v new=%+v", dl, replacement)
				}
				if err := db.UpdateDownloadHandoff(ctx, replacement); err != nil {
					t.Fatal(err)
				}
				if _, err := db.W.ExecContext(ctx, `UPDATE downloads SET added_at=?,updated_at=?,state='failed',cleanup_pending=1,payload_removed=0 WHERE id=? AND generation_key=?`, dl.AddedAt.UnixMilli(), dl.UpdatedAt.UnixMilli(), replacement.ID, replacement.GenerationKey); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := db.W.ExecContext(ctx, `UPDATE downloads SET delivery_expected=1 WHERE id=? AND generation_key=?`, dl.ID, dl.GenerationKey); err != nil {
					t.Fatal(err)
				}
			}

			svc.cleanupPartialPayloadCandidate(ctx, candidate)
			data, err := os.ReadFile(payload)
			if err != nil || string(data) != "must remain in custody" {
				t.Fatalf("stale cleanup candidate changed payload: data=%q err=%v", data, err)
			}
			current, err := db.GetDownload(ctx, dl.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !current.CleanupPending || current.PayloadRemoved {
				t.Fatalf("stale cleanup candidate settled newer custody state: %+v", current)
			}
			if tc.name == "reused id with a new generation" && (current.GenerationKey == candidate.GenerationKey || current.AddedAt.UnixMilli() != dl.AddedAt.UnixMilli()) {
				t.Fatalf("fixture failed to preserve timestamp while reusing ID with a new generation: %+v", current)
			}
			if tc.name == "delivery ownership changed after snapshot" && !current.DeliveryExpected {
				t.Fatalf("delivery ownership change was lost: %+v", current)
			}
		})
	}
}
