package sqlite

import (
	"context"
	"errors"
	"testing"
)

// A caller whose admission context has already ended must never mutate durable
// control or custody. This covers the public storage boundary used at shutdown.
func TestImportStorageCancelledCallsPreserveAdmission(t *testing.T) {
	db, dl, a := importControlFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := controlPlacement(dl, a, "cancelled-call", "/source/video", "/library/video")
	calls := map[string]func() error{
		"ensure":   func() error { _, e := db.EnsureImportAttempt(ctx, dl.ID, dl.GenerationKey); return e },
		"retry":    func() error { _, e := db.BeginImportRetry(ctx, dl.ID, dl.GenerationKey); return e },
		"seal":     func() error { return db.SealImportAttempt(ctx, a.ImportAttemptKey, nil, false) },
		"resolve":  func() error { return db.ResolveImportFile(ctx, a.ImportAttemptKey, p.Source, "skipped", "") },
		"cancel":   func() error { _, e := db.RequestImportCancellation(ctx, a.ImportAttemptKey); return e },
		"finalize": func() error { _, e := db.FinalizeImportCancellation(ctx, a.ImportAttemptKey); return e },
		"complete": func() error { return db.CompleteImportAttempt(ctx, a.ImportAttemptKey) },
		"grant":    func() error { return db.AuthorizeImportPlacement(ctx, p) },
		"retain":   func() error { _, e := db.RetainUncertainImport(ctx, a.ImportAttemptKey, nil); return e },
		"dispose": func() error {
			return db.ResolveRetainedImportTargets(ctx, a.ImportAttemptKey, []string{"stage:cancelled-call"}, "operator_disposed")
		},
		"sidecar-grant":  func() error { return db.AuthorizeImportSidecar(ctx, ImportPublication{}) },
		"sidecar-result": func() error { return db.CommitImportSidecar(ctx, ImportPublication{}, DeliveryFileImport{}) },
		"stage":          func() error { return db.RecordImportStage(ctx, a.ImportAttemptKey, p.Source, p.Target, p.Temporary) },
		"reuse":          func() error { return db.SatisfyImportFile(ctx, a.ImportAttemptKey, p) },
		"pending":        func() error { _, e := db.PendingImportCancellations(ctx); return e },
		"files":          func() error { _, e := db.ImportAttemptFiles(ctx, a.ImportAttemptKey); return e },
		"publications":   func() error { _, e := db.ImportPublications(ctx, dl.ID, dl.GenerationKey); return e },
		"stages":         func() error { _, e := db.ImportStageCustody(ctx, dl.ID, dl.GenerationKey); return e },
		"custody":        func() error { _, e := db.ImportSourceCustodyPaths(ctx); return e },
		"references":     func() error { _, e := db.unresolvedSidecarReferences(ctx); return e },
		"candidates":     func() error { _, e := db.CommittedImportPlacements(ctx, dl.ID, dl.GenerationKey, p.Source); return e },
		"available":      func() error { return db.AssertPublicationTargetAvailable(ctx, p.Target) },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if e := call(); !errors.Is(e, context.Canceled) {
				t.Fatalf("cancelled caller returned %v", e)
			}
		})
	}
	current, e := db.GetImportAttempt(context.Background(), dl.ID, dl.GenerationKey)
	if e != nil || current != a {
		t.Fatalf("cancelled calls changed durable admission: %+v %v", current, e)
	}
}

func TestImportAuthorityRejectsChangedPreparedSnapshot(t *testing.T) {
	for _, change := range []string{"digest", "epoch", "missing", "retained", "unsealed", "unselected"} {
		t.Run(change, func(t *testing.T) {
			db, dl, a := importControlFixture(t)
			ctx := context.Background()
			p := controlPlacement(dl, a, "snapshot", "/source/video", "/library/video")
			if change != "unsealed" {
				mustSeal(t, db, a, p.Source)
			}
			if change != "missing" {
				if e := db.PreparePlacement(ctx, p); e != nil {
					t.Fatal(e)
				}
			}
			switch change {
			case "digest":
				p.SHA256 = "other"
			case "epoch":
				p.ImportEpoch = nil
			case "retained":
				if _, e := db.W.ExecContext(ctx, `UPDATE import_placements SET state='retained' WHERE id=?`, p.ID); e != nil {
					t.Fatal(e)
				}
			case "unselected":
				if _, e := db.W.ExecContext(ctx, `DELETE FROM import_attempt_files WHERE download_id=?`, dl.ID); e != nil {
					t.Fatal(e)
				}
			}
			if e := db.AuthorizeImportPlacement(ctx, p); e == nil {
				t.Fatal("changed snapshot gained publication authority")
			}
			current, e := db.GetImportAttempt(ctx, dl.ID, dl.GenerationKey)
			if e != nil || current.PendingPublications != 0 {
				t.Fatalf("failed grant leaked authority: %+v %v", current, e)
			}
		})
	}
}

func TestImportTerminalOutcomesRemainIdempotentAndFenceRetryOverflow(t *testing.T) {
	db, dl, a := importControlFixture(t)
	ctx := context.Background()
	initial, e := db.FinalizeImportCancellation(ctx, a.ImportAttemptKey)
	if e != nil || initial.Outcome != "" {
		t.Fatalf("unrequested cancellation finalized: %+v %v", initial, e)
	}
	mustSeal(t, db, a, "/source/skipped")
	if e = db.ResolveImportFile(ctx, a.ImportAttemptKey, "/source/skipped", "committed", "test-receipt"); e != nil {
		t.Fatal(e)
	}
	done, e := db.RequestImportCancellation(ctx, a.ImportAttemptKey)
	if e != nil || done.Outcome != "completed" {
		t.Fatalf("completed plan lost: %+v %v", done, e)
	}
	done, e = db.RequestImportCancellation(ctx, a.ImportAttemptKey)
	if e != nil || done.Outcome != "completed" {
		t.Fatalf("completed plan recancelled: %+v %v", done, e)
	}
	if _, e = db.RetainUncertainImport(ctx, a.ImportAttemptKey, nil); !errors.Is(e, ErrImportPublicationPending) {
		t.Fatal(e)
	}
	if e = db.ResolveRetainedImportTargets(ctx, a.ImportAttemptKey, nil, "invalid"); e == nil {
		t.Fatal("invalid disposition accepted")
	}
	if e = db.ResolveRetainedImportTargets(ctx, a.ImportAttemptKey, nil, "operator_disposed"); !errors.Is(e, ErrImportReviewRequired) {
		t.Fatal(e)
	}
	if _, e = db.W.ExecContext(ctx, `UPDATE import_attempt_controls SET attempt_epoch=9223372036854775807 WHERE download_id=?`, dl.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = db.BeginImportRetry(ctx, dl.ID, dl.GenerationKey); e == nil {
		t.Fatal("epoch wrapped")
	}
}

// Storage schema loss must fail closed at each admission boundary, including
// reads used to authorize deletion. A missing table is never an empty ledger.
func TestImportStorageUnavailableNeverMeansNoCustody(t *testing.T) {
	for _, phase := range []string{"control", "plan", "placement", "publications", "stages", "handoff"} {
		t.Run(phase, func(t *testing.T) {
			db, dl, a := importControlFixture(t)
			ctx := context.Background()
			mustSeal(t, db, a, "/source/video")
			p := controlPlacement(dl, a, "storage-fault", "/source/video", "/library/video")
			if e := db.PreparePlacement(ctx, p); e != nil {
				t.Fatal(e)
			}
			if e := db.AuthorizeImportPlacement(ctx, p); e != nil {
				t.Fatal(e)
			}
			// Renames preserve all data for inspection but make the required contract
			// unavailable, modelling an interrupted/incompatible storage transition.
			table := map[string]string{"control": "import_attempt_controls", "plan": "import_attempt_files", "placement": "import_placements", "publications": "import_publication_authorizations", "stages": "import_attempt_stages"}[phase]
			if phase == "handoff" {
				if _, e := db.W.ExecContext(ctx, `UPDATE downloads SET handoff_log='not-json' WHERE id=?`, dl.ID); e != nil {
					t.Fatal(e)
				}
				if _, e := db.W.ExecContext(ctx, `UPDATE import_publication_authorizations SET outcome='reconciled',resolved_at=0,resolution='operator_disposed'`); e != nil {
					t.Fatal(e)
				}
				if _, e := db.RequestImportCancellation(ctx, a.ImportAttemptKey); e != nil {
					t.Fatal(e)
				}
				if _, e := db.FinalizeImportCancellation(ctx, a.ImportAttemptKey); e == nil {
					t.Fatal("malformed handoff lost intent")
				}
				current, e := db.GetImportAttempt(ctx, dl.ID, dl.GenerationKey)
				if e != nil || current.Outcome != "pending" {
					t.Fatalf("failed finalization erased intent: %+v %v", current, e)
				}
				return
			}
			if _, e := db.W.ExecContext(ctx, `ALTER TABLE `+table+` RENAME TO unavailable_`+table); e != nil {
				t.Fatal(e)
			}
			calls := map[string]func() error{}
			switch phase {
			case "control", "publications", "stages", "placement":
				calls["ensure"] = func() error { _, e := db.EnsureImportAttempt(ctx, dl.ID, dl.GenerationKey); return e }
				calls["cancel"] = func() error { _, e := db.RequestImportCancellation(ctx, a.ImportAttemptKey); return e }
				calls["retry"] = func() error { _, e := db.BeginImportRetry(ctx, dl.ID, dl.GenerationKey); return e }
				calls["stage"] = func() error { return db.RecordImportStage(ctx, a.ImportAttemptKey, p.Source, p.Target, p.Temporary) }
			}
			switch phase {
			case "control", "publications", "stages", "placement", "plan":
				calls["seal"] = func() error {
					return db.SealImportAttempt(ctx, a.ImportAttemptKey, []ImportAttemptFile{{Source: p.Source, Kind: "media"}}, false)
				}
				calls["complete"] = func() error { return db.CompleteImportAttempt(ctx, a.ImportAttemptKey) }
				calls["resolve"] = func() error { return db.ResolveImportFile(ctx, a.ImportAttemptKey, p.Source, "skipped", "") }
			}
			if phase == "publications" || phase == "stages" || phase == "placement" {
				calls["custody"] = func() error { _, e := db.ImportSourceCustodyPaths(ctx); return e }
				calls["stage-inventory"] = func() error { _, e := db.ImportStageCustody(ctx, dl.ID, dl.GenerationKey); return e }
			}
			if phase == "publications" || phase == "stages" {
				calls["sidecar-references"] = func() error { _, e := db.unresolvedSidecarReferences(ctx); return e }
			}
			if phase == "publications" {
				calls["target"] = func() error { return db.AssertPublicationTargetAvailable(ctx, p.Target) }
				calls["inventory"] = func() error { _, e := db.ImportPublications(ctx, dl.ID, dl.GenerationKey); return e }
			}
			if phase == "plan" {
				calls["files"] = func() error { _, e := db.ImportAttemptFiles(ctx, a.ImportAttemptKey); return e }
			}
			if phase == "placement" {
				calls["grant"] = func() error { return db.AuthorizeImportPlacement(ctx, p) }
				calls["reuse-candidates"] = func() error { _, e := db.CommittedImportPlacements(ctx, dl.ID, dl.GenerationKey, p.Source); return e }
			}
			for name, call := range calls {
				t.Run(name, func(t *testing.T) {
					if e := call(); e == nil {
						t.Fatal("unavailable storage reported safe success")
					}
				})
			}
		})
	}
}

func TestImportReviewRollbackDoesNotPartiallyReleaseCustody(t *testing.T) {
	for _, failure := range []string{"publication", "control", "handoff", "placement", "stage"} {
		t.Run(failure, func(t *testing.T) {
			db, dl, a := importControlFixture(t)
			ctx := context.Background()
			p := controlPlacement(dl, a, "review-fault", "/source/video", "/library/video")
			mustSeal(t, db, a, p.Source)
			if e := db.PreparePlacement(ctx, p); e != nil {
				t.Fatal(e)
			}
			if e := db.AuthorizeImportPlacement(ctx, p); e != nil {
				t.Fatal(e)
			}
			if failure == "stage" {
				if e := db.RecordImportStage(ctx, a.ImportAttemptKey, p.Source, p.Target, p.Temporary); e != nil {
					t.Fatal(e)
				}
				if _, e := db.W.ExecContext(ctx, `UPDATE import_publication_authorizations SET payload=json_set(payload,'$.stage',?)`, p.Temporary); e != nil {
					t.Fatal(e)
				}
			}
			if _, e := db.RequestImportCancellation(ctx, a.ImportAttemptKey); e != nil {
				t.Fatal(e)
			}
			if failure == "placement" || failure == "stage" {
				if _, e := db.RetainUncertainImport(ctx, a.ImportAttemptKey, []string{"placement:" + p.ID}); e != nil {
					t.Fatal(e)
				}
			}
			table := map[string]string{"publication": "import_publication_authorizations", "control": "import_attempt_controls", "handoff": "downloads", "placement": "import_placements", "stage": "import_attempt_stages"}[failure]
			if _, e := db.W.ExecContext(ctx, `CREATE TRIGGER fail_review BEFORE UPDATE ON `+table+` BEGIN SELECT RAISE(ABORT,'review storage unavailable'); END`); e != nil {
				t.Fatal(e)
			}
			var e error
			if failure == "placement" || failure == "stage" {
				e = db.ResolveRetainedImportTargets(ctx, a.ImportAttemptKey, []string{"placement:" + p.ID}, "operator_disposed")
			} else {
				_, e = db.RetainUncertainImport(ctx, a.ImportAttemptKey, []string{"placement:" + p.ID})
			}
			if e == nil {
				t.Fatal("failed review reported success")
			}
			pubs, e := db.ImportPublications(ctx, dl.ID, dl.GenerationKey)
			if e != nil || len(pubs) != 1 {
				t.Fatalf("evidence disappeared: %+v %v", pubs, e)
			}
			expected := "pending"
			if failure == "placement" || failure == "stage" {
				expected = "reconciled"
				if pubs[0].Resolution != "retained_for_review" {
					t.Fatal("partial release", pubs[0])
				}
			}
			if pubs[0].Outcome != expected {
				t.Fatalf("partial revocation: %+v", pubs[0])
			}
			current, e := db.GetPlacement(ctx, p.ID)
			if e != nil || current.State != "prepared" {
				t.Fatalf("partial stage release: %+v %v", current, e)
			}
		})
	}
}

func TestImportCancellationNeverReopensUnresolvedPublication(t *testing.T) {
	db, dl, a := importControlFixture(t)
	ctx := context.Background()
	p := controlPlacement(dl, a, "immutable", "/source/video", "/library/video")
	mustSeal(t, db, a, p.Source)
	if e := db.PreparePlacement(ctx, p); e != nil {
		t.Fatal(e)
	}
	if e := db.AuthorizeImportPlacement(ctx, p); e != nil {
		t.Fatal(e)
	}
	if _, e := db.BeginImportRetry(ctx, dl.ID, dl.GenerationKey); !errors.Is(e, ErrImportPublicationPending) {
		t.Fatal("pending publication retried", e)
	}
	// A grant is immutable even when another worker reuses its publication ID.
	pub, e := placementPublication(p)
	if e != nil {
		t.Fatal(e)
	}
	tx, e := db.W.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	changed := pub
	changed.Payload += " "
	if e = authorizeImportPublication(ctx, tx, changed); !errors.Is(e, ErrImportAttemptChanged) {
		_ = tx.Rollback()
		t.Fatal("grant rebound", e)
	}
	if e = commitImportPublication(ctx, tx, changed); !errors.Is(e, ErrImportAttemptChanged) {
		_ = tx.Rollback()
		t.Fatal("result rebound", e)
	}
	missing := pub
	missing.ID = "missing"
	if e = commitImportPublication(ctx, tx, missing); !errors.Is(e, ErrImportAttemptChanged) {
		_ = tx.Rollback()
		t.Fatal("missing grant committed", e)
	}
	if e = tx.Rollback(); e != nil {
		t.Fatal(e)
	}
	if _, e = db.RequestImportCancellation(ctx, a.ImportAttemptKey); e != nil {
		t.Fatal(e)
	}
	if _, e = db.RetainUncertainImport(ctx, a.ImportAttemptKey, []string{pub.ID, pub.ID}); !errors.Is(e, ErrImportAttemptChanged) {
		t.Fatal("duplicate review ids admitted", e)
	}
	if _, e = db.RetainUncertainImport(ctx, a.ImportAttemptKey, []string{pub.ID, "extra"}); !errors.Is(e, ErrImportAttemptChanged) {
		t.Fatal("extra review id admitted", e)
	}
	if e = db.ResolveRetainedImportTargets(ctx, a.ImportAttemptKey, []string{pub.ID}, "operator_disposed"); !errors.Is(e, ErrImportPublicationPending) {
		t.Fatal("pending grant released", e)
	}
	if _, e = db.RetainUncertainImport(ctx, a.ImportAttemptKey, []string{pub.ID}); e != nil {
		t.Fatal(e)
	}
	if e = db.RecordImportStage(ctx, a.ImportAttemptKey, p.Source, p.Target, p.Temporary); !errors.Is(e, ErrImportCancelled) {
		t.Fatal("cancelled attempt created stage", e)
	}
	if e = db.ResolveImportFile(ctx, a.ImportAttemptKey, p.Source, "committed", p.ID); !errors.Is(e, ErrImportCancelled) {
		t.Fatal("cancelled attempt resolved file", e)
	}
	if e = db.SealImportAttempt(ctx, a.ImportAttemptKey, nil, false); !errors.Is(e, ErrImportCancelled) {
		t.Fatal("cancelled plan resealed", e)
	}
	if e = db.ResolveRetainedImportTargets(ctx, a.ImportAttemptKey, []string{"stage:" + p.ID}, "operator_adopted"); !errors.Is(e, ErrImportReviewRequired) {
		t.Fatal("stage adopted as library", e)
	}
	if e = db.ResolveRetainedImportTargets(ctx, a.ImportAttemptKey, []string{"stage:missing"}, "operator_disposed"); !errors.Is(e, ErrImportAttemptChanged) {
		t.Fatal("missing stage released", e)
	}
}
