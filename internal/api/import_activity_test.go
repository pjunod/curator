package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	apigen "github.com/pjunod/monarr/internal/api/gen"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

func TestImportActivityCoversRetainedHistoryAndExcludesFetchFailures(t *testing.T) {
	e := newAPIEnv(t)
	ctx := context.Background()
	item := e.addMovie(t)
	insert := func(state, path, protocol string) int64 {
		t.Helper()
		id, err := e.db.InsertDownload(ctx, sqlite.Download{MediaItemID: item, State: state, ImportPath: path, Protocol: protocol, ReleaseTitle: "Import page fixture"})
		if err != nil {
			t.Fatal(err)
		}
		row, err := e.db.GetDownload(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		row.ImportPath = path
		if err := e.db.UpdateDownloadHandoff(ctx, row); err != nil {
			t.Fatal(err)
		}
		return id
	}
	insert("failed", "", "usenet") // A failed fetch is not an import failure.
	insert("downloading", "", "usenet")
	queued := insert("downloaded", "/source/queued", "usenet")
	failed := insert("failed", "/source/failed", "usenet")
	stale := insert("importing", "/source/interrupted", "manual")
	finished := insert("imported", "/source/finished", "manual")
	if _, err := e.db.W.ExecContext(ctx, "UPDATE downloads SET activity_dismissed=1,updated_at=? WHERE id=?", time.Now().Add(-time.Hour).UnixMilli(), finished); err != nil {
		t.Fatal(err)
	}
	var page apigen.ImportActivityPage
	e.get(t, "/api/v1/imports?limit=2").expect(t, http.StatusOK).into(t, &page)
	if page.Total != 4 || len(page.Items) != 2 || !page.HasMore || page.Counts["attention"] != 2 || page.Counts["queued"] != 1 || page.Counts["imported"] != 1 {
		t.Fatalf("page=%+v", page)
	}
	if page.Items[0].Download.Id != queued || page.Items[0].Category != "queued" {
		t.Fatalf("expected queued before history: %+v", page.Items)
	}
	e.get(t, "/api/v1/imports?filter=imported").expect(t, 200).into(t, &page)
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].Download.Id != finished || page.HasMore {
		t.Fatalf("dismissed retained import missing: %+v", page)
	}
	e.get(t, "/api/v1/imports?filter=attention&limit=1&offset=1").expect(t, 200).into(t, &page)
	if page.Total != 2 || len(page.Items) != 1 || page.HasMore {
		t.Fatalf("attention pagination: %+v", page)
	}
	if id := page.Items[0].Download.Id; id != failed && id != stale {
		t.Fatal(id)
	}
	e.get(t, "/api/v1/imports?q=not-a-matching-title").expect(t, 200).into(t, &page)
	if page.Total != 0 || len(page.Items) != 0 || page.Counts["all"] != 0 {
		t.Fatalf("search=%+v", page)
	}
	for _, q := range []string{"filter=bogus", "limit=0", "limit=201", "offset=-1"} {
		e.get(t, "/api/v1/imports?"+q).expect(t, 400)
	}
}

func TestImportActivitySearchesLibraryTitleAndPublicationOutcomes(t *testing.T) {
	e := newAPIEnv(t)
	ctx := context.Background()
	item := e.addMovie(t)
	id, err := e.db.InsertDownload(ctx, sqlite.Download{MediaItemID: item, State: "imported", Protocol: "usenet", ReleaseTitle: "unrecognizable.opaque.release"})
	if err != nil {
		t.Fatal(err)
	}
	pub, err := e.db.InsertPublication(ctx, sqlite.PublicationWrite{DownloadID: id, Generation: "activity", Kind: "completed", Path: "/completed/page", Source: "v1_record", Cohort: "live"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.db.SeedPublicationFiles(ctx, pub, []sqlite.PublicationFile{{Path: "main.mkv", Bytes: 123}, {Path: "orphan.nfo", Bytes: 2}}); err != nil {
		t.Fatal(err)
	}
	if err := e.db.SetPublicationFileOutcome(ctx, sqlite.PublicationFile{PublicationID: pub, Path: "orphan.nfo", Outcome: "unresolved", Reason: "No parent file"}); err != nil {
		t.Fatal(err)
	}
	var page apigen.ImportActivityPage
	e.get(t, "/api/v1/imports?q=Fight").expect(t, 200).into(t, &page)
	if page.Total != 1 || page.Items[0].Category != "attention" || page.Items[0].Title == "unrecognizable.opaque.release" {
		t.Fatalf("library search/unresolved classification: %+v", page)
	}
	if p := page.Items[0].Download.Publications; p == nil || len(*p) != 1 || len((*p)[0].Files) != 2 {
		t.Fatalf("missing manifest: %+v", p)
	}
	// Search is a literal substring, so SQL wildcard characters are not patterns.
	e.get(t, "/api/v1/imports?q=%25").expect(t, 200).into(t, &page)
	if page.Total != 0 {
		t.Fatalf("wildcard search matched: %+v", page)
	}
	e.get(t, fmt.Sprintf("/api/v1/imports?offset=%d", 10000)).expect(t, 200).into(t, &page)
	if len(page.Items) != 0 || page.HasMore {
		t.Fatalf("past final page: %+v", page)
	}
}

func TestImportActivityIncludesSalvageAndUnattributedImportedEvidence(t *testing.T) {
	e := newAPIEnv(t)
	ctx := context.Background()
	item := e.addMovie(t)
	for _, state := range []string{"downloading", "failed"} {
		id, err := e.db.InsertDownload(ctx, sqlite.Download{MediaItemID: item, State: state, Protocol: "usenet", ReleaseTitle: "Salvage " + state})
		if err != nil {
			t.Fatal(err)
		}
		pub, err := e.db.InsertPublication(ctx, sqlite.PublicationWrite{DownloadID: id, Generation: "salvage", Kind: "salvage", Path: "/completed/salvage-" + state, Source: "v1_record", Cohort: "live"})
		if err != nil {
			t.Fatal(err)
		}
		if err := e.db.SetPublicationState(ctx, pub, "imported", ""); err != nil {
			t.Fatal(err)
		}
	}
	detached, err := e.db.InsertPublication(ctx, sqlite.PublicationWrite{Kind: "completed", Path: "/old/actual-import", Source: "v1_record", Cohort: "historical"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.db.SeedPublicationFiles(ctx, detached, []sqlite.PublicationFile{{Path: "Film.mkv", Bytes: 100}}); err != nil {
		t.Fatal(err)
	}
	if err := e.db.SetPublicationFileOutcome(ctx, sqlite.PublicationFile{PublicationID: detached, Path: "Film.mkv", Outcome: "placed", LibraryPath: "/library/Film.mkv"}); err != nil {
		t.Fatal(err)
	}
	if err := e.db.SetPublicationState(ctx, detached, "imported", ""); err != nil {
		t.Fatal(err)
	}
	// Mere Storage inventory with no import evidence is not an imported file.
	if _, err := e.db.InsertPublication(ctx, sqlite.PublicationWrite{Kind: "completed", Path: "/old/unimported-inventory", Source: "none", Cohort: "historical"}); err != nil {
		t.Fatal(err)
	}
	partial, err := e.db.InsertPublication(ctx, sqlite.PublicationWrite{Kind: "completed", Path: "/old/interrupted-partial", Source: "v1_record", Cohort: "historical"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.db.SeedPublicationFiles(ctx, partial, []sqlite.PublicationFile{{Path: "One.mkv", Bytes: 100}, {Path: "Two.mkv", Bytes: 100}}); err != nil {
		t.Fatal(err)
	}
	if err := e.db.SetPublicationFileOutcome(ctx, sqlite.PublicationFile{PublicationID: partial, Path: "One.mkv", Outcome: "placed", LibraryPath: "/library/One.mkv"}); err != nil {
		t.Fatal(err)
	}

	var page apigen.ImportActivityPage
	e.get(t, "/api/v1/imports?filter=imported").expect(t, 200).into(t, &page)
	if page.Total != 3 || page.Counts["attention"] != 1 {
		t.Fatalf("salvage/detached=%+v", page)
	}
	found := false
	for _, i := range page.Items {
		if i.Download.Id < 0 {
			found = true
			if i.Download.Id != -detached || i.Download.MediaItemId != 0 || i.Destination != "" || i.Download.Publications == nil {
				t.Fatalf("unattributed=%+v", i)
			}
		}
	}
	if !found {
		t.Fatal("missing real historical import")
	}
}
