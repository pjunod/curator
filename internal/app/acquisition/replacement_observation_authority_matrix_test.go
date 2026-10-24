package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// These cases exercise the asynchronous observation boundary after the user
// selected a file. The worker must bind its result to the same database row,
// copy root, and storage identity that the request observed.
func TestReplacementObservationRejectsStaleRowsAndStorage(t *testing.T) {
	for _, scenario := range []string{"outside_copy_root", "storage_identity_changed", "directory_substituted"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			svc, db, itemID := autoSetup(t, nil, &fakeClient{})
			item, err := db.GetMediaItemFull(ctx, itemID)
			if err != nil {
				t.Fatal(err)
			}
			fileID, source := acq2AddFile(t, svc, itemID, 0, item.Path, "observed-source.mkv", "Test.Movie.2024.720p.WEB-DL-GROUP")
			files, err := db.ListFilesForItem(ctx, itemID)
			if err != nil {
				t.Fatal(err)
			}
			var generation string
			for _, file := range files {
				if file.ID == fileID {
					generation = file.FileGeneration
				}
			}
			observation, _, err := svc.StartReplacementFileObservation(ctx, itemID, fileID, generation, "stale-observation-"+scenario)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "outside_copy_root":
				if _, err := db.W.ExecContext(ctx, `UPDATE media_items SET path=? WHERE id=?`, t.TempDir(), itemID); err != nil {
					t.Fatal(err)
				}
			case "storage_identity_changed":
				actual := replacementStorageIdentity
				replacementStorageIdentity = func(root string) (string, error) {
					identity, err := actual(root)
					if err != nil {
						return "", err
					}
					return identity + "-different-mount", nil
				}
				t.Cleanup(func() { replacementStorageIdentity = actual })
			case "directory_substituted":
				if err := os.Remove(source); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(source, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			svc.processReplacementFileObservations(ctx)
			got, err := svc.GetReplacementFileObservation(ctx, observation.ID)
			if err != nil || got.State != "stale" {
				t.Fatalf("changed file authority was accepted: observation=%+v err=%v", got, err)
			}
			if scenario == "storage_identity_changed" && got.Error != "selected storage mount changed" {
				t.Fatalf("storage change reason=%q", got.Error)
			}
			if scenario == "directory_substituted" && got.Error != "selected file is unavailable or not regular" {
				t.Fatalf("non-regular replacement reason=%q", got.Error)
			}
			if scenario == "outside_copy_root" {
				if got.Error != "selected file is outside the copy folder" {
					t.Fatalf("copy-root substitution reason=%q", got.Error)
				}
			}
		})
	}
}

func TestReplacementObservationTreatsMissingFileAsAbsenceOnlyWithPriorPin(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		t.Run(map[bool]string{false: "untrusted_storage", true: "previously_pinned_storage"}[trusted], func(t *testing.T) {
			ctx := context.Background()
			svc, db, itemID := autoSetup(t, nil, &fakeClient{})
			item, err := db.GetMediaItemFull(ctx, itemID)
			if err != nil {
				t.Fatal(err)
			}
			fileID, source := acq2AddFile(t, svc, itemID, 0, item.Path, "missing-reviewed-source.mkv", "Test.Movie.2024.720p.WEB-DL-GROUP")
			files, err := db.ListFilesForItem(ctx, itemID)
			if err != nil {
				t.Fatal(err)
			}
			var generation string
			for _, file := range files {
				if file.ID == fileID {
					generation = file.FileGeneration
				}
			}
			observation, _, err := svc.StartReplacementFileObservation(ctx, itemID, fileID, generation, "missing-observation-"+map[bool]string{false: "untrusted", true: "trusted"}[trusted])
			if err != nil {
				t.Fatal(err)
			}
			if trusted {
				if err := db.PinReplacementStorageIdentity(ctx, filepath.Clean(item.Path), observation.StorageIdentity, "prior-present-observation"); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Remove(source); err != nil {
				t.Fatal(err)
			}
			svc.processReplacementFileObservations(ctx)
			got, err := svc.GetReplacementFileObservation(ctx, observation.ID)
			if err != nil {
				t.Fatal(err)
			}
			if trusted {
				if got.State != "absent" || got.SHA256 != "" || got.Size != 0 || got.FileIdentity == "" {
					t.Fatalf("trusted absence was not durably observed: %+v", got)
				}
			} else if got.State != "stale" || got.Error != "storage identity unverified; restore expected storage" {
				t.Fatalf("untrusted missing file was adopted as absence: %+v", got)
			}
		})
	}
}

func TestReplacementObservationFinishFailureRemainsPendingAndReplays(t *testing.T) {
	ctx := context.Background()
	svc, db, itemID := autoSetup(t, nil, &fakeClient{})
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	fileID, _ := acq2AddFile(t, svc, itemID, 0, item.Path, "finish-retry.mkv", "Test.Movie.2024.720p.WEB-DL-GROUP")
	files, err := db.ListFilesForItem(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	var generation string
	for _, file := range files {
		if file.ID == fileID {
			generation = file.FileGeneration
		}
	}
	observation, _, err := svc.StartReplacementFileObservation(ctx, itemID, fileID, generation, "finish-retry")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER reject_observation_finish BEFORE UPDATE ON replacement_file_observations BEGIN SELECT RAISE(FAIL, 'injected observation finish outage'); END`); err != nil {
		t.Fatal(err)
	}
	svc.processReplacementFileObservations(ctx)
	pending, err := svc.GetReplacementFileObservation(ctx, observation.ID)
	if err != nil || pending.State != "pending" {
		t.Fatalf("failed durable finish was not retained for replay: observation=%+v err=%v", pending, err)
	}
	if _, err := db.W.ExecContext(ctx, `DROP TRIGGER reject_observation_finish`); err != nil {
		t.Fatal(err)
	}
	svc.processReplacementFileObservations(ctx)
	ready, err := svc.GetReplacementFileObservation(ctx, observation.ID)
	if err != nil || ready.State != "ready" || ready.SHA256 == "" {
		t.Fatalf("pending observation did not replay after storage recovery: observation=%+v err=%v", ready, err)
	}
}
