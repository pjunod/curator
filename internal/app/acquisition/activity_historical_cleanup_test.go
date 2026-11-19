package acquisition

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

func TestHistoricalFailedPublicationsSweepOnlyWithCompletePositiveEvidence(t *testing.T) {
	for _, policy := range []string{"fully accounted", "undecided file", "paused"} {
		t.Run(policy, func(t *testing.T) {
			f := setupV2(t, nil)
			dl, err := f.db.InsertDownloadRecord(f.ctx, sqlite.Download{MediaItemID: f.item, Protocol: "manual", State: "failed", ReleaseTitle: "Proven old failure"})
			if err != nil {
				t.Fatal(err)
			}
			pub := f.publish("old-failed-pack", "old-failure-generation", map[string]string{"episode.mkv": "owned failed media", "notes.txt": "owned sidecar"})
			raw, err := os.ReadFile(filepath.Join(pub.Path, ports.ManifestFileName))
			if err != nil {
				t.Fatal(err)
			}
			var manifest ports.Manifest
			if err = json.Unmarshal(raw, &manifest); err != nil {
				t.Fatal(err)
			}
			id, err := f.db.InsertPublication(f.ctx, sqlite.PublicationWrite{DownloadID: dl.ID, Kind: "completed", Path: pub.Path, Generation: pub.Generation, Cohort: "historical", Source: "file", ManifestJSON: string(raw)})
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range manifest.Files {
				file := sqlite.PublicationFile{PublicationID: id, Path: m.Path, Bytes: m.Bytes, SHA256: m.SHA256, Outcome: "discarded", DiscardReason: "rejected"}
				if policy == "undecided file" && m.Path == "episode.mkv" {
					file.Outcome = "unresolved"
					file.DiscardReason = ""
					file.Reason = "kept by an operator in Resolve"
				}
				if err = f.db.SeedPublicationFiles(f.ctx, id, []sqlite.PublicationFile{file}); err != nil {
					t.Fatal(err)
				}
				if err = f.db.SetPublicationFileOutcome(f.ctx, file); err != nil {
					t.Fatal(err)
				}
			}
			if err = f.db.SetPublicationState(f.ctx, id, "import_failed", ""); err != nil {
				t.Fatal(err)
			}
			pause := 0
			if policy == "paused" {
				pause = 1
			}
			if _, err = f.db.W.ExecContext(f.ctx, `UPDATE publications SET updated_at=1,cleanup_paused_at=? WHERE id=?`, pause, id); err != nil {
				t.Fatal(err)
			}
			if err = f.svc.sweepPublications(f.ctx); err != nil {
				t.Fatal(err)
			}
			stored, err := f.db.GetPublication(f.ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if policy == "fully accounted" {
				if stored.State != "removed" || stored.CleanupApprovedAt.IsZero() || exists(pub.Path) {
					t.Fatalf("positively owned old failure did not drain through sweep: %+v", stored)
				}
			} else {
				if stored.State == "removed" || !stored.CleanupApprovedAt.IsZero() || !exists(filepath.Join(pub.Path, "episode.mkv")) {
					t.Fatalf("policy/unknown ownership was bypassed: %+v", stored)
				}
				if policy == "undecided file" && stored.UpdatedAt.UnixMilli() <= 1 {
					t.Fatal("unapproved history can starve the bounded sweep")
				}
			}
		})
	}
}
