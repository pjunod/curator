package acquisition

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

func storageChangeFixture(t *testing.T) (*Service, *sqlite.DB, string, *string) {
	t.Helper()
	svc, db, item, root := completedFixture(t)
	ctx := context.Background()
	writeCompleted(t, filepath.Join(root, "old-payload", epRelease+".mkv"), 21)
	cfg, err := db.GetDownloadClient(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	cfg.PathMappings = []ports.PathMapping{{Remote: "/remote/a", Local: root}}
	if err := db.UpdateDownloadClient(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	dl, err := db.InsertDownloadRecord(ctx, sqlite.Download{MediaItemID: item, ClientID: 1, ReleaseTitle: epRelease, Protocol: "usenet", State: "imported"})
	if err != nil {
		t.Fatal(err)
	}
	dl.ImportPath = filepath.Join(root, "old-payload")
	if err := db.UpdateDownloadHandoff(ctx, dl); err != nil {
		t.Fatal(err)
	}
	source := "nas:/expected"
	original := observeStorageIdentity
	observeStorageIdentity = func(string) (string, error) { return "nfs4\x00" + source + "\x00/exports", nil }
	t.Cleanup(func() { observeStorageIdentity = original })
	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	source = "nas:/replacement"
	if err := svc.ScanCompleted(ctx); err == nil {
		t.Fatal("different source accepted without review")
	}
	return svc, db, root, &source
}
func storageChangeRequest(t *testing.T, svc *Service, root string) StorageChangeRequest {
	t.Helper()
	cached := inventoryRoot(t, svc, root)
	return StorageChangeRequest{Root: root, ExpectedIdentity: cached.StorageIdentity, ObservedIdentity: cached.ObservedStorageIdentity}
}
func TestStorageChangeAcceptanceFencesOnlyOldCustodyInSelectedRoot(t *testing.T) {
	svc, db, root, source := storageChangeFixture(t)
	ctx := context.Background()
	rootB := t.TempDir()
	if err := svc.SetCompletedRoots(ctx, root+"\n"+rootB); err != nil {
		t.Fatal(err)
	}
	// One client may route to two roots. An in-flight path is not yet known;
	// its generation is held only if it later completes in the accepted root.
	cfg, err := db.GetDownloadClient(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	cfg.PathMappings = append(cfg.PathMappings, ports.PathMapping{Remote: "/remote/b", Local: rootB})
	if err := db.UpdateDownloadClient(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	pending, err := db.InsertDownloadRecord(ctx, sqlite.Download{MediaItemID: 1, ClientID: 1, ReleaseTitle: "in-flight", Protocol: "usenet", State: "downloading"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `INSERT INTO legacy_inflight_import_evidence(generation_key,cutover_ms,evidence) VALUES(?,1,'historical import permission')`, pending.GenerationKey); err != nil {
		t.Fatal(err)
	}
	request := storageChangeRequest(t, svc, root)
	review, err := svc.PreviewCompletedStorageChange(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if review.ExistingPayloads != 1 || review.ExistingRequests != 2 || review.InFlightRequests != 1 || !strings.Contains(review.ExpectedDescription, "nas:/expected") || !strings.Contains(review.ObservedDescription, "nas:/replacement") {
		t.Fatalf("comparison/impact=%+v", review)
	}
	if err := svc.AcceptCompletedStorageChange(ctx, review.StorageChangeRequest); err != nil {
		t.Fatal(err)
	}
	if err := svc.checkStorageIdentity(ctx, root); err != nil {
		t.Fatalf("accepted storage not available to future work: %v", err)
	}
	held, err := db.LegacyAutomaticImportReview(ctx, pending.GenerationKey, filepath.Join(root, "finished-later"))
	if err != nil || !held {
		t.Fatalf("old in-flight custody inherited new trust: %v %v", held, err)
	}
	held, err = db.LegacyCleanupReview(ctx, pending.GenerationKey, filepath.Join(rootB, "finished-elsewhere"))
	if err != nil || held {
		t.Fatalf("acceptance fenced unrelated root: %v %v", held, err)
	}
	fresh, err := db.InsertDownloadRecord(ctx, sqlite.Download{MediaItemID: 1, ClientID: 1, ReleaseTitle: "fresh", Protocol: "usenet", State: "downloading"})
	if err != nil {
		t.Fatal(err)
	}
	held, err = db.LegacyCleanupReview(ctx, fresh.GenerationKey, filepath.Join(root, "fresh-publication"))
	if err != nil || held {
		t.Fatalf("future acquisition inherited old fence: %v %v", held, err)
	}
	cached := inventoryRoot(t, svc, root)
	if cached.StorageIdentity != request.ObservedIdentity || cached.Error != "" || !cached.Entries[0].LegacyReview {
		t.Fatalf("accepted inventory lost exact review boundary: %+v", cached)
	}
	var audits int
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM history_events WHERE type='storage_change_accepted'`).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("audit=%d %v", audits, err)
	}
	// Returning the same accepted mount still uses ordinary automatic recovery.
	*source = "nas:/unavailable-replacement"
	_ = svc.ScanCompleted(ctx)
	*source = "nas:/replacement"
	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatalf("accepted expected source did not recover: %v", err)
	}
}
func TestStorageChangeRejectsStaleReviewAndRollsBackAuditFailure(t *testing.T) {
	for _, fault := range []string{"new request", "mount changed", "audit failure"} {
		t.Run(fault, func(t *testing.T) {
			svc, db, root, source := storageChangeFixture(t)
			ctx := context.Background()
			request := storageChangeRequest(t, svc, root)
			review, err := svc.PreviewCompletedStorageChange(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "new request":
				if _, err := db.InsertDownloadRecord(ctx, sqlite.Download{MediaItemID: 1, ClientID: 1, ReleaseTitle: "new pending", Protocol: "usenet", State: "downloading"}); err != nil {
					t.Fatal(err)
				}
			case "mount changed":
				*source = "nas:/third-source"
			case "audit failure":
				if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER fail_storage_audit BEFORE INSERT ON history_events WHEN NEW.type='storage_change_accepted' BEGIN SELECT RAISE(ABORT,'injected storage audit failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			if err := svc.AcceptCompletedStorageChange(ctx, review.StorageChangeRequest); err == nil {
				t.Fatalf("accepted %s", fault)
			}
			cached := inventoryRoot(t, svc, root)
			if cached.StorageIdentity != request.ExpectedIdentity {
				t.Fatal("failed acceptance changed trusted identity")
			}
			var fences int
			if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM storage_change_review_fences`).Scan(&fences); err != nil || fences != 0 {
				t.Fatalf("failed acceptance left partial custody fences: %d %v", fences, err)
			}
		})
	}
}
func TestStorageChangeReobservesLiveMountBeforeCommit(t *testing.T) {
	svc, db, root, _ := storageChangeFixture(t)
	ctx := context.Background()
	request := storageChangeRequest(t, svc, root)
	review, err := svc.PreviewCompletedStorageChange(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	prior := observeStorageIdentity
	observeStorageIdentity = func(string) (string, error) {
		calls++
		if calls == 1 {
			return request.ObservedIdentity, nil
		}
		return "", errors.New("mount disappeared at commit")
	}
	defer func() { observeStorageIdentity = prior }()
	if err := svc.AcceptCompletedStorageChange(ctx, review.StorageChangeRequest); err == nil {
		t.Fatal("storage changed after preview but before commit")
	}
	var fences int
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM storage_change_review_fences`).Scan(&fences); err != nil || fences != 0 {
		t.Fatalf("unstable mount gained authority: %d %v", fences, err)
	}
}

func TestStorageChangeReviewSurvivesUnchangedBackgroundRescan(t *testing.T) {
	svc, _, root, _ := storageChangeFixture(t)
	ctx := context.Background()
	request := storageChangeRequest(t, svc, root)
	review, err := svc.PreviewCompletedStorageChange(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	before := inventoryRoot(t, svc, root).CheckedAt
	if err := svc.ScanCompleted(ctx); err == nil {
		t.Fatal("changed storage unexpectedly passed scan")
	}
	after := inventoryRoot(t, svc, root).CheckedAt
	if !after.After(before) {
		t.Fatal("test did not refresh scan metadata")
	}
	refreshed, err := svc.PreviewCompletedStorageChange(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.ReviewToken != review.ReviewToken {
		t.Fatal("observation-only metadata invalidated unchanged storage review")
	}
	if err := svc.AcceptCompletedStorageChange(ctx, review.StorageChangeRequest); err != nil {
		t.Fatalf("unchanged background scan invalidated acceptance: %v", err)
	}
}
func TestLegacyBlockMountIdentityStrengthensOnlyOnExactRawObservation(t *testing.T) {
	for _, sameRaw := range []bool{true, false} {
		t.Run(fmt.Sprintf("raw unchanged=%v", sameRaw), func(t *testing.T) {
			svc, db, _, root := completedFixture(t)
			ctx := context.Background()
			writeCompleted(t, filepath.Join(root, "payload", "video.mkv"), 21)
			raw := "linux-v1\x00ext4\x00/dev/sda1\x00/"
			uuid := "linux-v1\x00ext4\x00UUID=known-filesystem\x00/"
			prior, priorLegacy := observeStorageIdentity, observeLegacyStorageIdentity
			defer func() { observeStorageIdentity = prior; observeLegacyStorageIdentity = priorLegacy }()
			observeStorageIdentity = func(string) (string, error) { return raw, nil }
			if err := svc.ScanCompleted(ctx); err != nil {
				t.Fatal(err)
			}
			observeStorageIdentity = func(string) (string, error) { return uuid, nil }
			observeLegacyStorageIdentity = func(string) (string, error) {
				if sameRaw {
					return raw, nil
				}
				return strings.Replace(raw, "/dev/sda1", "/dev/sdb1", 1), nil
			}
			err := svc.ScanCompleted(ctx)
			cached := inventoryRoot(t, svc, root)
			if sameRaw {
				if err != nil || cached.StorageIdentity != uuid || cached.Error != "" {
					t.Fatalf("same raw mount demanded reacceptance: %+v %v", cached, err)
				}
			} else if err == nil || cached.StorageIdentity != raw {
				t.Fatalf("changed raw mount inherited old trust: %+v %v", cached, err)
			}
			var fences int
			if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM storage_change_review_fences`).Scan(&fences); err != nil || fences != 0 {
				t.Fatalf("identity upgrade added custody fences: %d %v", fences, err)
			}
		})
	}
}
