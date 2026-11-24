package acquisition

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pjunod/monarr/internal/domain/mediainfo"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/infra/bus"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

func TestPublishedPlacementCommitFailureReplaysAfterDatabaseReopen(t *testing.T) {
	ctx := context.Background()
	svc, db, itemID := setup(t, nil, &fakeClient{})
	p := preparePublishedPlacementForReplay(t, ctx, svc, db, itemID, nil)
	if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER fail_placement_receipt BEFORE INSERT ON media_file_placements BEGIN SELECT RAISE(ABORT,'simulated placement receipt outage'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CommitPlacement(ctx, p); err == nil || !strings.Contains(err.Error(), "simulated placement receipt outage") {
		t.Fatalf("placement receipt trigger did not fail the commit at the intended point: %v", err)
	}
	if got, err := os.ReadFile(p.Target); err != nil || string(got) != "new committed bytes" {
		t.Fatalf("published bytes were not left for journal replay: %q err=%v", got, err)
	}
	if got, err := db.GetPlacement(ctx, p.ID); err != nil || got.State != "prepared" || got.PublishedFileGeneration == "" {
		t.Fatalf("failed commit did not preserve the pinned publication journal: placement=%+v err=%v", got, err)
	}

	reopened := reopenLifecycleDB(t, db)
	if _, err := reopened.W.ExecContext(ctx, `DROP TRIGGER fail_placement_receipt`); err != nil {
		t.Fatal(err)
	}
	restarted := New(reopened, bus.New(nil), nil, func(ports.IndexerConfig) ports.Indexer { return fakeIndexer{} }, func(ports.ClientConfig) ports.DownloadClient { return &fakeClient{} })
	restarted.reconcilePlacements(ctx)
	got, err := reopened.GetPlacement(ctx, p.ID)
	if err != nil || (got.State != "cleaned" && got.State != "committed") {
		t.Fatalf("published placement was not reconciled after reopening: state=%q err=%v", got.State, err)
	}
	files, err := reopened.ListFilesForItem(ctx, itemID)
	if err != nil || len(files) != 1 || files[0].Path != p.Target {
		t.Fatalf("replayed placement receipt rows=%+v err=%v", files, err)
	}
	if _, err := os.Stat(p.Temporary); !os.IsNotExist(err) {
		t.Fatalf("staging file was not settled after replay: %v", err)
	}
}

func TestPublishedPlacementConflictAfterCommitFailurePreservesBothByteSets(t *testing.T) {
	ctx := context.Background()
	svc, db, itemID := setup(t, nil, &fakeClient{})
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	prior := []byte("reviewed prior bytes")
	p := preparePublishedPlacementForReplay(t, ctx, svc, db, itemID, prior)
	if p.Target != filepath.Join(item.Path, "Season 1", "replayed.mkv") {
		t.Fatalf("placement helper target=%q, unexpected path", p.Target)
	}
	if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER fail_placement_conflict BEFORE INSERT ON media_file_placements BEGIN SELECT RAISE(ABORT,'simulated placement receipt outage'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CommitPlacement(ctx, p); err == nil || !strings.Contains(err.Error(), "simulated placement receipt outage") {
		t.Fatalf("placement receipt trigger did not fail the commit at the intended point: %v", err)
	}
	backup := p.Backup
	if got, err := os.ReadFile(backup); err != nil || string(got) != string(prior) {
		t.Fatalf("reviewed rollback bytes were not retained: %q err=%v", got, err)
	}
	// Keep the published inode alive under another name so this replacement
	// cannot accidentally reuse it, then place unrelated bytes at the target.
	held := p.Target + ".held-published"
	if err := os.Rename(p.Target, held); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(held) }()
	conflict := []byte("external destination conflict")
	if err := os.WriteFile(p.Target, conflict, 0o644); err != nil {
		t.Fatal(err)
	}
	reopened := reopenLifecycleDB(t, db)
	if _, err := reopened.W.ExecContext(ctx, `DROP TRIGGER fail_placement_conflict`); err != nil {
		t.Fatal(err)
	}
	restarted := New(reopened, bus.New(nil), nil, func(ports.IndexerConfig) ports.Indexer { return fakeIndexer{} }, func(ports.ClientConfig) ports.DownloadClient { return &fakeClient{} })
	restarted.reconcilePlacements(ctx)
	got, err := reopened.GetPlacement(ctx, p.ID)
	if err != nil || got.State != "prepared" {
		t.Fatalf("conflicted journal should remain prepared: state=%q err=%v", got.State, err)
	}
	if bytes, err := os.ReadFile(p.Target); err != nil || string(bytes) != string(conflict) {
		t.Fatalf("reconciliation overwrote the conflicting destination: %q err=%v", bytes, err)
	}
	if bytes, err := os.ReadFile(backup); err != nil || string(bytes) != string(prior) {
		t.Fatalf("reconciliation lost reviewed rollback bytes: %q err=%v", bytes, err)
	}
	files, err := reopened.ListFilesForItem(ctx, itemID)
	if err != nil || len(files) != 0 {
		t.Fatalf("conflicted placement created library ownership: files=%+v err=%v", files, err)
	}
}

func preparePublishedPlacementForReplay(t *testing.T, ctx context.Context, svc *Service, db *sqlite.DB, itemID int64, prior []byte) sqlite.Placement {
	t.Helper()
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(item.Path, "Season 1", "replayed.mkv")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if prior != nil {
		if err := os.WriteFile(target, prior, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	storage, parentIdentity, err := placementParentIdentity(target)
	if err != nil {
		t.Fatal(err)
	}
	newBytes := []byte("new committed bytes")
	id := fmt.Sprintf("%x", sha256.Sum256(append([]byte("replay-placement:"), target...)))
	p := sqlite.Placement{
		ID: id, Source: filepath.Join(t.TempDir(), "source.mkv"), Target: target,
		Temporary: filepath.Join(filepath.Dir(target), ".monarr-stage-"+id),
		Backup:    filepath.Join(filepath.Dir(target), ".monarr-prior-"+id),
		SHA256:    sha256Hex(newBytes), Size: int64(len(newBytes)),
		StorageRoot: filepath.Dir(target), StorageIdentity: storage,
		StorageRootIdentity: parentIdentity, TargetParentIdentity: parentIdentity,
		ItemID: item.ID, State: "prepared",
		Quality:    quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080},
		Provenance: mediainfo.ProvenanceFilename, Confidence: mediainfo.ConfidenceNone,
	}
	if prior != nil {
		info, statErr := os.Lstat(target)
		if statErr != nil {
			t.Fatal(statErr)
		}
		p.TargetObserved = true
		p.PreviousSHA256 = sha256Hex(prior)
		p.TargetFileGeneration = deliveryFileGeneration(info)
	}
	if err := db.PreparePlacement(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Temporary, newBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	staged, err := os.Lstat(p.Temporary)
	if err != nil {
		t.Fatal(err)
	}
	p.TemporaryFileGeneration = deliveryFileGeneration(staged)
	p.TemporaryObjectIdentity = deliveryFileStableIdentity(staged)
	if err := db.UpdatePreparedPlacement(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := svc.preparePlacementPublication(ctx, &p); err != nil {
		t.Fatalf("prepare published placement: %v", err)
	}
	if err := svc.publishPlacement(ctx, &p); err != nil {
		t.Fatalf("publish placement before DB transaction: %v", err)
	}
	return p
}

func reopenLifecycleDB(t *testing.T, db *sqlite.DB) *sqlite.DB {
	t.Helper()
	dir := filepath.Dir(db.Path)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return reopened
}
