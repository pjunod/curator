package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	sqliteDriver "modernc.org/sqlite"
)

// Exercise real SQLite transactions with one injected storage failure. The
// wrapper forwards every successful operation; snapshots inspect the independent
// read pool so an error cannot hide partially committed durable state.
var importFaultDriverID atomic.Int64
var errImportStorageFault = errors.New("injected import storage failure")

type importFaultDriver struct {
	at, calls int
	fired     bool
}

func (d *importFaultDriver) trip() error {
	d.calls++
	if d.calls == d.at {
		d.fired = true
		return errImportStorageFault
	}
	return nil
}
func (d *importFaultDriver) Open(name string) (driver.Conn, error) {
	c, e := (&sqliteDriver.Driver{}).Open(name)
	if e != nil {
		return nil, e
	}
	return &importFaultConn{Conn: c, fault: d}, nil
}

type importFaultConn struct {
	driver.Conn
	fault *importFaultDriver
}

func (c *importFaultConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}
func (c *importFaultConn) BeginTx(ctx context.Context, o driver.TxOptions) (driver.Tx, error) {
	if e := c.fault.trip(); e != nil {
		return nil, e
	}
	tx, e := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, o)
	if e != nil {
		return nil, e
	}
	return &importFaultTx{Tx: tx, fault: c.fault}, nil
}
func (c *importFaultConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	if e := c.fault.trip(); e != nil {
		return nil, e
	}
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, q, args)
}
func (c *importFaultConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	if e := c.fault.trip(); e != nil {
		return nil, e
	}
	rows, e := c.Conn.(driver.QueryerContext).QueryContext(ctx, q, args)
	if e != nil {
		return nil, e
	}
	return &importFaultRows{Rows: rows, fault: c.fault}, nil
}

type importFaultRows struct {
	driver.Rows
	fault *importFaultDriver
}

func (r *importFaultRows) Next(dest []driver.Value) error {
	if e := r.fault.trip(); e != nil {
		return e
	}
	return r.Rows.Next(dest)
}

type importFaultTx struct {
	driver.Tx
	fault *importFaultDriver
}

func (tx *importFaultTx) Commit() error {
	if e := tx.fault.trip(); e != nil {
		_ = tx.Tx.Rollback()
		return e
	}
	return tx.Tx.Commit()
}

func importDurableSnapshot(t *testing.T, db *DB) string {
	t.Helper()
	all := map[string][]any{}
	for _, table := range []string{"downloads", "media_files", "media_file_placements", "import_placements", "import_attempt_controls", "import_attempt_files", "import_publication_authorizations", "import_attempt_stages", "lifecycle_library_revision", "history_events", "runner_delivery_file_imports", "runner_delivery_file_outcomes"} {
		rows, e := db.R.QueryContext(t.Context(), `SELECT * FROM `+table+` ORDER BY rowid`)
		if e != nil {
			t.Fatal(e)
		}
		columns, e := rows.Columns()
		if e != nil {
			_ = rows.Close()
			t.Fatal(e)
		}
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if e = rows.Scan(pointers...); e != nil {
				_ = rows.Close()
				t.Fatal(e)
			}
			all[table] = append(all[table], values)
		}
		if e = rows.Err(); e != nil {
			_ = rows.Close()
			t.Fatal(e)
		}
		if e = rows.Close(); e != nil {
			t.Fatal(e)
		}
	}
	raw, e := json.Marshal(all)
	if e != nil {
		t.Fatal(e)
	}
	return string(raw)
}

func TestImportStorageFailuresCannotCommitPartialAuthority(t *testing.T) {
	for _, operation := range []string{"admit", "retry", "seal", "grant", "media-result", "cancel", "finalize", "retain", "dispose", "stage", "reuse", "resolve", "complete", "sidecar"} {
		t.Run(operation, func(t *testing.T) {
			completed := false
			for point := 1; point <= 80; point++ {
				ctx := context.Background()
				var db *DB
				var dl Download
				var a ImportAttempt
				var receipt DeliveryReceipt
				if operation == "sidecar" {
					var original Placement
					db, receipt, original = atomicDeliveryFixture(t)
					var e error
					dl, e = db.GetDownload(ctx, receipt.DownloadID)
					if e != nil {
						t.Fatal(e)
					}
					a, e = db.EnsureImportAttempt(ctx, dl.ID, dl.GenerationKey)
					if e != nil {
						t.Fatal(e)
					}
					_ = original
				} else {
					db, dl, a = importControlFixture(t)
				}
				p := controlPlacement(dl, a, "fault-model", "/source/video", "/library/video")
				if operation != "seal" {
					mustSeal(t, db, a, p.Source)
				}
				if operation != "sidecar" {
					if e := db.PreparePlacement(ctx, p); e != nil {
						t.Fatal(e)
					}
				}
				var sidecar ImportPublication
				if operation == "sidecar" {
					sidecar = ImportPublication{ImportAttemptKey: a.ImportAttemptKey, ID: "fault-sidecar", ReceiptID: receipt.ID, Source: p.Source, Target: p.Target, Payload: `{"stage":"/library/.sidecar-stage"}`}
					if e := db.RecordImportStage(ctx, a.ImportAttemptKey, p.Source, p.Target, "/library/.sidecar-stage"); e != nil {
						t.Fatal(e)
					}
					if e := db.AuthorizeImportSidecar(ctx, sidecar); e != nil {
						t.Fatal(e)
					}
				}
				if operation == "complete" {
					if e := db.ResolveImportFile(ctx, a.ImportAttemptKey, p.Source, "skipped", ""); e != nil {
						t.Fatal(e)
					}
				}
				if operation == "media-result" || operation == "retain" || operation == "dispose" || operation == "reuse" {
					if e := db.AuthorizeImportPlacement(ctx, p); e != nil {
						t.Fatal(e)
					}
				}
				if operation == "retain" || operation == "dispose" || operation == "finalize" {
					if _, e := db.RequestImportCancellation(ctx, a.ImportAttemptKey); e != nil {
						t.Fatal(e)
					}
				}
				if operation == "dispose" {
					if _, e := db.RetainUncertainImport(ctx, a.ImportAttemptKey, []string{"placement:" + p.ID}); e != nil {
						t.Fatal(e)
					}
				}
				if operation == "reuse" {
					if _, e := db.CommitPlacement(ctx, p); e != nil {
						t.Fatal(e)
					}
					var e error
					a, e = db.BeginImportRetry(ctx, dl.ID, dl.GenerationKey)
					if e != nil {
						t.Fatal(e)
					}
					mustSeal(t, db, a, p.Source)
				}
				if operation == "admit" {
					if _, e := db.W.ExecContext(ctx, `DELETE FROM import_attempt_files WHERE download_id=?`, dl.ID); e != nil {
						t.Fatal(e)
					}
					if _, e := db.W.ExecContext(ctx, `DELETE FROM import_attempt_controls WHERE download_id=?`, dl.ID); e != nil {
						t.Fatal(e)
					}
				}
				before := importDurableSnapshot(t, db)
				fault := &importFaultDriver{at: point}
				name := fmt.Sprintf("import-fault-%d", importFaultDriverID.Add(1))
				sql.Register(name, fault)
				writer, e := sql.Open(name, "file:"+db.Path+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
				if e != nil {
					t.Fatal(e)
				}
				writer.SetMaxOpenConns(1)
				original := db.W
				t.Cleanup(func() { db.W = original; _ = writer.Close() })
				db.W = writer
				switch operation {
				case "admit":
					_, e = db.EnsureImportAttempt(ctx, dl.ID, dl.GenerationKey)
				case "retry":
					_, e = db.BeginImportRetry(ctx, dl.ID, dl.GenerationKey)
				case "seal":
					e = db.SealImportAttempt(ctx, a.ImportAttemptKey, []ImportAttemptFile{{Source: p.Source, Kind: "media"}}, false)
				case "grant":
					e = db.AuthorizeImportPlacement(ctx, p)
				case "media-result":
					_, e = db.CommitPlacement(ctx, p)
				case "cancel":
					_, e = db.RequestImportCancellation(ctx, a.ImportAttemptKey)
				case "finalize":
					_, e = db.FinalizeImportCancellation(ctx, a.ImportAttemptKey)
				case "retain":
					_, e = db.RetainUncertainImport(ctx, a.ImportAttemptKey, []string{"placement:" + p.ID})
				case "dispose":
					e = db.ResolveRetainedImportTargets(ctx, a.ImportAttemptKey, []string{"placement:" + p.ID}, "operator_disposed")
				case "stage":
					e = db.RecordImportStage(ctx, a.ImportAttemptKey, p.Source, p.Target, p.Temporary)
				case "reuse":
					e = db.SatisfyImportFile(ctx, a.ImportAttemptKey, p)
				case "resolve":
					e = db.ResolveImportFile(ctx, a.ImportAttemptKey, p.Source, "skipped", "")
				case "complete":
					e = db.CompleteImportAttempt(ctx, a.ImportAttemptKey)
				case "sidecar":
					e = db.CommitImportSidecar(ctx, sidecar, DeliveryFileImport{ReceiptID: receipt.ID, Path: "movie.mkv", LibraryPath: p.Target, SourceGeneration: dl.GenerationKey})
				}
				db.W = original
				if closeErr := writer.Close(); closeErr != nil {
					t.Fatal(closeErr)
				}
				if !fault.fired {
					if e != nil {
						t.Fatalf("valid operation %s failed without injection: %v", operation, e)
					}
					if after := importDurableSnapshot(t, db); after == before {
						t.Fatalf("successful %s did not reach its durable mutation", operation)
					}
					if e := db.Close(); e != nil {
						t.Fatal(e)
					}
					completed = true
					break
				}
				if e == nil {
					t.Fatalf("%s storage failure %d reported success", operation, point)
				}
				if after := importDurableSnapshot(t, db); after != before {
					t.Fatalf("%s failure %d partially committed durable state", operation, point)
				}
				if e := db.Close(); e != nil {
					t.Fatal(e)
				}
			}
			if !completed {
				t.Fatal("operation never completed without an injected failure")
			}
		})
	}
}

var _ driver.ExecerContext = (*importFaultConn)(nil)
var _ driver.QueryerContext = (*importFaultConn)(nil)
var _ driver.ConnBeginTx = (*importFaultConn)(nil)
