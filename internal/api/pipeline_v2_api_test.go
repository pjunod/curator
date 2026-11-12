package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	apigen "github.com/pjunod/monarr/internal/api/gen"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

// Pipeline contract v2: Activity carries each row's publications and their
// files (read in one batch for the page), and Resolve decides an unresolved
// file (review #12, #13).
func TestQueueListsPublicationsAndResolveDecidesAFile(t *testing.T) {
	e := newAPIEnv(t)
	acqAddClient(t, e, "qbittorrent")
	itemID := e.addMovie(t)
	downloadID := acqGrab(t, e, itemID)
	other := gapGrabTitled(t, e, itemID, "Fight.Club.1999.1080p.WEB-DL.x264-SECOND")
	ctx := context.Background()
	pubID, err := e.db.InsertPublication(ctx, sqlite.PublicationWrite{DownloadID: downloadID, Generation: "g1", Kind: "completed",
		Path: t.TempDir(), Source: "v1_record", Cohort: "live"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.db.SeedPublicationFiles(ctx, pubID, []sqlite.PublicationFile{{Path: "Movie.mkv", Bytes: 10}, {Path: "Movie.nfo", Bytes: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := e.db.SetPublicationFileOutcome(ctx, sqlite.PublicationFile{PublicationID: pubID, Path: "Movie.nfo", Outcome: "unresolved", Reason: "a sidecar with no parent"}); err != nil {
		t.Fatal(err)
	}

	var queue []apigen.QueueItem
	e.get(t, "/api/v1/queue").expect(t, http.StatusOK).into(t, &queue)
	var row *apigen.QueueItem
	for i := range queue {
		if queue[i].Id == downloadID {
			row = &queue[i]
		}
	}
	if row == nil || row.Publications == nil || len(*row.Publications) != 1 {
		t.Fatalf("queue row = %+v, want one publication", row)
	}
	p := (*row.Publications)[0]
	if p.Id != pubID || len(p.Files) != 2 || p.Files[1].Path != "Movie.nfo" || p.Files[1].Outcome != "unresolved" || p.Files[1].Reason == nil {
		t.Fatalf("publication = %+v, want both files with the unresolved sidecar's reason", p)
	}

	endpoint := fmt.Sprintf("/api/v1/queue/%d/publications/%d/files/resolve", downloadID, pubID)
	// The discard is recorded; the bytes leave through the per-file cleanup
	// (M4), which waits while the import still owns a discovered
	// publication, and Resolve says so rather than reading as deleted.
	kept := e.post(t, endpoint, `{"path":"Movie.nfo","action":"discard"}`).expect(t, http.StatusConflict)
	if body := kept.Body.String(); !strings.Contains(body, `"discarded_kept"`) || !strings.Contains(body, "the import owns this publication") {
		t.Fatalf("discard response = %s, want discarded_kept with the reason", body)
	}
	files, _ := e.db.ListPublicationFiles(ctx, pubID)
	if files[1].Outcome != "discarded" || files[1].DiscardReason != "rejected" {
		t.Fatalf("discarded file = %+v", files[1])
	}
	// A decided file is no longer resolvable; an unknown action and a
	// missing path are refused; another download's publication is not found.
	e.post(t, endpoint, `{"path":"Movie.nfo","action":"keep"}`).expect(t, http.StatusBadRequest)
	e.post(t, endpoint, `{"path":"Movie.mkv","action":"shred"}`).expect(t, http.StatusBadRequest)
	e.post(t, endpoint, `{"action":"keep"}`).expect(t, http.StatusBadRequest)
	e.post(t, fmt.Sprintf("/api/v1/queue/%d/publications/%d/files/resolve", other, pubID), `{"path":"Movie.mkv","action":"keep"}`).expect(t, http.StatusNotFound)
	e.post(t, endpoint, `{"path":"Movie.mkv","action":"keep"}`).expect(t, http.StatusNoContent)
	files, _ = e.db.ListPublicationFiles(ctx, pubID)
	if files[0].Outcome != "pending" || files[0].Reason != "kept by an operator in Resolve" {
		t.Fatalf("kept file = %+v", files[0])
	}
}

// Review r3 (5): a publication the environment holds says so on its queue
// row (held, why, since when) and is counted on the System pipeline.
func TestHeldPublicationIsShownAndCounted(t *testing.T) {
	e := newAPIEnv(t)
	acqAddClient(t, e, "qbittorrent")
	itemID := e.addMovie(t)
	downloadID := acqGrab(t, e, itemID)
	ctx := context.Background()
	pubID, err := e.db.InsertPublication(ctx, sqlite.PublicationWrite{DownloadID: downloadID, Generation: "g1", Kind: "completed", Path: t.TempDir(), Cohort: "live"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.HoldPublicationForEnvironment(ctx, pubID, "library full"); err != nil {
		t.Fatal(err)
	}
	var queue []apigen.QueueItem
	e.get(t, "/api/v1/queue").expect(t, http.StatusOK).into(t, &queue)
	var pub *apigen.QueuePublication
	for i := range queue {
		if queue[i].Id == downloadID && queue[i].Publications != nil {
			pub = &(*queue[i].Publications)[0]
		}
	}
	if pub == nil || pub.Held == nil || !*pub.Held || pub.HeldReason == nil || *pub.HeldReason != "library full" || pub.HeldSince == nil {
		t.Fatalf("publication = %+v, want held with its reason and since", pub)
	}
	var sum apigen.PipelineSummary
	e.get(t, "/api/v1/system/pipeline").expect(t, http.StatusOK).into(t, &sum)
	if sum.HeldPublications != 1 {
		t.Fatalf("held publications = %d, want 1", sum.HeldPublications)
	}
}
