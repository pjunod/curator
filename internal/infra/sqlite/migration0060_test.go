package sqlite

import (
	"context"
	"testing"

	"github.com/pressly/goose/v3"
)

func TestExactTransferCorrelationMigrationRefusesLossyDowngrade(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpToContext(ctx, db.W, "migrations", 59); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `INSERT INTO media_items(id,kind,title,sort_title,added_at,updated_at)
		VALUES(1,'movie','Exact','exact',0,0)`); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpToContext(ctx, db.W, "migrations", 60); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `INSERT INTO downloads(id,media_item_id,protocol,release_title,state,added_at,updated_at,exact_transfer_correlation)
		VALUES(1,1,'torrent','exact','grabbed',1,1,1)`); err != nil {
		t.Fatal(err)
	}
	if err := goose.DownToContext(ctx, db.W, "migrations", 59); err == nil {
		t.Fatal("downgrade discarded exact transfer-correlation authority")
	}
	var exact int
	if err := db.R.QueryRowContext(ctx, `SELECT exact_transfer_correlation FROM downloads WHERE id=1`).Scan(&exact); err != nil || exact != 1 {
		t.Fatalf("refused downgrade changed exact-correlation row: value=%d err=%v", exact, err)
	}
}
