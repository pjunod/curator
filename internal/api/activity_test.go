package api

import (
	"context"
	"fmt"
	"testing"

	apigen "github.com/pjunod/monarr/internal/api/gen"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

func TestUnifiedActivityCompletePaginationAndRetainedImports(t *testing.T) {
	e := newAPIEnv(t)
	ctx := context.Background()
	item := e.addMovie(t)
	ids := map[int64]bool{}
	for i := 0; i < 213; i++ {
		id, err := e.db.InsertDownload(ctx, sqlite.Download{MediaItemID: item, State: "downloading", Protocol: "torrent", ReleaseTitle: fmt.Sprintf("active-%03d", i)})
		if err != nil {
			t.Fatal(err)
		}
		ids[id] = true
	}
	imported, err := e.db.InsertDownload(ctx, sqlite.Download{MediaItemID: item, State: "imported", Protocol: "manual", ReleaseTitle: "retained manual import"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.db.W.ExecContext(ctx, "UPDATE downloads SET activity_dismissed=1 WHERE id=?", imported); err != nil {
		t.Fatal(err)
	}
	// A publication already linked to a visible download must not add another row.
	for id := range ids {
		if _, err = e.db.InsertPublication(ctx, sqlite.PublicationWrite{DownloadID: id, Generation: "same-attempt", Kind: "completed", Path: "/completed/exact", Source: "file", Cohort: "live"}); err != nil {
			t.Fatal(err)
		}
		break
	}
	seen := map[int64]bool{}
	for offset := 0; offset < 213; offset += 50 {
		var page apigen.ImportActivityPage
		e.get(t, fmt.Sprintf("/api/v1/activity?view=progress&limit=50&offset=%d", offset)).expect(t, 200).into(t, &page)
		if page.Total != 213 || page.Counts["progress"] != 213 || page.Counts["history"] != 1 {
			t.Fatalf("incomplete counts: %+v", page)
		}
		if page.HasMore != (offset+50 < 213) {
			t.Fatalf("bad continuation at %d", offset)
		}
		for _, row := range page.Items {
			if seen[row.Download.Id] || !ids[row.Download.Id] {
				t.Fatalf("duplicate/unexpected identity %d", row.Download.Id)
			}
			seen[row.Download.Id] = true
		}
	}
	if len(seen) != 213 {
		t.Fatal(len(seen))
	}
	var history apigen.ImportActivityPage
	e.get(t, "/api/v1/activity?view=history&scope=imports").expect(t, 200).into(t, &history)
	if history.Total != 1 || history.Items[0].Download.Id != imported {
		t.Fatalf("retained import lost: %+v", history)
	}
	var search apigen.ImportActivityPage
	e.get(t, "/api/v1/activity?q=active-212").expect(t, 200).into(t, &search)
	if search.Total != 1 || search.Counts["all"] != 1 {
		t.Fatalf("search counts: %+v", search)
	}
	e.get(t, "/api/v1/activity?scope=imports&view=progress").expect(t, 200).into(t, &search)
	if search.Total != 1 {
		t.Fatalf("imports-only scope includes unrelated fetches: %+v", search)
	}
	for _, query := range []string{"view=unknown", "scope=unknown", "limit=0", "limit=201", "offset=-1"} {
		e.get(t, "/api/v1/activity?"+query).expect(t, 400)
	}
}

func TestActivityLocalFailuresAndReleasedReservation(t *testing.T) {
	e := newAPIEnv(t)
	ctx := context.Background()
	item := e.addMovie(t)
	for _, row := range []sqlite.Download{
		{MediaItemID: item, State: "failed", Protocol: "torrent", ReleaseTitle: "local placement", ImportPath: "/completed/local", Handoff: []sqlite.HandoffEntry{{Step: "importing"}}},
		{MediaItemID: item, State: "failed", Protocol: "manual", ReleaseTitle: "manual placement", ImportPath: "/completed/manual"},
		{MediaItemID: item, State: "failed", Protocol: "torrent", ReleaseTitle: "environment", ImportPath: "/completed/retry", Error: "import stopped: unavailable mount"},
		{MediaItemID: item, State: "failed", Protocol: "torrent", ReleaseTitle: "transfer failed"},
	} {
		id, err := e.db.InsertDownload(ctx, row)
		if err != nil {
			t.Fatal(err)
		}
		row.ID = id
		if err = e.db.UpdateDownloadHandoff(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	id, err := e.db.InsertDownload(ctx, sqlite.Download{MediaItemID: item, State: "failed", Protocol: "torrent", ReleaseTitle: "uncertain", SubmissionPhase: "uncertain"})
	if err != nil {
		t.Fatal(err)
	}
	var page apigen.ImportActivityPage
	e.get(t, "/api/v1/activity?view=attention").expect(t, 200).into(t, &page)
	if page.Total != 3 || page.Counts["progress"] != 1 || page.Counts["history"] != 1 {
		t.Fatalf("wrong categories: %+v", page)
	}
	e.post(t, fmt.Sprintf("/api/v1/queue/%d/resolve-reservation", id), `{"acknowledgeDuplicateRisk":true}`).expect(t, 204)
	e.get(t, "/api/v1/activity?view=attention").expect(t, 200).into(t, &page)
	if page.Total != 2 || page.Counts["history"] != 2 {
		t.Fatalf("released reservation remains actionable: %+v", page)
	}
}

func TestActivityPartialApprovalRequiresDecisionUntilKept(t *testing.T) {
	e := newAPIEnv(t)
	ctx := context.Background()
	item := e.addMovie(t)
	id, err := e.db.InsertDownload(ctx, sqlite.Download{MediaItemID: item, State: "imported", Protocol: "usenet", ReleaseTitle: "partial approval"})
	if err != nil {
		t.Fatal(err)
	}
	pub, err := e.db.InsertPublication(ctx, sqlite.PublicationWrite{DownloadID: id, Generation: "partial", Kind: "completed", Path: "/completed/partial", Source: "file", Cohort: "live"})
	if err != nil {
		t.Fatal(err)
	}
	if err = e.db.SeedPublicationFiles(ctx, pub, []sqlite.PublicationFile{{Path: "extra.mkv", Bytes: 10}}); err != nil {
		t.Fatal(err)
	}
	file := sqlite.PublicationFile{PublicationID: pub, Path: "extra.mkv", Outcome: "unresolved", Reason: "not selected for import; place or discard it in Resolve"}
	if err = e.db.SetPublicationFileOutcome(ctx, file); err != nil {
		t.Fatal(err)
	}
	var page apigen.ImportActivityPage
	e.get(t, "/api/v1/activity?view=attention").expect(t, 200).into(t, &page)
	if page.Total != 1 {
		t.Fatalf("partial approval hidden: %+v", page)
	}
	file.Reason = "kept by an operator in Resolve"
	if err = e.db.SetPublicationFileOutcome(ctx, file); err != nil {
		t.Fatal(err)
	}
	e.get(t, "/api/v1/activity?view=attention").expect(t, 200).into(t, &page)
	if page.Total != 0 {
		t.Fatalf("kept file still requests a decision: %+v", page)
	}
}
