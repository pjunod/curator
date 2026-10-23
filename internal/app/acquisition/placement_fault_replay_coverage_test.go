package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/infra/bus"
	"github.com/pjunod/monarr/internal/ports"
)

func TestPlacementFaultsPreserveReplayableAuthorityAcrossDatabaseReopen(t *testing.T) {
	tests := []struct {
		name          string
		trigger       string
		when          string
		failureWindow string
	}{
		{
			name:          "prepare journal insert fails before staging",
			trigger:       "fail_prepare_placement",
			when:          "BEFORE INSERT ON import_placements",
			failureWindow: "prepare",
		},
		{
			name:          "staged identity write fails and same request retries",
			trigger:       "fail_staged_identity",
			when:          "BEFORE UPDATE OF data ON import_placements WHEN COALESCE(json_extract(NEW.data,'$.temporary_object_identity'),'') <> ''",
			failureWindow: "staged",
		},
		{
			name:          "published identity write fails and startup reconciles",
			trigger:       "fail_published_identity",
			when:          "BEFORE UPDATE OF data ON import_placements WHEN COALESCE(json_extract(NEW.data,'$.published_file_generation'),'') <> ''",
			failureWindow: "published",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			svc, db, itemID := setup(t, nil, &fakeClient{})
			item, err := db.GetMediaItemFull(ctx, itemID)
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			source := filepath.Join(root, "accepted-source.mkv")
			destination := filepath.Join(item.Path, "placement-fault-replay.mkv")
			payload := []byte("accepted placement payload for durable fault replay")
			if err := os.WriteFile(source, payload, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := db.W.ExecContext(ctx, "CREATE TRIGGER "+tc.trigger+" "+tc.when+" BEGIN SELECT RAISE(ABORT,'injected placement persistence failure'); END"); err != nil {
				t.Fatal(err)
			}

			_, firstErr := svc.commitPlacement(ctx, item, importScope{Attempt: "fault-replay"}, source, destination, quality.Quality{}, nil, false)
			if firstErr == nil || !strings.Contains(firstErr.Error(), "injected placement persistence failure") {
				t.Fatalf("trigger did not interrupt the intended %s persistence boundary: %v", tc.failureWindow, firstErr)
			}
			if got, err := os.ReadFile(source); err != nil || string(got) != string(payload) {
				t.Fatalf("source changed at %s boundary: %q err=%v", tc.failureWindow, got, err)
			}

			switch tc.failureWindow {
			case "prepare":
				var placements int
				if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM import_placements`).Scan(&placements); err != nil || placements != 0 {
					t.Fatalf("failed prepare left placement authority: rows=%d err=%v", placements, err)
				}
				if _, err := os.Stat(destination); !os.IsNotExist(err) {
					t.Fatalf("failed prepare published destination bytes: %v", err)
				}
				entries, err := os.ReadDir(item.Path)
				if err != nil {
					t.Fatal(err)
				}
				if len(entries) != 0 {
					t.Fatalf("failed prepare created staging or backup files: %v", entries)
				}

			case "staged", "published":
				pending, err := db.PendingPlacements(ctx)
				if err != nil || len(pending) != 1 || pending[0].State != "prepared" {
					t.Fatalf("fault did not retain one prepared journal: rows=%+v err=%v", pending, err)
				}
				p := pending[0]
				if tc.failureWindow == "staged" {
					if p.TemporaryObjectIdentity != "" || p.TemporaryFileGeneration != "" {
						t.Fatalf("failed staged-identity transaction partially advanced journal: %+v", p)
					}
					if got, err := os.ReadFile(p.Temporary); err != nil || string(got) != string(payload) {
						t.Fatalf("staged bytes were not preserved for replay: %q err=%v", got, err)
					}
					if _, err := os.Stat(destination); !os.IsNotExist(err) {
						t.Fatalf("staging failure published destination: %v", err)
					}
				} else {
					if p.TemporaryObjectIdentity == "" || p.TemporaryFileGeneration == "" || p.PublishedFileGeneration != "" {
						t.Fatalf("published-identity failure journal state was not exact: %+v", p)
					}
					if got, err := os.ReadFile(destination); err != nil || string(got) != string(payload) {
						t.Fatalf("published bytes were lost before recovery: %q err=%v", got, err)
					}
					if _, err := os.Stat(p.Temporary); !os.IsNotExist(err) {
						t.Fatalf("source stage should have been renamed before journal fault: %v", err)
					}
				}
			}

			reopened := reopenLifecycleDB(t, db)
			if _, err := reopened.W.ExecContext(ctx, "DROP TRIGGER "+tc.trigger); err != nil {
				t.Fatal(err)
			}
			restarted := New(reopened, bus.New(nil), nil, func(ports.IndexerConfig) ports.Indexer { return fakeIndexer{} }, func(ports.ClientConfig) ports.DownloadClient { return &fakeClient{} })
			if tc.failureWindow == "published" {
				restarted.reconcilePlacements(ctx)
			} else {
				reloadedItem, err := reopened.GetMediaItemFull(ctx, itemID)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = restarted.commitPlacement(ctx, reloadedItem, importScope{Attempt: "fault-replay"}, source, destination, quality.Quality{}, nil, false); err != nil {
					t.Fatalf("same placement did not replay after reopening: %v", err)
				}
			}

			got, err := os.ReadFile(destination)
			if err != nil || string(got) != string(payload) {
				t.Fatalf("replay destination bytes=%q err=%v", got, err)
			}
			files, err := reopened.ListFilesForItem(ctx, itemID)
			if err != nil || len(files) != 1 || files[0].Path != destination {
				t.Fatalf("replay library receipt rows=%+v err=%v", files, err)
			}
			pending, err := reopened.PendingPlacements(ctx)
			if err != nil || len(pending) != 0 {
				t.Fatalf("replay left unresolved placements: rows=%+v err=%v", pending, err)
			}
		})
	}
}

func TestPlacementReconcileDoesNotAdoptSameBytesFromDifferentPublishedGeneration(t *testing.T) {
	ctx := context.Background()
	svc, db, itemID := setup(t, nil, &fakeClient{})
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	source := filepath.Join(root, "accepted-source.mkv")
	destination := filepath.Join(item.Path, "same-bytes-new-generation.mkv")
	payload := []byte("published bytes whose inode must remain pinned")
	if err := os.WriteFile(source, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER fail_published_generation BEFORE UPDATE OF data ON import_placements WHEN COALESCE(json_extract(NEW.data,'$.published_file_generation'),'') <> '' BEGIN SELECT RAISE(ABORT,'injected published generation write failure'); END`); err != nil {
		t.Fatal(err)
	}
	_, err = svc.commitPlacement(ctx, item, importScope{Attempt: "published-generation-conflict"}, source, destination, quality.Quality{}, nil, false)
	if err == nil || !strings.Contains(err.Error(), "injected published generation write failure") {
		t.Fatalf("test did not stop after physical publication and before generation persistence: %v", err)
	}
	pending, err := db.PendingPlacements(ctx)
	if err != nil || len(pending) != 1 || pending[0].PublishedFileGeneration != "" || pending[0].TemporaryObjectIdentity == "" {
		t.Fatalf("expected a staged-object-pinned prepared journal: rows=%+v err=%v", pending, err)
	}
	acceptedInfo, err := os.Lstat(destination)
	if err != nil {
		t.Fatal(err)
	}
	acceptedIdentity := deliveryFileStableIdentity(acceptedInfo)

	// Keep the accepted inode alive, then create different bytes storage-wise
	// with the same digest at the destination. Reconciliation must require the
	// recorded publication object identity, not only equal contents.
	held := destination + ".held-original-publication"
	if err := os.Rename(destination, held); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(held) }()
	if err := os.WriteFile(destination, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	conflictingInfo, err := os.Lstat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if deliveryFileStableIdentity(conflictingInfo) == acceptedIdentity {
		t.Fatal("fixture did not create a distinct destination object")
	}

	reopened := reopenLifecycleDB(t, db)
	if _, err := reopened.W.ExecContext(ctx, `DROP TRIGGER fail_published_generation`); err != nil {
		t.Fatal(err)
	}
	restarted := New(reopened, bus.New(nil), nil, func(ports.IndexerConfig) ports.Indexer { return fakeIndexer{} }, func(ports.ClientConfig) ports.DownloadClient { return &fakeClient{} })
	restarted.reconcilePlacements(ctx)
	rows, err := reopened.PendingPlacements(ctx)
	if err != nil || len(rows) != 1 || rows[0].PublishedFileGeneration != "" {
		t.Fatalf("reconciliation adopted a different published object: rows=%+v err=%v", rows, err)
	}
	if got, err := os.ReadFile(destination); err != nil || string(got) != string(payload) {
		t.Fatalf("same-content conflicting target was changed: %q err=%v", got, err)
	}
	files, err := reopened.ListFilesForItem(ctx, itemID)
	if err != nil || len(files) != 0 {
		t.Fatalf("conflicting object gained library ownership: files=%+v err=%v", files, err)
	}
}
