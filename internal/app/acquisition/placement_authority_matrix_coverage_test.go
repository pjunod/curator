package acquisition

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/infra/bus"
	"github.com/pjunod/monarr/internal/ports"
)

func TestPlacementRejectsBlockedParentBeforeCreatingAuthority(t *testing.T) {
	ctx := context.Background()
	svc, db, itemID := setup(t, nil, &fakeClient{})
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "accepted.mkv")
	payload := []byte("accepted bytes remain at source")
	if err = os.WriteFile(source, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(item.Path, "blocked-parent")
	if err = os.WriteFile(parent, []byte("parent obstruction"), 0o600); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(parent, "episode.mkv")

	if _, err = svc.commitPlacement(ctx, item, importScope{Attempt: "blocked-parent"}, source, destination, quality.Quality{}, nil, false); err == nil {
		t.Fatal("placement accepted a non-directory target parent")
	}
	if got, readErr := os.ReadFile(source); readErr != nil || string(got) != string(payload) {
		t.Fatalf("source bytes changed after parent obstruction: %q err=%v", got, readErr)
	}
	if got, readErr := os.ReadFile(parent); readErr != nil || string(got) != "parent obstruction" {
		t.Fatalf("obstructing parent file changed: %q err=%v", got, readErr)
	}
	placements, err := db.PendingPlacements(ctx)
	if err != nil || len(placements) != 0 {
		t.Fatalf("unpublishable destination acquired placement authority: rows=%+v err=%v", placements, err)
	}
}

func TestReconcilePlacementDefersAcrossParentReplacementThenCommitsRestoredParent(t *testing.T) {
	ctx := context.Background()
	svc, db, itemID := setup(t, nil, &fakeClient{})
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(item.Path, "parent-authority")
	if err = os.MkdirAll(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "accepted.mkv")
	if err = os.WriteFile(source, []byte("payload pinned to the staged inode"), 0o600); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(parent, "episode.mkv")
	if _, err = db.W.ExecContext(ctx, `CREATE TRIGGER fail_parent_test_generation BEFORE UPDATE OF data ON import_placements
		WHEN COALESCE(json_extract(NEW.data,'$.published_file_generation'),'') <> ''
		BEGIN SELECT RAISE(ABORT,'stop after publication'); END`); err != nil {
		t.Fatal(err)
	}
	_, err = svc.commitPlacement(ctx, item, importScope{Attempt: "parent-identity"}, source, destination, quality.Quality{}, nil, false)
	if err == nil {
		t.Fatal("expected journal write to fail after physical publication")
	}
	accepted, err := os.Lstat(destination)
	if err != nil {
		t.Fatal(err)
	}

	// Preserve the exact published inode while moving it under a different
	// parent object. A digest-only reconcile would incorrectly adopt it.
	replacementParent := parent + ".replacement"
	if err = os.Mkdir(replacementParent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err = os.Link(destination, filepath.Join(replacementParent, filepath.Base(destination))); err != nil {
		t.Fatal(err)
	}
	heldParent := parent + ".held"
	if err = os.Rename(parent, heldParent); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(replacementParent, parent); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, statErr := os.Lstat(heldParent); statErr == nil {
			_ = os.RemoveAll(parent)
			_ = os.Rename(heldParent, parent)
		}
		_ = os.RemoveAll(replacementParent)
	}()
	replacedInfo, err := os.Lstat(destination)
	if err != nil || !os.SameFile(accepted, replacedInfo) {
		t.Fatalf("fixture did not preserve published inode under replacement parent: %v", err)
	}

	reopened := reopenLifecycleDB(t, db)
	if _, err = reopened.W.ExecContext(ctx, `DROP TRIGGER fail_parent_test_generation`); err != nil {
		t.Fatal(err)
	}
	restarted := New(reopened, bus.New(nil), nil,
		func(ports.IndexerConfig) ports.Indexer { return fakeIndexer{} },
		func(ports.ClientConfig) ports.DownloadClient { return &fakeClient{} })
	restarted.reconcilePlacements(ctx)
	placement, err := reopened.PendingPlacements(ctx)
	if err != nil || len(placement) != 1 || placement[0].State != "prepared" {
		t.Fatalf("reconcile adopted publication beneath a different parent object: rows=%+v err=%v", placement, err)
	}
	var files int
	if err = reopened.R.QueryRowContext(ctx, `SELECT count(*) FROM media_files WHERE path=?`, destination).Scan(&files); err != nil || files != 0 {
		t.Fatalf("replacement parent gained library ownership: files=%d err=%v", files, err)
	}

	// Restoring the exact pinned parent makes the original publication safely
	// recoverable; reconciliation must not strand an otherwise valid journal.
	if err = os.Rename(parent, replacementParent); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(heldParent, parent); err != nil {
		t.Fatal(err)
	}
	restarted.reconcilePlacements(ctx)
	got, err := os.ReadFile(destination)
	if err != nil || string(got) != "payload pinned to the staged inode" {
		t.Fatalf("restored parent publication bytes=%q err=%v", got, err)
	}
	filesForItem, err := reopened.ListFilesForItem(ctx, itemID)
	if err != nil || len(filesForItem) != 1 || filesForItem[0].Path != destination {
		t.Fatalf("restored parent did not commit exact placement: files=%+v err=%v", filesForItem, err)
	}
}

func TestRecoveryPlacementObservationChangeDoesNotPublishAndStableRetryCommits(t *testing.T) {
	ctx := context.Background()
	svc, db, itemID := setup(t, nil, &fakeClient{})
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "recovery-source.mkv")
	payload := []byte("recovery selection remains exact")
	if err = os.WriteFile(source, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(item.Path, "recovery-observation.mkv")
	reads := 0
	recovery := &recoveryPlacementContext{
		Files: map[string]string{source: "recovery-file-1"},
		ObserveFinal: func() (string, error) {
			reads++
			if reads == 1 {
				return "reviewed-library-observation", nil
			}
			return "changed-library-observation", nil
		},
	}
	ctx = context.WithValue(ctx, recoveryPlacementKey{}, recovery)
	if _, err = svc.commitPlacement(ctx, item, importScope{Attempt: "recovery-observation"}, source, destination, quality.Quality{}, nil, false); err == nil {
		t.Fatal("recovery placement published after its final library observation changed")
	}
	if _, statErr := os.Lstat(destination); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("changed recovery observation published a target: %v", statErr)
	}
	if got, readErr := os.ReadFile(source); readErr != nil || string(got) != string(payload) {
		t.Fatalf("recovery source changed on held publication: %q err=%v", got, readErr)
	}
	placements, err := db.PendingPlacements(context.Background())
	if err != nil || len(placements) != 1 || placements[0].State != "prepared" {
		t.Fatalf("held recovery did not retain exact prepared intent: rows=%+v err=%v", placements, err)
	}

	recovery.ObserveFinal = func() (string, error) { return "stable-reviewed-library-observation", nil }
	if _, err = svc.commitPlacement(ctx, item, importScope{Attempt: "recovery-observation"}, source, destination, quality.Quality{}, nil, false); err != nil {
		t.Fatalf("stable recovery retry did not complete the exact intent: %v", err)
	}
	got, err := os.ReadFile(destination)
	if err != nil || string(got) != string(payload) {
		t.Fatalf("stable recovery retry destination=%q err=%v", got, err)
	}
	files, err := db.ListFilesForItem(context.Background(), itemID)
	if err != nil || len(files) != 1 || files[0].Path != destination {
		t.Fatalf("stable recovery retry receipt=%+v err=%v", files, err)
	}
}
