package acquisition

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pjunod/monarr/internal/infra/sqlite"
)

func TestActivityAbsentPublicationStillAccountsForRecordedLibraryWork(t *testing.T) {
	for _, scenario := range []string{"no placed work", "placed work needs its manifest", "invalid recorded manifest", "different manifest generation"} {
		t.Run(scenario, func(t *testing.T) {
			f := setupV2(t, nil)
			dl, err := f.db.InsertDownloadRecord(f.ctx, sqlite.Download{MediaItemID: f.item, Protocol: "manual", State: "failed", ReleaseTitle: "Absent publication"})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(f.root, "already-removed")
			publication := sqlite.PublicationWrite{DownloadID: dl.ID, Kind: "completed", Path: path, Generation: "original-publication", Cohort: "historical", Source: "none"}
			switch scenario {
			case "invalid recorded manifest":
				publication.Source = "file"
				publication.ManifestJSON = "invalid cached manifest"
			case "different manifest generation":
				publication.Source = "file"
				publication.ManifestJSON = `{"generation":"another-publication","files":[{"path":"episode.mkv","bytes":4}]}`
			}
			id, err := f.db.InsertPublication(f.ctx, publication)
			if err != nil {
				t.Fatal(err)
			}
			library := writeFile(t, filepath.Join(t.TempDir(), "library.mkv"), "independent installed bytes")
			if scenario == "placed work needs its manifest" {
				p := sqlite.Placement{ID: "old-placement", DownloadID: dl.ID, SourceGeneration: dl.GenerationKey, SourcePayloadGeneration: publication.Generation, Source: filepath.Join(path, "episode.mkv"), Target: library, State: "cleaned"}
				raw, err := json.Marshal(p)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = f.db.W.ExecContext(f.ctx, `INSERT INTO import_placements(id,target,state,data,updated_at) VALUES(?,?,'cleaned',?,1)`, p.ID, p.Target, string(raw)); err != nil {
					t.Fatal(err)
				}
			}
			const op = "delete-mounted-absent-publication"
			job := claimActivityAction(t, f.db, op, "delete", sqlite.ActivityIdentity{ID: dl.ID, GenerationKey: dl.GenerationKey, Epoch: "0"})
			if err = f.svc.handleActivityAction(f.ctx, job); err != nil {
				t.Fatal(err)
			}
			operation, err := f.db.ActivityOperation(f.ctx, op)
			if err != nil {
				t.Fatal(err)
			}
			stored, err := f.db.GetPublication(f.ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			_, verified, err := f.db.DownloadDeletionStatus(f.ctx, dl.ID, dl.GenerationKey)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "no placed work" {
				if operation.Succeeded != 1 || !verified || stored.State != "removed" || stored.CleanupApprovedAt.IsZero() {
					t.Fatalf("mounted absence failed to settle: %+v %+v verified=%v", operation, stored, verified)
				}
			} else {
				if operation.Blocked != 1 || verified || stored.State == "removed" {
					t.Fatalf("missing accounting was acknowledged: %+v %+v verified=%v", operation, stored, verified)
				}
				if !strings.Contains(operation.Targets[0].Message, "manifest") {
					t.Fatalf("missing prerequisite: %+v", operation)
				}
			}
			if got, err := os.ReadFile(library); err != nil || string(got) != "independent installed bytes" {
				t.Fatalf("installed file changed: %q, %v", got, err)
			}
		})
	}
}
