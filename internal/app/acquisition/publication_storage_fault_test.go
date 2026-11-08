package acquisition

import (
	"fmt"
	"testing"

	"github.com/pjunod/monarr/internal/ports"
)

// Contract section 3.4: a completed terminal's publication imports on its
// own lifecycle, file by file, with per-file outcomes. Fail every storage call the terminal and its import make, on either pool;
// after a replay of the same fact and the next polls, the row and its
// publication are imported with exactly one library file, its placement
// receipt and one imported event, or the import has failed visibly with its
// reason and no imported event. Never a second publication or file, never a
// library file without its placement receipt.

func TestPublicationImportStorageFaultsNeverDuplicateOrOrphan(t *testing.T) {
	for _, write := range []bool{true, false} {
		t.Run(fmt.Sprintf("write=%t", write), func(t *testing.T) {
			eachStorageFault(t, func(t *testing.T, sf *storageFault) {
				runner := newFakeRunner()
				svc, db, item := setupOn(t, migratedTestDB(t), nil, &runner.fakeClient)
				f := setupV2On(t, runner, svc, db, item)
				dl := f.grab(1, epOne)
				pub := f.publish("Test.Show.S01E01.job-101.runner-fault", "fault", map[string]string{epOne + ".mkv": "video"})
				fact := terminalFact(dl, ports.RunnerTerminal{Outcome: ports.OutcomeCompleted, Publication: &pub, Salvage: "none"}, "event 1")
				var faulted error
				withStorageFault(t, f.db, write, sf, func() { faulted = f.svc.observe(f.ctx, dl, f.cfg, fact) })
				if !sf.fired.Load() && faulted != nil {
					t.Fatalf("unfaulted terminal: %v", faulted)
				}
				// Runner redelivers the fact (the event stream or the history
				// page); the poll then resumes whatever import is left.
				_ = f.svc.observe(f.ctx, f.row(dl.ID), f.cfg, fact)
				for i := 0; i < 2; i++ {
					if err := f.svc.RefreshQueue(f.ctx); err != nil {
						t.Fatalf("poll after failure (err=%v): %v", faulted, err)
					}
				}
				p, ok, err := f.db.PublicationByPath(f.ctx, pub.Path)
				if err != nil || !ok {
					t.Fatalf("publication after failure (err=%v): ok=%v %v", faulted, ok, err)
				}
				got := f.row(dl.ID)
				files := f.count(`SELECT COUNT(*) FROM media_files WHERE media_item_id=?`, f.item)
				receipts := f.count(`SELECT COUNT(*) FROM media_file_placements p JOIN media_files m ON m.id=p.media_file_id WHERE m.media_item_id=?`, f.item)
				imported := f.count(`SELECT COUNT(*) FROM history_events WHERE type='imported' AND media_item_id=?`, f.item)
				publications := f.count(`SELECT COUNT(*) FROM publications WHERE download_id=?`, dl.ID)
				if publications != 1 || files > 1 || files != receipts || imported > 1 {
					t.Fatalf("after failure (err=%v): %d publications, %d files, %d placement receipts, %d imported events", faulted, publications, files, receipts, imported)
				}
				switch {
				case got.State == "imported" && (p.State == "imported" || p.State == "removed"):
					// Removed is the per-file cleanup (M4) finishing after the
					// import; a fault inside that cleanup leaves the
					// publication imported for the next pass.
					if files != 1 || imported != 1 {
						t.Fatalf("imported with %d files and %d imported events", files, imported)
					}
					if !sf.fired.Load() && p.State != "removed" {
						t.Fatalf("unfaulted import left the publication %s (%s), want it cleaned up", p.State, p.Error)
					}
				case got.State == "failed" && p.State == "import_failed" && sf.fired.Load():
					// A failure inside placement ends the import visibly, with
					// its reason on both, and the stage it retained waits for
					// an explicit retry; the poll does not resume it.
					if p.Error == "" || got.Error == "" || imported != 0 {
						t.Fatalf("failed import without its reason or with an imported event: row %q, publication %q, %d imported", got.Error, p.Error, imported)
					}
				default:
					t.Fatalf("after failure (err=%v) row %s (%s), publication %s (%s)", faulted, got.State, got.Error, p.State, p.Error)
				}
			})
		})
	}
}
