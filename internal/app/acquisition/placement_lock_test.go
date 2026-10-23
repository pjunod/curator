package acquisition

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

func TestPlacementStagingDoesNotHoldStorageAuthorityOrInvertAdmissionLocks(t *testing.T) {
	client := &fakeClient{}
	svc, _, itemID := setup(t, nil, client)
	ctx := context.Background()
	item, err := svc.db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	src := filepath.Join(root, "source.mkv")
	dest := filepath.Join(root, "library", "target.mkv")
	if err = os.WriteFile(src, corpusFile(t, wholeCorpus1080), 0644); err != nil {
		t.Fatal(err)
	}

	stageStarted := make(chan struct{})
	continueStage := make(chan struct{})
	previousPlaceFile := placeFile
	placeFile = func(ctx context.Context, src, dest string, progress func(int64, int64)) error {
		close(stageStarted)
		select {
		case <-continueStage:
		case <-ctx.Done():
			return ctx.Err()
		}
		return previousPlaceFile(ctx, src, dest, progress)
	}
	defer func() { placeFile = previousPlaceFile }()

	// Model an import holding its per-target reservation over staging.
	svc.importTargetMu.Lock()
	targetLocked := true
	unlockTarget := func() {
		if targetLocked {
			targetLocked = false
			svc.importTargetMu.Unlock()
		}
	}
	placementDone := make(chan error, 1)
	go func() {
		_, placeErr := svc.commitPlacement(ctx, item, importScope{Attempt: "blocked-stage"}, src, dest, quality.Quality{}, nil, false)
		placementDone <- placeErr
	}()
	select {
	case <-stageStarted:
	case <-time.After(3 * time.Second):
		close(continueStage)
		unlockTarget()
		<-placementDone
		t.Fatal("placement did not reach the staged-copy boundary")
	}

	// A concurrent admission waits for the import target first. It must not
	// hold storageDecisionMu while waiting, or E02 of a pack can deadlock after
	// E01 commits and releases it to an admission.
	grabDone := make(chan error, 1)
	go func() {
		_, grabErr := svc.Grab(ctx, GrabRequest{
			MediaItemID: itemID, Season: 1, Episode: 1, Protocol: "torrent",
			Title: "Test.Show.S01E01.1080p.WEB-DL-LOCK", Indexer: "idx",
			DownloadURL: "magnet:lock-order-regression", Size: 1024,
		})
		grabDone <- grabErr
	}()

	storageReady := make(chan struct{})
	go func() {
		_, release := svc.lockStorageDecision(ctx)
		release()
		close(storageReady)
	}()
	select {
	case <-storageReady:
	case <-time.After(500 * time.Millisecond):
		close(continueStage)
		unlockTarget()
		<-placementDone
		<-grabDone
		t.Fatal("storage authority was held while the import target was reserved for staging")
	}

	close(continueStage)
	if err = <-placementDone; err != nil {
		unlockTarget()
		<-grabDone
		t.Fatalf("placement: %v", err)
	}
	unlockTarget()
	select {
	case err = <-grabDone:
		if err != nil {
			t.Fatalf("concurrent admission: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("admission did not finish after staged placement released its target")
	}
}

func TestReplacementPlacementDestinationPreservesExistingAndSelectsSafeSibling(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	src := filepath.Join(root, "new.mkv")
	dest := filepath.Join(root, "library", "episode.mkv")
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("replacement bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte("protected existing bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	scope := importScope{ReplacementOperationID: "operation-123456"}
	selected, err := replacementPlacementDestination(ctx, src, dest, scope)
	if err != nil {
		t.Fatal(err)
	}
	if selected == dest || filepath.Dir(selected) != filepath.Dir(dest) || !strings.Contains(filepath.Base(selected), "replacement-oper") {
		t.Fatalf("replacement destination = %q; want a library-local operation sibling", selected)
	}
	old, err := os.ReadFile(dest)
	if err != nil || string(old) != "protected existing bytes" {
		t.Fatalf("existing target changed during selection: %q %v", old, err)
	}

	// A collision at the deterministic candidate name must be preserved and
	// cause selection of the next sibling slot.
	if err = os.WriteFile(selected, []byte("unrelated protected bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	next, err := replacementPlacementDestination(ctx, src, dest, scope)
	if err != nil {
		t.Fatal(err)
	}
	if next == selected || !strings.Contains(filepath.Base(next), "-1.mkv") {
		t.Fatalf("collision selected %q; want the next protected sibling", next)
	}

	noOp, err := replacementPlacementDestination(ctx, src, dest, importScope{})
	if err != nil || noOp != dest {
		t.Fatalf("ordinary placement destination = %q, %v; want original destination", noOp, err)
	}
}

func TestPreparedPlacementRecoversHardlinkBeforeJournalGenerationWrite(t *testing.T) {
	svc, db, itemID := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	root := t.TempDir()
	src := filepath.Join(root, "new.mkv")
	dest := filepath.Join(root, "library", "episode.mkv")
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, corpusFile(t, wholeCorpus1080), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte("reviewed prior bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	newDigest, newSize, _, err := fileDigestSnapshot(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	previousDigest, _, targetGeneration, err := fileDigestSnapshot(ctx, dest)
	if err != nil {
		t.Fatal(err)
	}
	storage, parentIdentity, err := placementParentIdentity(dest)
	if err != nil {
		t.Fatal(err)
	}
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf("%x", sha256.Sum256([]byte("link-before-journal")))
	p := sqlite.Placement{
		ID: id, Source: src, Target: dest,
		Temporary: filepath.Join(filepath.Dir(dest), ".monarr-stage-"+id),
		Backup:    filepath.Join(filepath.Dir(dest), ".monarr-prior-"+id),
		SHA256:    newDigest, PreviousSHA256: previousDigest, Size: newSize,
		TargetObserved: true, TargetFileGeneration: targetGeneration,
		StorageRoot: filepath.Dir(dest), StorageIdentity: storage,
		StorageRootIdentity: parentIdentity, TargetParentIdentity: parentIdentity,
		ItemID: item.ID, State: "prepared",
	}
	if err = db.PreparePlacement(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(p.Temporary, corpusFile(t, wholeCorpus1080), 0644); err != nil {
		t.Fatal(err)
	}
	if err = syncPath(p.Temporary); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after making the rollback link but before persisting the
	// ctime change caused by that controlled link operation.
	if err = os.Link(p.Target, p.Backup); err != nil {
		t.Fatal(err)
	}
	if err = svc.preparePlacementPublication(ctx, &p); err != nil {
		t.Fatalf("recover link-before-journal state: %v", err)
	}
	if p.TargetAfterBackupGen == "" || p.TemporaryFileGeneration == "" {
		t.Fatalf("recovered generations: target=%q temporary=%q", p.TargetAfterBackupGen, p.TemporaryFileGeneration)
	}
	decisionCtx, release := svc.lockStorageDecision(ctx)
	if err = svc.publishPlacement(decisionCtx, &p); err != nil {
		release()
		t.Fatal(err)
	}
	if _, err = db.CommitPlacement(decisionCtx, p); err != nil {
		release()
		t.Fatal(err)
	}
	release()
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != string(corpusFile(t, wholeCorpus1080)) {
		t.Fatalf("published bytes changed: %v", err)
	}
}

func TestPreparedPlacementRejectsStagedAndTargetGenerationChanges(t *testing.T) {
	for _, tamper := range []string{"stage", "target"} {
		t.Run(tamper, func(t *testing.T) {
			svc, db, itemID := setup(t, nil, &fakeClient{})
			ctx := context.Background()
			root := t.TempDir()
			src := filepath.Join(root, "new.mkv")
			dest := filepath.Join(root, "library", "episode.mkv")
			if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(src, corpusFile(t, wholeCorpus1080), 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dest, []byte("reviewed prior bytes"), 0644); err != nil {
				t.Fatal(err)
			}
			newDigest, newSize, _, err := fileDigestSnapshot(ctx, src)
			if err != nil {
				t.Fatal(err)
			}
			previousDigest, _, targetGeneration, err := fileDigestSnapshot(ctx, dest)
			if err != nil {
				t.Fatal(err)
			}
			storage, parentIdentity, err := placementParentIdentity(dest)
			if err != nil {
				t.Fatal(err)
			}
			item, err := db.GetMediaItemFull(ctx, itemID)
			if err != nil {
				t.Fatal(err)
			}
			id := fmt.Sprintf("%x", sha256.Sum256([]byte("placement-tamper-"+tamper)))
			p := sqlite.Placement{
				ID: id, Source: src, Target: dest,
				Temporary: filepath.Join(filepath.Dir(dest), ".monarr-stage-"+id),
				Backup:    filepath.Join(filepath.Dir(dest), ".monarr-prior-"+id),
				SHA256:    newDigest, PreviousSHA256: previousDigest, Size: newSize,
				TargetObserved: true, TargetFileGeneration: targetGeneration,
				StorageRoot: filepath.Dir(dest), StorageIdentity: storage,
				StorageRootIdentity: parentIdentity, TargetParentIdentity: parentIdentity,
				ItemID: item.ID, State: "prepared",
			}
			if err = db.PreparePlacement(ctx, p); err != nil {
				t.Fatal(err)
			}
			if err = svc.preparePlacementPublication(ctx, &p); err != nil {
				t.Fatal(err)
			}
			switch tamper {
			case "stage":
				if err = os.WriteFile(p.Temporary, []byte("tampered staged data"), 0644); err != nil {
					t.Fatal(err)
				}
			case "target":
				if err = os.WriteFile(dest, []byte("changed target bytes"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			decisionCtx, release := svc.lockStorageDecision(ctx)
			err = svc.publishPlacement(decisionCtx, &p)
			release()
			if err == nil {
				t.Fatal("tampered placement published")
			}
			got, readErr := os.ReadFile(dest)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if tamper == "target" && string(got) != "changed target bytes" {
				t.Fatalf("changed target was overwritten: %q", got)
			}
			if tamper == "stage" && string(got) != "reviewed prior bytes" {
				t.Fatalf("original target changed: %q", got)
			}
		})
	}
}

func TestReconcilePreparedPlacementRejectsSameBytesFromDifferentStagedObject(t *testing.T) {
	svc, db, itemID := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	data := corpusFile(t, wholeCorpus1080)
	dest := filepath.Join(t.TempDir(), "library", "episode.mkv")
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	storage, parentIdentity, err := placementParentIdentity(dest)
	if err != nil {
		t.Fatal(err)
	}
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf("%x", sha256.Sum256([]byte("reconcile-other-staged-object")))
	p := sqlite.Placement{
		ID: id, Source: filepath.Join(t.TempDir(), "source.mkv"), Target: dest,
		Temporary: filepath.Join(filepath.Dir(dest), ".monarr-stage-"+id),
		Backup:    filepath.Join(filepath.Dir(dest), ".monarr-prior-"+id),
		SHA256:    digest, Size: int64(len(data)), StorageRoot: filepath.Dir(dest),
		StorageIdentity: storage, StorageRootIdentity: parentIdentity,
		TargetParentIdentity: parentIdentity, ItemID: item.ID, State: "prepared",
	}
	if err = db.PreparePlacement(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(p.Temporary, data, 0644); err != nil {
		t.Fatal(err)
	}
	stagedInfo, err := os.Lstat(p.Temporary)
	if err != nil {
		t.Fatal(err)
	}
	p.TemporaryFileGeneration = deliveryFileGeneration(stagedInfo)
	p.TemporaryObjectIdentity = deliveryFileStableIdentity(stagedInfo)
	if err = db.UpdatePreparedPlacement(ctx, p); err != nil {
		t.Fatal(err)
	}
	// This represents another same-content inode appearing at the target before
	// reconciliation writes PublishedFileGeneration into the journal.
	if err = os.WriteFile(dest, data, 0644); err != nil {
		t.Fatal(err)
	}
	targetInfo, err := os.Lstat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(stagedInfo, targetInfo) {
		t.Fatal("test setup unexpectedly made the target the staged inode")
	}

	svc.reconcilePlacements(ctx)
	got, err := db.GetPlacement(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "prepared" || got.PublishedFileGeneration != "" {
		t.Fatalf("reconciliation adopted another staged object: state=%q publishedGeneration=%q", got.State, got.PublishedFileGeneration)
	}
	if bytes, readErr := os.ReadFile(dest); readErr != nil || string(bytes) != string(data) {
		t.Fatalf("reconciliation changed the target: %v", readErr)
	}
}

func TestLegacyPublishedPlacementIdentityUpgradeRemainsCompatible(t *testing.T) {
	for _, method := range []string{"prepare", "reconcile"} {
		t.Run(method, func(t *testing.T) {
			svc, db, itemID := setup(t, nil, &fakeClient{})
			ctx := context.Background()
			item, err := db.GetMediaItemFull(ctx, itemID)
			if err != nil {
				t.Fatal(err)
			}
			data := corpusFile(t, wholeCorpus1080)
			dest := filepath.Join(item.Path, "legacy-published.mkv")
			if err = os.WriteFile(dest, data, 0644); err != nil {
				t.Fatal(err)
			}
			id := fmt.Sprintf("%x", sha256.Sum256([]byte("legacy-published-"+method)))
			p := sqlite.Placement{
				ID: id, Source: "legacy-source.mkv", Target: dest,
				Temporary: filepath.Join(filepath.Dir(dest), ".monarr-stage-"+id),
				Backup:    filepath.Join(filepath.Dir(dest), ".monarr-prior-"+id),
				SHA256:    fmt.Sprintf("%x", sha256.Sum256(data)), Size: int64(len(data)),
				ItemID: item.ID, State: "prepared",
			}
			if err = db.PreparePlacement(ctx, p); err != nil {
				t.Fatal(err)
			}
			switch method {
			case "prepare":
				if err = svc.preparePlacementPublication(ctx, &p); err != nil {
					t.Fatalf("legacy published target was rejected: %v", err)
				}
				if p.PublishedFileGeneration == "" {
					t.Fatal("legacy published target did not gain its digest-proven generation")
				}
			case "reconcile":
				svc.reconcilePlacements(ctx)
				got, getErr := db.GetPlacement(ctx, id)
				if getErr != nil {
					t.Fatal(getErr)
				}
				if got.State == "prepared" || got.PublishedFileGeneration == "" {
					t.Fatalf("legacy published target was not reconciled: state=%q generation=%q", got.State, got.PublishedFileGeneration)
				}
			}
		})
	}
}

func TestPlacementIdentityUpgradeDoesNotAcceptPartialJournalIdentity(t *testing.T) {
	svc, db, itemID := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	data := corpusFile(t, wholeCorpus1080)
	dest := filepath.Join(item.Path, "partial-identity.mkv")
	if err = os.WriteFile(dest, data, 0644); err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf("%x", sha256.Sum256([]byte("partial-identity")))
	p := sqlite.Placement{
		ID: id, Source: "legacy-source.mkv", Target: dest,
		Temporary: filepath.Join(filepath.Dir(dest), ".monarr-stage-"+id),
		Backup:    filepath.Join(filepath.Dir(dest), ".monarr-prior-"+id),
		SHA256:    fmt.Sprintf("%x", sha256.Sum256(data)), Size: int64(len(data)),
		TemporaryFileGeneration: "partial-new-journal", ItemID: item.ID, State: "prepared",
	}
	if err = db.PreparePlacement(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err = svc.preparePlacementPublication(ctx, &p); err == nil {
		t.Fatal("partially identified journal fell back to legacy digest-only adoption")
	}
}

func TestRollbackPreparedPlacementRestoresReviewedBackup(t *testing.T) {
	svc, db, itemID := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(item.Path, "rollback")
	if err = os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "episode.mkv")
	backup := filepath.Join(root, ".monarr-prior-rollback")
	temporary := filepath.Join(root, ".monarr-stage-rollback")
	oldBytes, newBytes := []byte("reviewed old bytes"), []byte("published replacement bytes")
	if err = os.WriteFile(target, newBytes, 0644); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(backup, oldBytes, 0644); err != nil {
		t.Fatal(err)
	}
	p := sqlite.Placement{
		ID: "rollback-reviewed-backup", Source: "source.mkv", Target: target,
		Temporary: temporary, Backup: backup,
		SHA256:         fmt.Sprintf("%x", sha256.Sum256(newBytes)),
		PreviousSHA256: fmt.Sprintf("%x", sha256.Sum256(oldBytes)),
		Size:           int64(len(newBytes)), ItemID: item.ID, State: "prepared",
	}
	if err = db.PreparePlacement(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err = svc.rollbackPlacement(ctx, p); err != nil {
		t.Fatalf("rollback prepared placement: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != string(oldBytes) {
		t.Fatalf("rollback target = %q, %v; want reviewed bytes", got, err)
	}
	if _, err = os.Lstat(backup); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rollback backup still exists: %v", err)
	}
	var journalState string
	if err = db.R.QueryRowContext(ctx, `SELECT state FROM import_placements WHERE id=?`, p.ID).Scan(&journalState); err != nil {
		t.Fatal(err)
	}
	if journalState != "rolled_back" {
		t.Fatalf("journal state = %q; want rolled_back", journalState)
	}
}

func TestRollbackPreparedPlacementPreservesChangedTarget(t *testing.T) {
	svc, db, itemID := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(item.Path, "rollback-changed")
	if err = os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "episode.mkv")
	backup := filepath.Join(root, ".monarr-prior-changed")
	oldBytes, changedBytes := []byte("reviewed old bytes"), []byte("new unrelated bytes")
	if err = os.WriteFile(target, changedBytes, 0644); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(backup, oldBytes, 0644); err != nil {
		t.Fatal(err)
	}
	p := sqlite.Placement{
		ID: "rollback-changed-target", Source: "source.mkv", Target: target,
		Temporary: filepath.Join(root, ".monarr-stage-changed"), Backup: backup,
		SHA256:         fmt.Sprintf("%x", sha256.Sum256([]byte("replacement bytes"))),
		PreviousSHA256: fmt.Sprintf("%x", sha256.Sum256(oldBytes)),
		ItemID:         item.ID, State: "prepared",
	}
	if err = db.PreparePlacement(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err = svc.rollbackPlacement(ctx, p); err == nil {
		t.Fatal("rollback accepted an unrelated current target")
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != string(changedBytes) {
		t.Fatalf("changed target = %q, %v; it must remain untouched", got, err)
	}
	if got, readErr := os.ReadFile(backup); readErr != nil || string(got) != string(oldBytes) {
		t.Fatalf("reviewed backup = %q, %v; it must remain available", got, readErr)
	}
}

func TestReconcileCommittedPlacementRetriesVerifiedCleanup(t *testing.T) {
	for _, tamperBackup := range []bool{false, true} {
		t.Run(map[bool]string{false: "verified cleanup", true: "tampered backup"}[tamperBackup], func(t *testing.T) {
			svc, db, itemID := setup(t, nil, &fakeClient{})
			ctx := context.Background()
			item, err := db.GetMediaItemFull(ctx, itemID)
			if err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(item.Path, "committed-cleanup")
			if err = os.MkdirAll(root, 0755); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(root, "episode.mkv")
			backup := filepath.Join(root, ".monarr-prior-committed")
			temporary := filepath.Join(root, ".monarr-stage-committed")
			oldBytes, newBytes := []byte("superseded bytes"), []byte("committed bytes")
			if tamperBackup {
				oldBytes = []byte("changed backup")
			}
			for path, data := range map[string][]byte{target: newBytes, backup: oldBytes, temporary: newBytes} {
				if err = os.WriteFile(path, data, 0644); err != nil {
					t.Fatal(err)
				}
			}
			storage, parentIdentity, err := placementParentIdentity(target)
			if err != nil {
				t.Fatal(err)
			}
			targetInfo, err := os.Lstat(target)
			if err != nil {
				t.Fatal(err)
			}
			p := sqlite.Placement{
				ID:     "committed-cleanup-" + map[bool]string{false: "valid", true: "tampered"}[tamperBackup],
				Source: "source.mkv", Target: target, Temporary: temporary, Backup: backup,
				SHA256:                  fmt.Sprintf("%x", sha256.Sum256(newBytes)),
				PreviousSHA256:          fmt.Sprintf("%x", sha256.Sum256([]byte("superseded bytes"))),
				PublishedFileGeneration: deliveryFileGeneration(targetInfo),
				StorageRoot:             root, StorageIdentity: storage, StorageRootIdentity: parentIdentity,
				TargetParentIdentity: parentIdentity, Size: int64(len(newBytes)), ItemID: item.ID, State: "committed",
			}
			if err = db.PreparePlacement(ctx, p); err != nil {
				t.Fatal(err)
			}
			svc.reconcilePlacements(ctx)
			var journalState string
			if err = db.R.QueryRowContext(ctx, `SELECT state FROM import_placements WHERE id=?`, p.ID).Scan(&journalState); err != nil {
				t.Fatal(err)
			}
			if tamperBackup {
				if journalState != "committed" {
					t.Fatalf("tampered cleanup state = %q; want committed/pending", journalState)
				}
				if _, statErr := os.Lstat(backup); statErr != nil {
					t.Fatalf("tampered backup was removed: %v", statErr)
				}
				return
			}
			if journalState != "cleaned" {
				t.Fatalf("verified cleanup state = %q; want cleaned", journalState)
			}
			for _, path := range []string{backup, temporary} {
				if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("verified cleanup left %s: %v", path, statErr)
				}
			}
			if data, readErr := os.ReadFile(target); readErr != nil || string(data) != string(newBytes) {
				t.Fatalf("committed target changed: %v", readErr)
			}
		})
	}
}
