package acquisition

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// A v1 delivery receipt after the pipeline v2 cutover (contract section
// 3.4, IMPL M2): it is shown, it re-offers the historical publication it
// migrated into, and nothing deletes through it while that publication is
// in the frozen cohort.

type v1Receipt struct {
	f       *v2Fixture
	dl      sqlite.Download
	receipt sqlite.DeliveryReceipt
	pub     sqlite.Publication
	dir     string
}

// migratedReceipt leaves what migration 0072 leaves: a completed row, its
// accepted v1 receipt, and a historical publication at the receipt's path.
func migratedReceipt(t *testing.T, accept bool) *v1Receipt {
	t.Helper()
	f := setupV2(t, nil)
	dl := f.grab(0, seasonPack)
	pub := f.publish("Test.Show.S01.job-55.runner-v1r", "v1r", map[string]string{"Test.Show.S01E01.mkv": "e1", "Test.Show.S01E02.mkv": "e2"})
	m := mustReadManifest(t, pub.Path)
	raw, _ := os.ReadFile(filepath.Join(pub.Path, ports.ManifestFileName))
	v := ports.RunnerDelivery{Sequence: 1, DeliveryID: "d-v1r", PublicationGeneration: "v1r", ArtifactID: "a", JobID: dl.Handle,
		RequestToken: dl.Transfer, RunnerInstanceID: "runner", PublicationPath: "/runner/completed/" + filepath.Base(pub.Path),
		ManifestDigest: m.Digest, ReadyAt: time.Now()}
	for _, file := range m.Files {
		v.Files = append(v.Files, ports.RunnerFile{Path: file.Path, SizeBytes: file.Bytes, SHA256: file.SHA256})
	}
	// Nothing in production writes runner_delivery_receipts since M2, so the
	// receipt is written as a pre-M2 curator left it: discovered, associated
	// with its download and (when accepted) acknowledged to Runner.
	ack, consumer := "pending", ""
	if accept {
		ack, consumer = "accepted", "consumer-1"
	}
	files, err := json.Marshal(v.Files)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	res, err := f.db.W.ExecContext(f.ctx, `INSERT INTO runner_delivery_receipts
		(client_id, delivery_id, sequence, publication_generation, source_artifact_id, source_generation, manifest_digest,
		artifact_id, job_id, request_token, runner_instance_id, ready_at, publication_path, mapped_path, files_json,
		source_cleanup_json, file_outcomes_json, state, ack_state, consumer_receipt_id, download_id, generation_key,
		download_handle, media_item_id, copy_id, error, created_at, updated_at, runner_stages_json, source_copies_json, lifecycle_timing_json)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'null','null','unresolved',?,?,?,?,?,?,?,'',?,?,'null','null','null')`,
		dl.ClientID, v.DeliveryID, v.Sequence, v.PublicationGeneration, v.SourceArtifactID, v.SourceGeneration, v.ManifestDigest,
		v.ArtifactID, v.JobID, v.RequestToken, v.RunnerInstanceID, v.ReadyAt.UnixMilli(), v.PublicationPath, pub.Path, string(files),
		ack, consumer, dl.ID, dl.GenerationKey, dl.Handle, dl.MediaItemID, dl.CopyID, now, now)
	if err != nil {
		t.Fatal(err)
	}
	receiptID, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	r, err := f.db.DeliveryReceiptByID(f.ctx, receiptID)
	if err != nil {
		t.Fatal(err)
	}
	if r, err = f.db.DeliveryReceiptByID(f.ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.W.ExecContext(f.ctx, `UPDATE downloads SET state='downloaded', runner_outcome='completed', import_path=?, save_path=? WHERE id=?`, pub.Path, pub.Path, dl.ID); err != nil {
		t.Fatal(err)
	}
	id, err := f.db.InsertPublication(f.ctx, sqlite.PublicationWrite{DownloadID: dl.ID, Generation: "v1r", Kind: "completed", Path: pub.Path,
		ManifestJSON: string(raw), Source: "v1_record", Cohort: "historical"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.db.GetPublication(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	// Leave it import_failed, as a Resolve candidate is.
	if err := f.db.SetPublicationState(f.ctx, id, "import_failed", "awaiting review"); err != nil {
		t.Fatal(err)
	}
	return &v1Receipt{f: f, dl: f.row(dl.ID), receipt: r, pub: p, dir: pub.Path}
}

// The lifecycle page shows a receipt with its Curator files and history.
func TestDeliveryLifecycleProjection(t *testing.T) {
	v := migratedReceipt(t, true)
	f := v.f
	if _, err := f.svc.DeliveryLifecycle(f.ctx, 0, 0, 0); err == nil {
		t.Fatal("a zero page limit was accepted")
	}
	if _, err := f.db.BeginDeliveryAttempt(f.ctx, v.receipt.ID, "import"); err != nil {
		t.Fatal(err)
	}
	// The file journal row a v1 import wrote before it placed anything.
	if _, err := f.db.W.ExecContext(f.ctx, `INSERT INTO runner_delivery_file_outcomes
		(receipt_id,path,size_bytes,sha256,file_generation,disposition,cleanup_state,updated_at)
		VALUES(?,'Test.Show.S01E01.mkv',2,'','g1','pending','protected',?)`, v.receipt.ID, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	all, err := f.svc.DeliveryLifecycle(f.ctx, 0, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	mine, err := f.svc.DeliveryLifecycle(f.ctx, 0, v.dl.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Items) != 1 || len(mine.Items) != 1 {
		t.Fatalf("pages = %d / %d, want the receipt in both", len(all.Items), len(mine.Items))
	}
	got := mine.Items[0]
	if got.ReceiptID != v.receipt.ID || got.DownloadID != v.dl.ID || got.Acceptance != "accepted" || got.DeliveryID != "d-v1r" ||
		len(got.ManifestFiles) != 2 || len(got.CuratorFiles) != 1 || len(got.AttemptHistory) != 1 || got.AttemptHistory[0].Stage != "import" {
		t.Fatalf("projection = %+v", got)
	}
	byID, err := f.svc.DeliveryLifecycleByID(f.ctx, v.receipt.ID)
	if err != nil || byID.ReceiptID != v.receipt.ID {
		t.Fatalf("by id = %+v (%v)", byID.ReceiptID, err)
	}
	if _, err := f.svc.DeliveryLifecycleByID(f.ctx, v.receipt.ID+50); err == nil {
		t.Fatal("an unknown receipt was projected")
	}
}

// Cleanup through a v1 receipt: Runner retires its own source, and the
// historical cohort is frozen until its review batch is approved.
func TestReceiptCleanupIsRefusedAfterCutover(t *testing.T) {
	v := migratedReceipt(t, true)
	f := v.f
	if err := f.svc.RetryRunnerSourceCleanup(f.ctx, v.receipt.ID); !errors.Is(err, ErrRunnerOwnsSource) {
		t.Fatalf("source cleanup = %v, want Runner's to retire", err)
	}
	if err := f.svc.RetryRunnerSourceCleanup(f.ctx, v.receipt.ID+50); err == nil || errors.Is(err, ErrRunnerOwnsSource) {
		t.Fatalf("an unknown receipt = %v, want not found", err)
	}
	if err := f.svc.RetryCuratorDeliveryCleanup(f.ctx, v.receipt.ID); !errors.Is(err, ErrHistoricalCohort) {
		t.Fatalf("curator cleanup = %v, want the historical cohort refusal", err)
	}
	for _, name := range []string{"Test.Show.S01E01.mkv", "Test.Show.S01E02.mkv"} {
		if _, err := os.Stat(filepath.Join(v.dir, name)); err != nil {
			t.Fatalf("a historical file was touched: %v", err)
		}
	}
	if _, err := f.db.W.ExecContext(f.ctx, `UPDATE runner_delivery_receipts SET state='conflict', error='identity changed' WHERE id=?`, v.receipt.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.RetryCuratorDeliveryCleanup(f.ctx, v.receipt.ID); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("a conflicted receipt = %v, want refused", err)
	}
	if err := f.svc.RetryDeliveryHandoff(f.ctx, v.receipt.ID); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("handoff of a conflicted receipt = %v, want refused", err)
	}
	if err := f.svc.QueueDeliveryImport(f.ctx, v.receipt.ID, []string{"Test.Show.S01E01.mkv"}); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("a selection on a conflicted receipt = %v, want refused", err)
	}
}

// Retrying a receipt's handoff re-offers its historical publication, which
// imports through the v2 path and stays historical.
func TestReceiptHandoffImportsItsPublication(t *testing.T) {
	v := migratedReceipt(t, true)
	f := v.f
	if err := f.svc.RetryDeliveryHandoff(f.ctx, v.receipt.ID); err != nil {
		t.Fatal(err)
	}
	p, _ := f.db.GetPublication(f.ctx, v.pub.ID)
	if p.State != "imported" || p.Cohort != "historical" || f.row(v.dl.ID).State != "imported" {
		t.Fatalf("publication %s/%s row %s (%s), want imported and still historical", p.State, p.Cohort, f.row(v.dl.ID).State, f.row(v.dl.ID).Error)
	}
	if n := f.count(`SELECT COUNT(*) FROM media_files WHERE media_item_id=?`, f.item); n != 2 {
		t.Fatalf("library files = %d, want both episodes", n)
	}
}

// A receipt with no publication row to import into says so.
func TestReceiptWithoutAPublicationCannotImport(t *testing.T) {
	v := migratedReceipt(t, true)
	f := v.f
	if _, err := f.db.W.ExecContext(f.ctx, `DELETE FROM publication_files`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.W.ExecContext(f.ctx, `DELETE FROM publications`); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.RetryDeliveryHandoff(f.ctx, v.receipt.ID); err == nil || !strings.Contains(err.Error(), "no publication") {
		t.Fatalf("err = %v, want no publication", err)
	}
	// Cleanup is the publication's per-file cleanup (M4): with no
	// publication there is nothing to clean, so the retry is refused and
	// nothing is deleted.
	if err := f.svc.RetryCuratorDeliveryCleanup(f.ctx, v.receipt.ID); err == nil || !strings.Contains(err.Error(), "no publication to clean up") {
		t.Fatalf("err = %v, want no publication to clean up", err)
	}
	if _, err := os.Stat(filepath.Join(v.dir, "Test.Show.S01E01.mkv")); err != nil {
		t.Fatal("a payload file was deleted")
	}
}

// A Resolve selection on a receipt imports exactly the selected manifest
// files; anything that is not an exact manifest path is refused.
func TestQueueDeliveryImportSelection(t *testing.T) {
	t.Run("refusals", func(t *testing.T) {
		v := migratedReceipt(t, true)
		f := v.f
		for name, paths := range map[string][]string{
			"none":          nil,
			"not manifest":  {"Other.mkv"},
			"absolute":      {"/" + "Test.Show.S01E01.mkv"},
			"unclean":       {"./Test.Show.S01E01.mkv"},
			"parent escape": {"../Test.Show.S01E01.mkv"},
		} {
			if err := f.svc.QueueDeliveryImport(f.ctx, v.receipt.ID, paths); err == nil {
				t.Errorf("%s: accepted", name)
			}
		}
		if err := f.svc.QueueDeliveryImport(f.ctx, v.receipt.ID+50, []string{"Test.Show.S01E01.mkv"}); err == nil {
			t.Error("an unknown receipt accepted a selection")
		}
	})
	t.Run("an unprepared acceptance", func(t *testing.T) {
		v := migratedReceipt(t, false)
		if _, err := v.f.db.W.ExecContext(v.f.ctx, `UPDATE runner_delivery_receipts SET ack_state='pending', consumer_receipt_id='' WHERE id=?`, v.receipt.ID); err != nil {
			t.Fatal(err)
		}
		if err := v.f.svc.QueueDeliveryImport(v.f.ctx, v.receipt.ID, []string{"Test.Show.S01E01.mkv"}); err == nil || !strings.Contains(err.Error(), "not durably prepared") {
			t.Fatalf("err = %v, want the acceptance refused", err)
		}
	})
	t.Run("a changed download generation", func(t *testing.T) {
		v := migratedReceipt(t, true)
		if _, err := v.f.db.W.ExecContext(v.f.ctx, `UPDATE downloads SET handle='other' WHERE id=?`, v.dl.ID); err != nil {
			t.Fatal(err)
		}
		if err := v.f.svc.QueueDeliveryImport(v.f.ctx, v.receipt.ID, []string{"Test.Show.S01E01.mkv"}); err == nil || !strings.Contains(err.Error(), "generation changed") {
			t.Fatalf("err = %v, want the generation change refused", err)
		}
	})
	t.Run("exactly the selection imports", func(t *testing.T) {
		v := migratedReceipt(t, true)
		f := v.f
		if err := f.svc.QueueDeliveryImport(f.ctx, v.receipt.ID, []string{"Test.Show.S01E02.mkv", "Test.Show.S01E02.mkv"}); err != nil {
			t.Fatal(err)
		}
		files, _ := f.db.ListPublicationFiles(f.ctx, v.pub.ID)
		got := map[string]string{}
		for _, file := range files {
			got[file.Path] = file.Outcome
		}
		if got["Test.Show.S01E02.mkv"] != "placed" || got["Test.Show.S01E01.mkv"] == "placed" {
			t.Fatalf("outcomes = %v (%s), want only the selected E02 placed", got, f.row(v.dl.ID).Error)
		}
	})
}
