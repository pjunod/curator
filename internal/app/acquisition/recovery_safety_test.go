package acquisition

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

func TestRecoverySeriesMappingAndCopyRootSurviveImport(t *testing.T) {
	svc, db, req, receipts := recoveryNamedFixture(t, "obfuscated.bin", false)
	ctx := context.Background()
	root, err := db.AddRootFolder(ctx, t.TempDir(), domain.KindMixed)
	if err != nil {
		t.Fatal(err)
	}
	item, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindSeries, Title: "Recovery Show", Path: filepath.Join(root.Path, "primary"), RootFolderID: root.ID, QualityProfileID: 1, Seasons: []domain.Season{{Number: 1, Episodes: []domain.Episode{{SeasonNumber: 1, EpisodeNumber: 1, Title: "Pilot"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	cp, err := db.AddMediaCopy(ctx, domain.MediaCopy{MediaItemID: item, Name: "Archive", Path: filepath.Join(root.Path, "archive"), RootFolderID: root.ID, QualityProfileID: 1})
	if err != nil {
		t.Fatal(err)
	}
	req.MediaItemID, req.CopyID = item, cp
	preview, err := svc.PreviewRecovery(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Files[0].Usable || !strings.Contains(preview.Files[0].Reason, "mapping") {
		t.Fatalf("unmapped episode accepted: %+v", preview)
	}
	req.EpisodeTargets = map[string]RecoveryEpisodeTarget{"file": {Season: 1, Episodes: []int{99}}}
	if _, err = svc.PreviewRecovery(ctx, req); err == nil {
		t.Fatal("unknown episode accepted")
	}
	req.EpisodeTargets["file"] = RecoveryEpisodeTarget{Season: 1, Episodes: []int{1}}
	preview, err = svc.PreviewRecovery(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(preview.Files[0].Destination, filepath.Join(root.Path, "archive")) {
		t.Fatalf("wrong copy destination: %+v", preview)
	}
	job, err := svc.QueueRecovery(ctx, preview.Request)
	if err != nil {
		t.Fatal(err)
	}
	svc.recoverySweep(ctx)
	result, err := svc.RecoveryImport(ctx, job.ID)
	if err != nil || result.State != "imported" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	files, err := db.ListFilesForItem(ctx, item)
	if err != nil || len(files) != 1 || files[0].CopyID != cp || receipts.Load() != 1 {
		t.Fatalf("files=%+v receipts=%d err=%v", files, receipts.Load(), err)
	}
	ep, err := db.GetEpisodeID(ctx, item, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	var links int
	if err = db.R.QueryRowContext(ctx, `SELECT count(*) FROM media_file_episodes WHERE episode_id=?`, ep).Scan(&links); err != nil || links != 1 {
		t.Fatalf("episode links=%d err=%v", links, err)
	}
}

func TestRecoveryReviewRetryAndChangedPublishedBytes(t *testing.T) {
	svc, db, req, receipts := recoveryFixture(t)
	ctx := context.Background()
	preview, err := svc.PreviewRecovery(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	job, err := svc.QueueRecovery(ctx, preview.Request)
	if err != nil {
		t.Fatal(err)
	}
	job.State, job.Error = "review", "retry after interruption"
	if err = svc.saveRecoveryImport(ctx, job); err != nil {
		t.Fatal(err)
	}
	retried, err := svc.QueueRecovery(ctx, preview.Request)
	if err != nil || retried.State != "queued" || retried.Error != "" {
		t.Fatalf("retry=%+v err=%v", retried, err)
	}
	svc.recoverySweep(ctx)
	done, err := svc.RecoveryImport(ctx, job.ID)
	if err != nil || done.State != "imported" {
		t.Fatalf("done=%+v err=%v", done, err)
	}
	files, err := db.ListFilesForItem(ctx, req.MediaItemID)
	if err != nil || len(files) != 1 {
		t.Fatalf("files=%v err=%v", files, err)
	}
	if err = os.WriteFile(files[0].Path, []byte("external replacement"), 0644); err != nil {
		t.Fatal(err)
	}
	if err = svc.verifyRecoveryPublications(ctx, done); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("changed publication accepted: %v", err)
	}
	if err = svc.CancelRecoveryImport(ctx, job.ID); err == nil {
		t.Fatal("committed import cancellation accepted")
	}
	imports, err := svc.RecoveryImports(ctx)
	if err != nil || len(imports) != 1 || imports[0].State != "imported" {
		t.Fatalf("imports=%v err=%v", imports, err)
	}
	metrics, err := svc.RecoveryMetrics(ctx)
	if err != nil || metrics["imported"] != 1 || metrics["receipt_outbox_pending"] != 0 || receipts.Load() != 1 {
		t.Fatalf("metrics=%v err=%v", metrics, err)
	}
}

func TestPlacementRestartNeverOverwritesUnrelatedBytes(t *testing.T) {
	for _, scenario := range []string{"changed-target", "changed-stage", "changed-backup", "cancel-changed-target", "cancel-changed-backup", "cancel-missing-target", "cancel-new-target"} {
		t.Run(scenario, func(t *testing.T) {
			svc, db, item := autoSetup(t, nil, &fakeClient{})
			ctx := context.Background()
			root := t.TempDir()
			write := func(name, body string) string {
				p := filepath.Join(root, name)
				if err := os.WriteFile(p, []byte(body), 0644); err != nil {
					t.Fatal(err)
				}
				return p
			}
			digest := func(s string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(s))) }
			p := sqlite.Placement{ID: scenario, Source: write("source", "new"), Target: write("target", "old"), Temporary: filepath.Join(root, "stage"), Backup: filepath.Join(root, "backup"), SHA256: digest("new"), PreviousSHA256: digest("old"), Size: 3, ItemID: item, State: "prepared"}
			switch scenario {
			case "changed-target":
				write("target", "external")
			case "changed-stage":
				write("stage", "external")
			case "changed-backup":
				write("backup", "external")
			case "cancel-changed-target":
				write("target", "external")
			case "cancel-changed-backup":
				write("target", "new")
				write("backup", "external")
			case "cancel-missing-target":
				if err := os.Remove(p.Target); err != nil {
					t.Fatal(err)
				}
				write("backup", "old")
			case "cancel-new-target":
				write("target", "new")
				p.PreviousSHA256 = ""
			}
			if err := db.PreparePlacement(ctx, p); err != nil {
				t.Fatal(err)
			}
			var err error
			if strings.HasPrefix(scenario, "cancel") {
				err = svc.rollbackPlacement(ctx, p)
			} else {
				err = svc.publishPlacement(ctx, p)
			}
			success := scenario == "cancel-missing-target" || scenario == "cancel-new-target"
			if (err == nil) != success {
				t.Fatalf("error=%v success=%v", err, success)
			}
			if success {
				var state string
				if err = db.R.QueryRowContext(ctx, `SELECT state FROM import_placements WHERE id=?`, p.ID).Scan(&state); err != nil || state != "rolled_back" {
					t.Fatalf("state=%s err=%v", state, err)
				}
				if scenario == "cancel-new-target" {
					if _, err = os.Stat(p.Target); !os.IsNotExist(err) {
						t.Fatalf("new target survived: %v", err)
					}
				} else {
					got, err := os.ReadFile(p.Target)
					if err != nil || string(got) != "old" {
						t.Fatalf("old target not restored: %q %v", got, err)
					}
				}
			} else {
				expected := "old"
				if strings.Contains(scenario, "changed-target") {
					expected = "external"
				}
				if scenario == "cancel-changed-backup" {
					expected = "new"
				}
				got, err := os.ReadFile(p.Target)
				if err != nil || string(got) != expected {
					t.Fatalf("target was overwritten: %q %v", got, err)
				}
			}
		})
	}
}

func TestRecoveryRootIdentityAndOverlapRequireReview(t *testing.T) {
	svc, db, req, _ := recoveryFixture(t)
	ctx := context.Background()
	cfg, err := svc.RecoverySettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	root, err := db.AddRootFolder(ctx, t.TempDir(), domain.KindMixed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.W.ExecContext(ctx, `UPDATE media_items SET root_folder_id=? WHERE id=?`, root.ID, req.MediaItemID); err != nil {
		t.Fatal(err)
	}
	before, _, err := svc.targetSnapshot(ctx, req.MediaItemID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(root.Path, root.Path+"-original"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(root.Path + "-original") })
	if err = os.Mkdir(root.Path, 0755); err != nil {
		t.Fatal(err)
	}
	after, _, err := svc.targetSnapshot(ctx, req.MediaItemID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if before.Metadata == after.Metadata {
		t.Fatal("replaced root identity was not detected")
	}
	cfg.LocalRoot = root.Path
	if err = svc.SetRecoverySettings(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.PreviewRecovery(ctx, req); err == nil || !strings.Contains(err.Error(), "overlaps") {
		t.Fatalf("overlapping mount accepted: %v", err)
	}
}
