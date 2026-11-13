package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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

// Model a cancelled failed import after cleanup, then independently reintroduce
// each durable custody signal. GET classification must never change audit data.
func TestCancelledImportHistoryPreservesAuditAndUnresolvedCustody(t *testing.T) {
	e := newAPIEnv(t)
	ctx := context.Background()
	item := e.addMovie(t)
	for _, custody := range []string{"resolved", "stage", "prepared", "retained", "authorization", "retained_authorization", "unresolved", "pending_publication", "failed_publication", "reservation", "stale_generation"} {
		t.Run(custody, func(t *testing.T) {
			id, err := e.db.InsertDownload(ctx, sqlite.Download{MediaItemID: item, State: "failed", Protocol: "usenet", ReleaseTitle: "cancel-fixture-" + custody, ImportPath: "/completed/" + custody})
			if err != nil {
				t.Fatal(err)
			}
			exec := func(q string, args ...any) {
				t.Helper()
				if _, err := e.db.W.ExecContext(ctx, q, args...); err != nil {
					t.Fatal(err)
				}
			}
			exec(`UPDATE downloads SET error='original import failed: stage retained',import_path=? WHERE id=?`, "/completed/"+custody, id)
			dl, err := e.db.GetDownload(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			a, err := e.db.EnsureImportAttempt(ctx, id, dl.GenerationKey)
			if err != nil {
				t.Fatal(err)
			}
			if err := e.db.RecordImportStage(ctx, a.ImportAttemptKey, "/source/"+custody, "/target/"+custody, "/stage/"+custody); err != nil {
				t.Fatal(err)
			}
			exec(`UPDATE import_attempt_stages SET state='operator_disposed' WHERE download_id=?`, id)
			exec(`UPDATE import_attempt_controls SET outcome='cancelled',requested_at=1,finalized_at=2 WHERE download_id=?`, id)
			pub, err := e.db.InsertPublication(ctx, sqlite.PublicationWrite{DownloadID: id, Generation: custody, Kind: "completed", Path: "/completed/" + custody, Source: "none", Cohort: "live"})
			if err != nil {
				t.Fatal(err)
			}
			if err := e.db.SeedPublicationFiles(ctx, pub, []sqlite.PublicationFile{{Path: "film.mkv", Bytes: 123}}); err != nil {
				t.Fatal(err)
			}
			if err := e.db.SetPublicationFileOutcome(ctx, sqlite.PublicationFile{PublicationID: pub, Path: "film.mkv", Outcome: "discarded", Reason: "a later import satisfied every want", DiscardReason: "superseded"}); err != nil {
				t.Fatal(err)
			}
			if err := e.db.SetPublicationState(ctx, pub, "removed", ""); err != nil {
				t.Fatal(err)
			}
			switch custody {
			case "stage":
				exec(`UPDATE import_attempt_stages SET state='held' WHERE download_id=?`, id)
			case "prepared", "retained":
				p := sqlite.Placement{ID: "custody-" + custody, DownloadID: id, SourceGeneration: dl.GenerationKey, Source: "/source/" + custody, Target: "/target/" + custody, State: custody}
				raw, err := json.Marshal(p)
				if err != nil {
					t.Fatal(err)
				}
				exec(`INSERT INTO import_placements(id,target,state,data,updated_at) VALUES(?,?,?,?,1)`, p.ID, p.Target, p.State, string(raw))
			case "authorization", "retained_authorization":
				exec(`INSERT INTO import_publication_authorizations(download_id,generation_key,attempt_epoch,publication_id,source,target,payload,authorized_at) VALUES(?,?,0,?,'/source','/target','{}',1)`, id, dl.GenerationKey, custody)
				if custody == "retained_authorization" {
					exec(`UPDATE import_publication_authorizations SET outcome='reconciled',resolved_at=2,resolution='retained_for_review' WHERE download_id=?`, id)
				}
			case "unresolved":
				if err := e.db.SetPublicationFileOutcome(ctx, sqlite.PublicationFile{PublicationID: pub, Path: "film.mkv", Outcome: "unresolved", Reason: "requires review"}); err != nil {
					t.Fatal(err)
				}
			case "pending_publication":
				if err := e.db.SetPublicationState(ctx, pub, "discovered", ""); err != nil {
					t.Fatal(err)
				}
			case "failed_publication":
				if err := e.db.SetPublicationState(ctx, pub, "import_failed", "unresolved placement"); err != nil {
					t.Fatal(err)
				}
			case "reservation":
				exec(`UPDATE downloads SET submission_phase='uncertain' WHERE id=?`, id)
			case "stale_generation":
				exec(`UPDATE downloads SET generation_key='new-generation' WHERE id=?`, id)
			}
			// Snapshot every persisted table touched by the fixture before GETs.
			snapshot := func() string {
				t.Helper()
				var out strings.Builder
				for _, table := range []string{"downloads", "publications", "publication_files", "import_attempt_controls", "import_attempt_stages", "import_publication_authorizations", "import_placements"} {
					rows, err := e.db.R.QueryContext(ctx, "SELECT * FROM "+table+" ORDER BY rowid")
					if err != nil {
						t.Fatal(err)
					}
					cols, err := rows.Columns()
					if err != nil {
						t.Fatal(err)
					}
					for rows.Next() {
						values := make([]any, len(cols))
						dest := make([]any, len(cols))
						for i := range values {
							dest[i] = &values[i]
						}
						if err := rows.Scan(dest...); err != nil {
							t.Fatal(err)
						}
						fmt.Fprintf(&out, "%s:%#v\n", table, values)
					}
					if err := rows.Err(); err != nil {
						t.Fatal(err)
					}
					rows.Close()
				}
				return out.String()
			}
			before := snapshot()
			want := "attention"
			if custody == "resolved" {
				want = "history"
			}
			// Literal title suffix avoids overlapping names in the shared fixture.
			for _, base := range []string{"/api/v1/imports?filter=", "/api/v1/activity?scope=imports&view="} {
				var page apigen.ImportActivityPage
				e.get(t, base+want+"&q=cancel-fixture-"+custody).expect(t, 200).into(t, &page)
				found := false
				for _, row := range page.Items {
					if row.Download.Id == id {
						found = true
						if row.Category != apigen.ImportActivityItemCategory(want) || row.Download.State != "failed" || row.Download.Error == nil || *row.Download.Error != "original import failed: stage retained" {
							t.Fatalf("audit/classification changed: %+v", row)
						}
					}
				}
				if !found {
					t.Fatalf("%s: missing %d from %s", base, id, want)
				}
				if custody == "resolved" {
					if page.Counts["attention"] != 0 || page.Counts["imported"] != 0 || page.Counts["all"] != 1 {
						t.Fatalf("untruthful counts: %+v", page.Counts)
					}
					e.get(t, base+"attention&q=cancel-fixture-"+custody).expect(t, 200).into(t, &page)
					if page.Total != 0 {
						t.Fatalf("stale attention: %+v", page)
					}
				}
			}
			if after := snapshot(); after != before {
				t.Fatal("GET classification rewrote persisted audit/custody")
			}
		})
	}
}
