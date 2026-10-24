package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/infra/bus"
	"github.com/pjunod/monarr/internal/ports"
)

func TestCommittedPlacementCleanupStampReplaysAfterRestart(t *testing.T) {
	ctx := context.Background()
	svc, db, itemID := setup(t, nil, &fakeClient{})
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(item.Path, "cleanup-stamp-replay.mkv")
	prior := []byte("reviewed original target bytes")
	payload := []byte("accepted replacement bytes")
	if err := os.WriteFile(destination, prior, 0o600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "accepted-source.mkv")
	if err := os.WriteFile(source, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER fail_cleanup_stamp BEFORE UPDATE OF state ON import_placements
		WHEN NEW.state='cleaned' BEGIN SELECT RAISE(ABORT,'injected cleanup stamp failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.commitPlacement(ctx, item, importScope{Attempt: "cleanup-stamp-replay"}, source, destination, quality.Quality{}, nil, false); err != nil {
		t.Fatalf("placement commit should remain successful with cleanup pending: %v", err)
	}
	got, err := os.ReadFile(destination)
	if err != nil || string(got) != string(payload) {
		t.Fatalf("published bytes=%q err=%v", got, err)
	}
	var state string
	if err := db.R.QueryRowContext(ctx, `SELECT state FROM import_placements WHERE target=?`, destination).Scan(&state); err != nil || state != "committed" {
		t.Fatalf("failed cleanup stamp should leave committed journal, got state=%q err=%v", state, err)
	}
	if entries, err := os.ReadDir(item.Path); err != nil {
		t.Fatal(err)
	} else if len(entries) != 1 || entries[0].Name() != filepath.Base(destination) {
		t.Fatalf("cleanup did not durably remove only stage and backup before stamp failure: %v", entries)
	}
	if _, err := db.W.ExecContext(ctx, `DROP TRIGGER fail_cleanup_stamp`); err != nil {
		t.Fatal(err)
	}

	reopened := reopenLifecycleDB(t, db)
	restarted := New(reopened, bus.New(nil), nil,
		func(ports.IndexerConfig) ports.Indexer { return fakeIndexer{} },
		func(ports.ClientConfig) ports.DownloadClient { return &fakeClient{} })
	restarted.reconcilePlacements(ctx)
	if err := reopened.R.QueryRowContext(ctx, `SELECT state FROM import_placements WHERE target=?`, destination).Scan(&state); err != nil || state != "cleaned" {
		t.Fatalf("restart did not settle cleanup-only journal: state=%q err=%v", state, err)
	}
	got, err = os.ReadFile(destination)
	if err != nil || string(got) != string(payload) {
		t.Fatalf("cleanup replay changed committed bytes: %q err=%v", got, err)
	}
	files, err := reopened.ListFilesForItem(ctx, itemID)
	if err != nil || len(files) != 1 || files[0].Path != destination || files[0].FileGeneration == "" {
		t.Fatalf("cleanup replay changed committed library ownership: files=%+v err=%v", files, err)
	}
}
