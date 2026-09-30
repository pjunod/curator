package acquisition

import (
	"context"
	"crypto/sha256"
	"fmt"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRetainedPlanMixedReleasesPartialAndPendingReceipts(t *testing.T) {
	svc, db, item := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	root := t.TempDir()
	names := []string{"Monster.S01E01.HONE.mkv", "Monster.S01E02.BETTY.mkv", "Monster.S01E05.part", "Monster.S01E02.HONE.mkv"}
	candidates := []RetainedCandidate{}
	for i, name := range names {
		path := filepath.Join(root, name)
		data := []byte(fmt.Sprintf("synthetic verified episode %d", i%3))
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		episode := i + 1
		if i == 2 {
			episode = 5
		}
		if i == 3 {
			episode = 2
		}
		candidates = append(candidates, RetainedCandidate{Path: path, MediaItemID: item, Season: 1, Episodes: []int{episode}, Evidence: "synthetic full digest", ExpectedSHA256: fmt.Sprintf("%x", sha256.Sum256(data)), CompleteCoverage: true, Partial: i == 2})
	}
	plan, err := svc.PlanRetainedRecovery(ctx, candidates)
	if err != nil {
		t.Fatal(err)
	}
	if plan.MutationAuthorized || plan.Provisional || len(plan.Files) != 4 {
		t.Fatalf("invalid plan %+v", plan)
	}
	for _, row := range plan.Files[:2] {
		if row.Action != "preview_through_placement_coordinator" || row.Destination == "" || row.Identity == "" {
			t.Fatalf("missing exact plan %+v", row)
		}
	}
	if plan.Files[2].Action != "hold" || !strings.Contains(plan.Files[2].BlockedReason, "partial") {
		t.Fatalf("partial eligible %+v", plan.Files[2])
	}
	if plan.Files[3].DuplicateOf != candidates[0].Path {
		t.Fatalf("missing duplicate %+v", plan.Files[3])
	}
	if err := db.PreparePlacement(ctx, sqlite.Placement{ID: "pending-receipt", Source: candidates[0].Path, Target: plan.Files[0].Destination, State: "committed"}); err != nil {
		t.Fatal(err)
	}
	pending, err := svc.PlanRetainedRecovery(ctx, candidates[:1])
	if err != nil {
		t.Fatal(err)
	}
	if pending.Files[0].Action != "hold" || !strings.Contains(pending.Files[0].BlockedReason, "acknowledgement") {
		t.Fatalf("pending receipt %+v", pending)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE lifecycle_library_revision SET revision=revision+1 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if err := svc.ValidateRetainedPlan(ctx, plan); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale plan accepted %v", err)
	}
	for _, c := range candidates {
		if _, err := os.Stat(c.Path); err != nil {
			t.Fatal("dry run mutated source", err)
		}
	}
	files, err := db.ListFilesForItem(ctx, item)
	if err != nil || len(files) != 0 {
		t.Fatalf("dry run changed library %+v %v", files, err)
	}
}

func TestRetainedPlanActiveParentIsProvisionalAndFreshOverlapBlocksValidation(t *testing.T) {
	svc, db, item := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "nested.mkv")
	data := []byte("retained")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	candidate := RetainedCandidate{Path: path, MediaItemID: item, Season: 1, Episodes: []int{1}, ExpectedSHA256: fmt.Sprintf("%x", sha256.Sum256(data)), CompleteCoverage: true}
	plan, err := svc.PlanRetainedRecovery(ctx, []RetainedCandidate{candidate})
	if err != nil {
		t.Fatal(err)
	}
	id := grabEpisode(t, svc)
	row, err := db.GetDownload(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	row.SavePath = filepath.Dir(path)
	if err := db.UpdateDownloadHandoff(ctx, row); err != nil {
		t.Fatal(err)
	}
	if err := svc.ValidateRetainedPlan(ctx, plan); err == nil {
		t.Fatal("fresh active overlap accepted")
	}
	observed, err := svc.PlanRetainedRecovery(ctx, []RetainedCandidate{candidate})
	if err != nil {
		t.Fatal(err)
	}
	if !observed.Provisional || observed.Files[0].Action != "hold" {
		t.Fatalf("active parent not protected %+v", observed)
	}
}
