package acquisition

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	sqlitegen "github.com/pjunod/monarr/internal/infra/sqlite/gen"
	"github.com/pjunod/monarr/internal/infra/sqlite/sqlitetest"
	sqliteDriver "modernc.org/sqlite"
)

// A real SQLite connection that fails its at-th driver call (begin, exec,
// query, row step, RowsAffected, LastInsertId or commit), so a service
// operation can be driven through every storage failure it can meet.
// malformed instead hands back a NULL first column at that row step.

var errStorageFault = errors.New("injected storage failure")
var storageFaultID atomic.Int64

type storageFault struct {
	at        int
	calls     atomic.Int64
	fired     atomic.Bool
	malformed bool
}

func (f *storageFault) trip() error {
	if int(f.calls.Add(1)) == f.at {
		f.fired.Store(true)
		return errStorageFault
	}
	return nil
}

func (f *storageFault) Open(name string) (driver.Conn, error) {
	c, err := (&sqliteDriver.Driver{}).Open(name)
	if err != nil {
		return nil, err
	}
	return &faultConn{Conn: c, f: f}, nil
}

type faultConn struct {
	driver.Conn
	f *storageFault
}

func (c *faultConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}
func (c *faultConn) BeginTx(ctx context.Context, o driver.TxOptions) (driver.Tx, error) {
	if err := c.f.trip(); err != nil {
		return nil, err
	}
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, o)
	if err != nil {
		return nil, err
	}
	return &faultTx{Tx: tx, f: c.f}, nil
}
func (c *faultConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	if err := c.f.trip(); err != nil {
		return nil, err
	}
	r, err := c.Conn.(driver.ExecerContext).ExecContext(ctx, q, args)
	if err != nil {
		return nil, err
	}
	return &faultResult{Result: r, f: c.f}, nil
}
func (c *faultConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	if err := c.f.trip(); err != nil {
		return nil, err
	}
	r, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, q, args)
	if err != nil {
		return nil, err
	}
	return &faultRows{Rows: r, f: c.f}, nil
}

type faultResult struct {
	driver.Result
	f *storageFault
}

func (r *faultResult) RowsAffected() (int64, error) {
	if err := r.f.trip(); err != nil {
		return 0, err
	}
	return r.Result.RowsAffected()
}
func (r *faultResult) LastInsertId() (int64, error) {
	if err := r.f.trip(); err != nil {
		return 0, err
	}
	return r.Result.LastInsertId()
}

type faultRows struct {
	driver.Rows
	f *storageFault
}

func (r *faultRows) Next(dest []driver.Value) error {
	if err := r.f.trip(); err != nil {
		if r.f.malformed {
			if next := r.Rows.Next(dest); next == nil {
				dest[0] = nil
				return nil
			}
		}
		return err
	}
	return r.Rows.Next(dest)
}

type faultTx struct {
	driver.Tx
	f *storageFault
}

func (tx *faultTx) Commit() error {
	if err := tx.f.trip(); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Tx.Commit()
}

// withStorageFault routes one side of db's pool (the single writer, or the
// read pool) through a connection that fails its at-th call, for op only.
func withStorageFault(t *testing.T, db *sqlite.DB, write bool, f *storageFault, op func()) {
	t.Helper()
	name := fmt.Sprintf("acquisition-fault-%d", storageFaultID.Add(1))
	sql.Register(name, f)
	dsn := "file:" + db.Path + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	if !write {
		dsn = "file:" + db.Path + "?_pragma=query_only(1)&_pragma=busy_timeout(5000)"
	}
	conn, err := sql.Open(name, dsn)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetMaxOpenConns(1)
	if write {
		w, q := db.W, db.Write
		db.W, db.Write = conn, sqlitegen.New(conn)
		defer func() { db.W, db.Write = w, q }()
	} else {
		r, q := db.R, db.Read
		db.R, db.Read = conn, sqlitegen.New(conn)
		defer func() { db.R, db.Read = r, q }()
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	op()
}

// readFaultMatrix fails each read of call in turn, plain and malformed, and
// requires every injected failure to come back as an error: a failed read is
// never an empty or partial success. An unfaulted run must succeed.
func readFaultMatrix(t *testing.T, db *sqlite.DB, calls map[string]func() error) {
	t.Helper()
	for name, call := range calls {
		for _, malformed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/malformed=%t", name, malformed), func(t *testing.T) {
				for point := 1; point <= 64; point++ {
					f := &storageFault{at: point, malformed: malformed}
					var err error
					withStorageFault(t, db, false, f, func() { err = call() })
					if !f.fired.Load() {
						if err != nil {
							t.Fatalf("unfaulted %s failed: %v", name, err)
						}
						return
					}
					if err == nil {
						t.Fatalf("read failure %d of %s returned success", point, name)
					}
				}
				t.Fatalf("%s never completed without an injected failure", name)
			})
		}
	}
}

// eachStorageFault runs one unfaulted pass to count the storage calls an
// operation makes on a fresh fixture, then reruns it once per call with that
// call failing, in parallel. run builds its own fixture, injects f around the
// operation under test with withStorageFault and asserts on the result.
func eachStorageFault(t *testing.T, run func(t *testing.T, f *storageFault)) {
	t.Helper()
	probe := &storageFault{at: -1}
	t.Run("unfaulted", func(t *testing.T) { run(t, probe) })
	calls := int(probe.calls.Load())
	if calls == 0 {
		t.Fatal("operation made no storage calls through the injected connection")
	}
	for point := 1; point <= calls; point++ {
		t.Run(fmt.Sprintf("fail-%d", point), func(t *testing.T) {
			t.Parallel()
			run(t, &storageFault{at: point})
		})
	}
}

// migratedTestDB opens a private copy of the migrated schema template, as
// setup does, without migrating again: goose keeps global state that parallel
// fault points would race on.
func migratedTestDB(t *testing.T) *sqlite.DB {
	t.Helper()
	db, err := sqlite.Open(sqlitetest.Dir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
