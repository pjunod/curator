package sqlite

import (
	"database/sql"
	"fmt"
	"testing"
)

// A failed custody/provenance read must never become evidence that no files
// remain. Exercise query, iteration and SQL-value conversion failures against
// populated real SQLite ledgers, including a failure after a partial result.
func TestImportCustodyReadFailuresNeverBecomeEmptySuccess(t *testing.T) {
	db, receipt, _ := atomicDeliveryFixture(t)
	ctx := t.Context()
	dl, err := db.GetDownload(ctx, receipt.DownloadID)
	if err != nil {
		t.Fatal(err)
	}
	a, err := db.EnsureImportAttempt(ctx, dl.ID, dl.GenerationKey)
	if err != nil {
		t.Fatal(err)
	}
	p := controlPlacement(dl, a, "read-committed", "/source/video", "/library/video")
	if err = db.SealImportAttempt(ctx, a.ImportAttemptKey, []ImportAttemptFile{{Source: p.Source, Kind: "media"}, {Source: "/source/caption", Kind: "sidecar"}}, false); err != nil {
		t.Fatal(err)
	}
	if err = db.PreparePlacement(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err = db.AuthorizeImportPlacement(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, err = db.CommitPlacement(ctx, p); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"held-one", "held-two"} {
		held := controlPlacement(dl, a, id, "/source/"+id, "/library/"+id)
		if err = db.PreparePlacement(ctx, held); err != nil {
			t.Fatal(err)
		}
		if err = db.RecordImportStage(ctx, a.ImportAttemptKey, held.Source, held.Target, "/library/."+id); err != nil {
			t.Fatal(err)
		}
	}
	pub := ImportPublication{ImportAttemptKey: a.ImportAttemptKey, ReceiptID: receipt.ID, ID: "read-sidecar", Source: "/source/caption", Target: "/library/caption", Payload: `{ "stage": "/library/.caption" }`}
	if err = db.AuthorizeImportSidecar(ctx, pub); err != nil {
		t.Fatal(err)
	}
	if _, err = db.RequestImportCancellation(ctx, a.ImportAttemptKey); err != nil {
		t.Fatal(err)
	}
	if err = recordDeliveryFileImport(ctx, db.W, DeliveryFileImport{ReceiptID: receipt.ID, Path: "movie.mkv", LibraryPath: p.Target}); err != nil {
		t.Fatal(err)
	}
	if _, err = db.BeginDeliveryAttempt(ctx, receipt.ID, "import"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.W.ExecContext(ctx, `UPDATE runner_delivery_receipts SET ack_state='accepted',directory_cleanup_state='pending' WHERE id=?`, receipt.ID); err != nil {
		t.Fatal(err)
	}
	calls := map[string]func() (int, error){
		"control":            func() (int, error) { _, e := db.GetImportAttempt(ctx, dl.ID, dl.GenerationKey); return 1, e },
		"pending":            func() (int, error) { v, e := db.PendingImportCancellations(ctx); return len(v), e },
		"files":              func() (int, error) { v, e := db.ImportAttemptFiles(ctx, a.ImportAttemptKey); return len(v), e },
		"publications":       func() (int, error) { v, e := db.ImportPublications(ctx, dl.ID, dl.GenerationKey); return len(v), e },
		"stages":             func() (int, error) { v, e := db.ImportStageCustody(ctx, dl.ID, dl.GenerationKey); return len(v), e },
		"source-custody":     func() (int, error) { v, e := db.ImportSourceCustodyPaths(ctx); return len(v), e },
		"sidecar-references": func() (int, error) { v, e := db.unresolvedSidecarReferences(ctx); return len(v), e },
		"unresolved":         func() (int, error) { v, e := db.UnresolvedPlacementReferences(ctx); return len(v), e },
		"recovery-batch":     func() (int, error) { v, e := db.PendingPlacements(ctx); return len(v), e },
		"committed-receipt": func() (int, error) {
			v, e := db.CommittedImportPlacements(ctx, dl.ID, dl.GenerationKey, p.Source)
			return len(v), e
		},
		"placement":         func() (int, error) { _, e := db.GetPlacement(ctx, p.ID); return 1, e },
		"delivery-imports":  func() (int, error) { v, e := db.DeliveryFileImports(ctx, receipt.ID); return len(v), e },
		"delivery-outcomes": func() (int, error) { v, e := db.DeliveryFileOutcomes(ctx, receipt.ID); return len(v), e },
		"delivery-receipt":  func() (int, error) { _, e := db.DeliveryReceiptByID(ctx, receipt.ID); return 1, e },
	}
	calls["delivery-page"] = func() (int, error) { v, _, e := db.DeliveryReceiptPage(ctx, 0, 100); return len(v), e }
	calls["delivery-retry-details"] = func() (int, error) { _, e := db.DeliveryRetryDetails(ctx, receipt.ID); return 1, e }
	calls["delivery-events"] = func() (int, error) { v, e := db.DeliveryAttemptEvents(ctx, receipt.ID); return len(v), e }
	calls["delivery-file-proof"] = func() (int, error) { _, e := db.DeliveryFileOutcomeByPath(ctx, receipt.ID, "movie.mkv"); return 1, e }

	before := importDurableSnapshot(t, db)
	for name, call := range calls {
		for _, malformed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/malformed=%t", name, malformed), func(t *testing.T) {
				completed := false
				for point := 1; point <= 32; point++ {
					fault := &importFaultDriver{at: point, malformed: malformed}
					driverName := fmt.Sprintf("custody-read-fault-%d", importFaultDriverID.Add(1))
					sql.Register(driverName, fault)
					reader, e := sql.Open(driverName, "file:"+db.Path+"?_pragma=query_only(1)&_pragma=busy_timeout(5000)")
					if e != nil {
						t.Fatal(e)
					}
					original := db.R
					t.Cleanup(func() { db.R = original; _ = reader.Close() })
					db.R = reader
					count, e := call()
					db.R = original
					if closeErr := reader.Close(); closeErr != nil {
						t.Fatal(closeErr)
					}
					if !fault.fired {
						if e != nil || count == 0 {
							t.Fatalf("unfaulted populated read returned %d, %v", count, e)
						}
						completed = true
						break
					}
					if e == nil {
						t.Fatalf("read failure %d returned success with %d rows", point, count)
					}
				}
				if !completed {
					t.Fatal("read never completed without injection")
				}
				if after := importDurableSnapshot(t, db); after != before {
					t.Fatal("custody read changed durable state")
				}
			})
		}
	}
}

// Syntactically valid JSON with incompatible field types must not silently
// erase the receipt/custody inventory during restart or deletion review.
func TestImportRecoveryRejectsIncompatibleStoredReceipts(t *testing.T) {
	for _, column := range []string{"files_json", "file_outcomes_json", "lifecycle_timing_json", "runner_stages_json", "source_copies_json"} {
		t.Run(column, func(t *testing.T) {
			db, r, _ := atomicDeliveryFixture(t)
			if _, err := db.W.ExecContext(t.Context(), `UPDATE runner_delivery_receipts SET `+column+`='true',ack_state='accepted' WHERE id=?`, r.ID); err != nil {
				t.Fatal(err)
			}
			before := importDurableSnapshot(t, db)
			if _, err := db.DeliveryReceiptByID(t.Context(), r.ID); err == nil {
				t.Fatal("incompatible receipt decoded as usable")
			}
			if _, _, err := db.DeliveryReceiptPage(t.Context(), 0, 100); err == nil {
				t.Fatal("incompatible receipt disappeared from recovery page")
			}
			if after := importDurableSnapshot(t, db); after != before {
				t.Fatal("failed decode mutated custody")
			}
		})
	}
	db, dl, a := importControlFixture(t)
	p := controlPlacement(dl, a, "incompatible-placement", "/source/video", "/library/video")
	if err := db.PreparePlacement(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(t.Context(), `UPDATE import_placements SET data=json_set(data,'$.size','not-an-integer') WHERE id=?`, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetPlacement(t.Context(), p.ID); err == nil {
		t.Fatal("incompatible placement decoded")
	}
	if _, err := db.UnresolvedPlacementReferences(t.Context()); err == nil {
		t.Fatal("incompatible placement disappeared from custody")
	}
	if _, err := db.W.ExecContext(t.Context(), `UPDATE import_placements SET state='committed' WHERE id=?`, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CommittedImportPlacements(t.Context(), dl.ID, dl.GenerationKey, p.Source); err == nil {
		t.Fatal("incompatible receipt became reuse evidence")
	}
}
