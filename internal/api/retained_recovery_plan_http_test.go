package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pjunod/monarr/internal/domain"
)

func TestPlanRetainedRecoveryHTTPIsBoundedReadOnlyAndReportsEvidence(t *testing.T) {
	e := newAPIEnv(t)
	ctx := context.Background()
	itemID, err := e.db.CreateMediaItem(ctx, domain.MediaItem{
		Kind: domain.KindMovie, Title: "Recovery Plan Feature", SortTitle: "recovery plan feature", Path: e.root,
	})
	if err != nil {
		t.Fatal(err)
	}
	beforeRevision, err := e.db.LibraryRevision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	data := []byte("verified retained movie payload")
	first := filepath.Join(root, "Recovery.Plan.Feature.1080p.WEB-DL.mkv")
	duplicate := filepath.Join(root, "Recovery.Plan.Feature.copy.mkv")
	if err := os.WriteFile(first, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(duplicate, data, 0600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	candidates := []map[string]any{
		{"path": first, "media_item_id": itemID, "copy_id": 0, "evidence": "finalized CRC and digest", "expected_sha256": hex.EncodeToString(digest[:]), "complete_coverage": true},
		{"path": duplicate, "media_item_id": itemID, "copy_id": 0, "evidence": "same bytes from second source", "expected_sha256": hex.EncodeToString(digest[:]), "complete_coverage": true},
		{"path": first, "media_item_id": itemID, "copy_id": 0, "evidence": "partial input", "expected_sha256": hex.EncodeToString(digest[:]), "complete_coverage": true, "partial": true},
		{"path": first, "media_item_id": itemID, "copy_id": 0, "evidence": "digest mismatch", "expected_sha256": strings.Repeat("0", 64), "complete_coverage": true},
	}
	body, err := json.Marshal(candidates)
	if err != nil {
		t.Fatal(err)
	}
	response := e.post(t, "/api/v1/import/recovery/retained/plan", string(body)).expect(t, 200)
	var plan struct {
		LibraryRevision    int64 `json:"library_revision"`
		Provisional        bool  `json:"provisional"`
		MutationAuthorized bool  `json:"mutation_authorized"`
		Files              []struct {
			Path        string `json:"path"`
			Action      string `json:"action"`
			SHA256      string `json:"sha256"`
			DuplicateOf string `json:"duplicate_of"`
			Blocked     string `json:"blocked_reason"`
			Destination string `json:"destination"`
		} `json:"files"`
	}
	response.into(t, &plan)
	if plan.LibraryRevision != beforeRevision || plan.Provisional || plan.MutationAuthorized || len(plan.Files) != len(candidates) {
		t.Fatalf("unexpected dry-run response: %+v", plan)
	}
	if plan.Files[0].Action != "preview_through_placement_coordinator" || plan.Files[0].SHA256 != hex.EncodeToString(digest[:]) || plan.Files[0].Destination == "" {
		t.Fatalf("verified candidate was not planned: %+v", plan.Files[0])
	}
	if plan.Files[1].DuplicateOf != first {
		t.Fatalf("duplicate evidence was not reported: %+v", plan.Files[1])
	}
	if plan.Files[2].Action != "hold" || !strings.Contains(plan.Files[2].Blocked, "partial") {
		t.Fatalf("partial evidence was not held: %+v", plan.Files[2])
	}
	if plan.Files[3].Action != "hold" || !strings.Contains(plan.Files[3].Blocked, "unverified") {
		t.Fatalf("wrong digest was not held: %+v", plan.Files[3])
	}
	if got, err := os.ReadFile(first); err != nil || string(got) != string(data) {
		t.Fatalf("plan altered source bytes: %q %v", got, err)
	}
	files, err := e.db.ListFilesForItem(ctx, itemID)
	if err != nil || len(files) != 0 {
		t.Fatalf("plan published files: %+v %v", files, err)
	}
	afterRevision, err := e.db.LibraryRevision(ctx)
	if err != nil || afterRevision != beforeRevision {
		t.Fatalf("plan changed library revision: before=%d after=%d err=%v", beforeRevision, afterRevision, err)
	}
	e.post(t, "/api/v1/import/recovery/retained/plan", `{`).expect(t, 400)
	oversizedBody := `{"padding":"` + strings.Repeat("x", 1<<20) + `"}`
	e.post(t, "/api/v1/import/recovery/retained/plan", oversizedBody).expect(t, 400)
	tooMany := make([]map[string]any, 1001)
	for i := range tooMany {
		tooMany[i] = map[string]any{"path": "/not-read", "media_item_id": itemID, "evidence": fmt.Sprintf("candidate-%d", i)}
	}
	oversizedList, err := json.Marshal(tooMany)
	if err != nil {
		t.Fatal(err)
	}
	rejected := e.post(t, "/api/v1/import/recovery/retained/plan", string(oversizedList)).expect(t, 409)
	if !strings.Contains(rejected.Body.String(), "file limit") {
		t.Fatalf("unexpected candidate-limit response: %s", rejected.Body)
	}
	if got, err := os.ReadFile(duplicate); err != nil || string(got) != string(data) {
		t.Fatalf("rejected plan altered duplicate source: %q %v", got, err)
	}
}
