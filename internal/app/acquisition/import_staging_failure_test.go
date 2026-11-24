package acquisition

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/pjunod/monarr/internal/infra/sqlite"
)

// A failed stage write never publishes; what it leaves is scratch that the
// next attempt of the placement discards (TestRetryDiscardsAnInterruptedStage).
func TestJournaledStageFailuresPublishNothing(t *testing.T) {
	for _, failure := range []string{"missing-source", "occupied-stage", "unpersisted-stage", "cancelled", "changed-size"} {
		t.Run(failure, func(t *testing.T) {
			svc, db, itemID := setup(t, nil, &fakeClient{})
			ctx := context.Background()
			root := t.TempDir()
			source := filepath.Join(root, "source")
			target := filepath.Join(root, "stage")
			if e := os.WriteFile(source, []byte("payload"), 0400); e != nil {
				t.Fatal(e)
			}
			dl := insertImportable(t, db, itemID, source)
			a, e := db.EnsureImportAttempt(ctx, dl.ID, dl.GenerationKey)
			if e != nil {
				t.Fatal(e)
			}
			epoch := a.Epoch
			p := sqlite.Placement{ID: "stage-fault", DownloadID: dl.ID, ImportEpoch: &epoch, SourceGeneration: dl.GenerationKey, ItemID: itemID, Source: source, Target: filepath.Join(root, "published"), Temporary: target, State: "prepared", Size: 7}
			if failure != "unpersisted-stage" {
				if e = db.PreparePlacement(ctx, p); e != nil {
					t.Fatal(e)
				}
			}
			switch failure {
			case "missing-source":
				p.Source = source + "-missing"
			case "occupied-stage":
				if e = os.WriteFile(target, []byte("prior owned bytes"), 0600); e != nil {
					t.Fatal(e)
				}
			case "changed-size":
				p.Size = 99
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			e = svc.stageImportPlacement(ctx, &p, nil)
			if e == nil {
				t.Fatal("failed stage reported success")
			}
			if failure == "cancelled" && !errors.Is(e, context.Canceled) {
				t.Fatalf("lost cancellation: %v", e)
			}
			if _, err := os.Stat(p.Target); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failure published bytes", err)
			}
			if failure == "occupied-stage" {
				got, err := os.ReadFile(target)
				if err != nil || string(got) != "prior owned bytes" {
					t.Fatalf("occupied stage changed: %q %v", got, err)
				}
			}
			if failure == "changed-size" {
				got, err := os.ReadFile(target)
				if err != nil || string(got) != "payload" {
					t.Fatalf("partial stage lost: %q %v", got, err)
				}
			}
		})
	}
}
