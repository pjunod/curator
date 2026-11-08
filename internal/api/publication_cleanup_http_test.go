package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/pjunod/monarr/internal/infra/sqlite"
)

// The M7 approval of a historical review batch (contract section 3.4): it
// sets cleanup_approved_at on the named historical publications only.
func TestApprovePublicationCleanupHTTP(t *testing.T) {
	e := newAPIEnv(t)
	ctx := t.Context()
	hist, err := e.db.InsertPublication(ctx, sqlite.PublicationWrite{Kind: "completed", Path: "/completed/old", Cohort: "historical"})
	if err != nil {
		t.Fatal(err)
	}
	live, err := e.db.InsertPublication(ctx, sqlite.PublicationWrite{Kind: "completed", Path: "/completed/new"})
	if err != nil {
		t.Fatal(err)
	}
	e.post(t, "/api/v1/downloads/publications/approve-cleanup", `{"ids":[]}`).expect(t, http.StatusBadRequest)
	body, _ := json.Marshal(map[string][]int64{"ids": {hist, live}})
	rr := e.post(t, "/api/v1/downloads/publications/approve-cleanup", string(body)).expect(t, http.StatusOK)
	var out struct{ Approved int64 }
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil || out.Approved != 1 {
		t.Fatalf("approved = %+v (%v), want only the historical one", out, err)
	}
	if p, _ := e.db.GetPublication(ctx, hist); p.CleanupApprovedAt.IsZero() {
		t.Fatal("historical publication not approved")
	}
	if p, _ := e.db.GetPublication(ctx, live); !p.CleanupApprovedAt.IsZero() {
		t.Fatal("a live publication was stamped")
	}
	// A second approval of the same batch approves nothing new.
	rr = e.post(t, "/api/v1/downloads/publications/approve-cleanup", string(body)).expect(t, http.StatusOK)
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil || out.Approved != 0 {
		t.Fatalf("re-approval = %+v", out)
	}
}
