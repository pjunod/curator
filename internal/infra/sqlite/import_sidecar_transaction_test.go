package sqlite

import (
	"context"
	"errors"
	"testing"
)

func TestSidecarGrantResultAndCustodyRollbackTogether(t *testing.T) {
	for _, failure := range []string{"provenance", "stage-release", "disposition", "file-result"} {
		t.Run(failure, func(t *testing.T) {
			db, r, _ := atomicDeliveryFixture(t)
			ctx := context.Background()
			a, e := db.EnsureImportAttempt(ctx, r.DownloadID, r.GenerationKey)
			if e != nil {
				t.Fatal(e)
			}
			source := r.MappedPath + "/movie.mkv"
			target := "/library/caption.srt"
			stage := "/library/.caption-stage"
			if e = db.SealImportAttempt(ctx, a.ImportAttemptKey, []ImportAttemptFile{{Source: source, Kind: "sidecar"}}, false); e != nil {
				t.Fatal(e)
			}
			if e = db.RecordImportStage(ctx, a.ImportAttemptKey, source, target, stage); e != nil {
				t.Fatal(e)
			}
			p := ImportPublication{ImportAttemptKey: a.ImportAttemptKey, ID: "sidecar-test", ReceiptID: r.ID, Source: source, Target: target, Payload: `{"stage":"/library/.caption-stage"}`}
			if e = db.AuthorizeImportSidecar(ctx, p); e != nil {
				t.Fatal(e)
			}
			if _, e = db.RequestImportCancellation(ctx, a.ImportAttemptKey); e != nil {
				t.Fatal(e)
			}
			v := DeliveryFileImport{ReceiptID: r.ID, Path: "movie.mkv", SourceGeneration: r.GenerationKey, LibraryPath: target, PlacementID: "caption-placement", LibraryFileGeneration: "caption-generation"}
			trigger := map[string]string{
				"provenance":    `CREATE TRIGGER fail_sidecar BEFORE INSERT ON runner_delivery_file_imports BEGIN SELECT RAISE(ABORT,'ledger unavailable'); END`,
				"stage-release": `CREATE TRIGGER fail_sidecar BEFORE UPDATE ON import_attempt_stages BEGIN SELECT RAISE(ABORT,'stage release unavailable'); END`,
				"disposition":   `CREATE TRIGGER fail_sidecar BEFORE UPDATE ON runner_delivery_file_outcomes BEGIN SELECT RAISE(ABORT,'outcome unavailable'); END`,
				"file-result":   `CREATE TRIGGER fail_sidecar BEFORE UPDATE ON import_attempt_files BEGIN SELECT RAISE(ABORT,'file result unavailable'); END`,
			}[failure]
			if _, e = db.W.ExecContext(ctx, trigger); e != nil {
				t.Fatal(e)
			}
			if e = db.CommitImportSidecar(ctx, p, v); e == nil {
				t.Fatal("partial sidecar result reported complete")
			}
			current, e := db.GetImportAttempt(ctx, r.DownloadID, r.GenerationKey)
			if e != nil || current.Outcome != "pending" || current.PendingPublications != 1 || current.HeldStages != 1 {
				t.Fatalf("failed result released custody: %+v %v", current, e)
			}
			files, e := db.ImportAttemptFiles(ctx, a.ImportAttemptKey)
			if e != nil || files[0].Outcome != "pending" {
				t.Fatalf("partial file result: %+v %v", files, e)
			}
			if _, e = db.FinalizeImportCancellation(ctx, a.ImportAttemptKey); !errors.Is(e, ErrImportPublicationPending) {
				t.Fatal("failed sidecar allowed cancellation finalization", e)
			}
			refs, e := db.unresolvedSidecarReferences(ctx)
			if e != nil || len(refs) != 2 {
				t.Fatalf("stage/publication references lost: %+v %v", refs, e)
			}
			if _, e = db.W.ExecContext(ctx, `DROP TRIGGER fail_sidecar`); e != nil {
				t.Fatal(e)
			}
			if e = db.CommitImportSidecar(ctx, p, v); e != nil {
				t.Fatal(e)
			}
			done, e := db.FinalizeImportCancellation(ctx, a.ImportAttemptKey)
			if e != nil || done.Outcome != "completed" || done.HeldStages != 0 || done.PendingPublications != 0 {
				t.Fatalf("completed sidecar did not settle atomically: %+v %v", done, e)
			}
		})
	}
}
