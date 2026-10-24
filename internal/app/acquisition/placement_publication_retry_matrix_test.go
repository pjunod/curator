package acquisition

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/pjunod/monarr/internal/infra/sqlite"
)

func TestPlacementPublicationAuthorityObstructionsRetryWithoutOverwriting(t *testing.T) {
	for _, scenario := range []string{"missing_stage", "foreign_target", "foreign_symlink_target", "foreign_directory_target"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			svc, db, itemID := setup(t, nil, &fakeClient{})
			item, err := db.GetMediaItemFull(ctx, itemID)
			if err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(t.TempDir(), "accepted-source.mkv")
			destination := filepath.Join(item.Path, "placement-obstruction.mkv")
			payload := []byte("accepted placement payload after obstruction recovery")
			if err := os.WriteFile(source, payload, 0o600); err != nil {
				t.Fatal(err)
			}
			digest, size, _, err := fileDigestSnapshot(ctx, source)
			if err != nil {
				t.Fatal(err)
			}
			p := sqlite.Placement{ID: "placement-obstruction-" + scenario, Source: source, Target: destination,
				Temporary: filepath.Join(item.Path, ".monarr-stage-obstruction-"+scenario),
				Backup:    filepath.Join(item.Path, ".monarr-prior-obstruction-"+scenario), SHA256: digest, Size: size,
				ItemID: itemID, State: "prepared"}
			if err := db.PreparePlacement(ctx, p); err != nil {
				t.Fatal(err)
			}
			if err := svc.preparePlacementPublication(ctx, &p); err != nil {
				t.Fatalf("prepare exact staged intent: %v", err)
			}

			var foreignPath string
			switch scenario {
			case "missing_stage":
				if err := os.Remove(p.Temporary); err != nil {
					t.Fatal(err)
				}
			case "foreign_target":
				foreignPath = destination
				if err := os.WriteFile(destination, []byte("unrelated library bytes"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "foreign_symlink_target":
				foreignPath = destination
				foreignTarget := filepath.Join(t.TempDir(), "outside-library-file")
				if err := os.WriteFile(foreignTarget, []byte("outside bytes remain untouched"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(foreignTarget, destination); err != nil {
					t.Fatal(err)
				}
			case "foreign_directory_target":
				foreignPath = destination
				if err := os.Mkdir(destination, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(destination, "retain.keep"), []byte("foreign directory"), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			decisionCtx, release := svc.lockStorageDecision(ctx)
			err = svc.publishPlacement(decisionCtx, &p)
			release()
			if err == nil {
				t.Fatal("publication crossed a missing-stage or foreign-target boundary")
			}
			if got, readErr := os.ReadFile(source); readErr != nil || string(got) != string(payload) {
				t.Fatalf("failed publication changed accepted source: %q err=%v", got, readErr)
			}
			if scenario == "foreign_target" {
				if got, readErr := os.ReadFile(foreignPath); readErr != nil || string(got) != "unrelated library bytes" {
					t.Fatalf("foreign target was overwritten: %q err=%v", got, readErr)
				}
			} else if scenario == "foreign_symlink_target" {
				info, statErr := os.Lstat(foreignPath)
				if statErr != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Fatalf("foreign target link was consumed: info=%v err=%v", info, statErr)
				}
			} else if scenario == "foreign_directory_target" {
				if got, readErr := os.ReadFile(filepath.Join(foreignPath, "retain.keep")); readErr != nil || string(got) != "foreign directory" {
					t.Fatalf("foreign directory target was altered: %q err=%v", got, readErr)
				}
			} else if _, statErr := os.Lstat(destination); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("missing-stage failure published target bytes: %v", statErr)
			}

			// Recover from the persisted intent after discarding process state.
			reopened := reopenLifecycleDB(t, db)
			svc = New(reopened, nil, nil, nil, nil)
			db = reopened
			p, err = reopened.GetPlacement(ctx, p.ID)
			if err != nil || p.State != "prepared" {
				t.Fatalf("restart did not retain prepared placement authority: placement=%+v err=%v", p, err)
			}

			if scenario == "missing_stage" {
				if err := svc.preparePlacementPublication(ctx, &p); err != nil {
					t.Fatalf("rebuild exact stage from accepted source: %v", err)
				}
			} else if scenario == "foreign_directory_target" {
				if err := os.RemoveAll(foreignPath); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Remove(foreignPath); err != nil {
				t.Fatal(err)
			}
			decisionCtx, release = svc.lockStorageDecision(ctx)
			if err = svc.publishPlacement(decisionCtx, &p); err == nil {
				_, err = db.CommitPlacement(decisionCtx, p)
			}
			release()
			if err != nil {
				t.Fatalf("exact intent did not retry after the obstruction was removed: %v", err)
			}
			got, err := os.ReadFile(destination)
			if err != nil || string(got) != string(payload) {
				t.Fatalf("retry target bytes=%q err=%v", got, err)
			}
			files, err := db.ListFilesForItem(ctx, itemID)
			if err != nil || len(files) != 1 || files[0].Path != destination {
				t.Fatalf("retry did not commit exact library ownership: files=%+v err=%v", files, err)
			}
		})
	}
}
