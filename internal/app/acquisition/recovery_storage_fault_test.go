package acquisition

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/pjunod/monarr/internal/infra/sqlite"
)

// Recovery imports are operator-approved writes into the library from a
// Runner recovery. A storage failure anywhere must be visible (an error, or a
// recovery held for review with the reason), must never be read as an empty
// or partial preview, and must never leave the library half-committed: the
// title ends with either its one recovered file and the durable receipt, or
// with nothing.

func TestRecoveryReadsFailClosedOnStorageFaults(t *testing.T) {
	ctx := context.Background()
	svc, db, req, _ := recoveryFixture(t)
	preview, err := svc.PreviewRecovery(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	job, err := svc.QueueRecovery(ctx, preview.Request)
	if err != nil {
		t.Fatal(err)
	}
	readFaultMatrix(t, db, map[string]func() error{
		"preview":    func() error { _, err := svc.PreviewRecovery(ctx, req); return err },
		"imports":    func() error { _, err := svc.RecoveryImports(ctx); return err },
		"import":     func() error { _, err := svc.RecoveryImport(ctx, job.ID); return err },
		"metrics":    func() error { _, err := svc.RecoveryMetrics(ctx); return err },
		"settings":   func() error { _, err := svc.RecoverySettings(ctx); return err },
		"recoveries": func() error { _, err := svc.RunnerRecoveries(ctx); return err },
	})
}

// A storage failure while accepting or cancelling a recovery is reported, and
// the operator's retry converges on exactly one durable request.
func TestRecoveryAdmissionStorageFaultsAreReportedAndRetrySafe(t *testing.T) {
	ctx := context.Background()
	for _, op := range []string{"queue", "cancel", "settings"} {
		t.Run(op, func(t *testing.T) {
			for point := 1; point <= 64; point++ {
				svc, db, req, _ := fastRecoveryFixture(t)
				preview, err := svc.PreviewRecovery(ctx, req)
				if err != nil {
					t.Fatal(err)
				}
				var job RecoveryImport
				if op == "cancel" {
					if job, err = svc.QueueRecovery(ctx, preview.Request); err != nil {
						t.Fatal(err)
					}
				}
				run := func() error {
					switch op {
					case "queue":
						var e error
						job, e = svc.QueueRecovery(ctx, preview.Request)
						return e
					case "cancel":
						return svc.CancelRecoveryImport(ctx, job.ID)
					default:
						return svc.SetRecoverySettings(ctx, RecoverySettings{LocalRoot: "/elsewhere", RemoteRoot: "/published"})
					}
				}
				f := &storageFault{at: point}
				var faulted error
				withStorageFault(t, db, true, f, func() { faulted = run() })
				if !f.fired.Load() {
					if faulted != nil {
						t.Fatalf("unfaulted %s failed: %v", op, faulted)
					}
					return
				}
				if faulted == nil {
					// Only a failure after the durable write may be absorbed.
					t.Logf("%s absorbed storage failure %d after its write", op, point)
				} else if !errors.Is(faulted, errStorageFault) {
					t.Fatalf("storage failure %d of %s surfaced as %v", point, op, faulted)
				}
				// A cancel whose write landed before the failure is already
				// pending, so its retry is refused; the state check below holds.
				if err := run(); err != nil && op != "cancel" {
					t.Fatalf("retry of %s after storage failure %d: %v", op, point, err)
				}
				var imports int
				var state string
				if err := db.R.QueryRowContext(ctx, `SELECT count(*), COALESCE(max(state),'') FROM recovery_imports`).Scan(&imports, &state); err != nil {
					t.Fatal(err)
				}
				switch op {
				case "queue":
					if imports != 1 || state != "queued" {
						t.Fatalf("retry after failure %d left %d imports in %q", point, imports, state)
					}
				case "cancel":
					if imports != 1 || state != "cancel_pending" {
						t.Fatalf("cancel retry after failure %d left %d imports in %q", point, imports, state)
					}
				default:
					cfg, err := svc.RecoverySettings(ctx)
					if err != nil || cfg.LocalRoot != "/elsewhere" || cfg.ConsumerToken != "credential" {
						t.Fatalf("settings retry after failure %d = %+v %v", point, cfg, err)
					}
				}
			}
			t.Fatalf("%s never completed without an injected failure", op)
		})
	}
}

// The worker meets a storage failure at every point of one import. Whatever
// it hit, the next sweeps leave the recovery imported with exactly one file
// and its durable receipt, or held for review with the reason and no library
// file: never stranded, never duplicated.
func TestRecoverySweepStorageFaultsNeverStrandOrDuplicate(t *testing.T) {
	ctx := context.Background()
	for _, write := range []bool{true, false} {
		t.Run(fmt.Sprintf("write=%t", write), func(t *testing.T) {
			eachStorageFault(t, func(t *testing.T, f *storageFault) {
				svc, db, req, receipts := fastRecoveryFixture(t)
				preview, err := svc.PreviewRecovery(ctx, req)
				if err != nil {
					t.Fatal(err)
				}
				job, err := svc.QueueRecovery(ctx, preview.Request)
				if err != nil {
					t.Fatal(err)
				}
				withStorageFault(t, db, write, f, func() { svc.recoverySweep(ctx) })
				for i := 0; i < 3; i++ {
					svc.recoverySweep(ctx)
				}
				final, err := svc.RecoveryImport(ctx, job.ID)
				if err != nil {
					t.Fatal(err)
				}
				files, err := db.ListFilesForItem(ctx, req.MediaItemID)
				if err != nil {
					t.Fatal(err)
				}
				var results int
				if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM recovery_file_results WHERE import_id=?`, job.ID).Scan(&results); err != nil {
					t.Fatal(err)
				}
				switch final.State {
				case "imported":
					if len(files) != 1 || results != 1 || receipts.Load() < 1 {
						t.Fatalf("imported with files=%d results=%d receipts=%d", len(files), results, receipts.Load())
					}
				case "review":
					if !f.fired.Load() {
						t.Fatalf("unfaulted sweep held the recovery for review: %s", final.Error)
					}
					if final.Error == "" {
						t.Fatal("held for review without a reason")
					}
					if len(files) > 1 || (len(files) == 1) != (results == 1) {
						t.Fatalf("review hold with files=%d results=%d", len(files), results)
					}
				default:
					t.Fatalf("recovery stranded in %q (%s)", final.State, final.Error)
				}
			})
		})
	}
}

// fastRecoveryFixture is recoveryFixture on a copy of the migrated template.
func fastRecoveryFixture(t *testing.T) (*Service, *sqlite.DB, RecoveryRequest, *atomic.Int32) {
	t.Helper()
	svc, db, itemID := autoSetupOn(t, migratedTestDB(t), nil, &fakeClient{})
	return recoveryFixtureOn(t, svc, db, itemID, "Recovered.2024.1080p.WEB-DL.mkv", false)
}

// A cancelled recovery is reconciled with Runner and finished by the sweep.
// Whatever storage call of that sweep fails, the next sweeps finish it
// cancelled, with nothing placed in the library and no receipt sent.
func TestRecoveryCancelStorageFaultsStillFinishCancelled(t *testing.T) {
	ctx := context.Background()
	for _, write := range []bool{true, false} {
		t.Run(fmt.Sprintf("write=%t", write), func(t *testing.T) {
			eachStorageFault(t, func(t *testing.T, f *storageFault) {
				svc, db, req, receipts := fastRecoveryFixture(t)
				preview, err := svc.PreviewRecovery(ctx, req)
				if err != nil {
					t.Fatal(err)
				}
				job, err := svc.QueueRecovery(ctx, preview.Request)
				if err != nil {
					t.Fatal(err)
				}
				if err := svc.CancelRecoveryImport(ctx, job.ID); err != nil {
					t.Fatal(err)
				}
				withStorageFault(t, db, write, f, func() { svc.recoverySweep(ctx) })
				for i := 0; i < 3; i++ {
					svc.recoverySweep(ctx)
				}
				final, err := svc.RecoveryImport(ctx, job.ID)
				if err != nil {
					t.Fatal(err)
				}
				files, err := db.ListFilesForItem(ctx, req.MediaItemID)
				if err != nil {
					t.Fatal(err)
				}
				if final.State != "cancelled" || len(files) != 0 || receipts.Load() != 0 {
					t.Fatalf("after failure: state %q (%s), %d library files, %d receipts", final.State, final.Error, len(files), receipts.Load())
				}
			})
		})
	}
}
