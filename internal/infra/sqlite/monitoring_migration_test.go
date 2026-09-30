package sqlite

import (
	"context"
	"testing"

	"github.com/pressly/goose/v3"
)

func TestMonitoringMigrationPreservesExistingSelections(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpToContext(ctx, db.W, "migrations", 35); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`INSERT INTO media_items (id,kind,title,sort_title,monitored,added_at,updated_at) VALUES (1,'series','Show','show',0,0,0)`,
		`INSERT INTO seasons (media_item_id,number,monitored) VALUES (1,1,0)`,
		`INSERT INTO episodes (media_item_id,season_number,episode_number,title,monitored) VALUES (1,1,1,'Pilot',0)`,
	} {
		if _, err := db.W.ExecContext(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetMediaItemFull(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Monitor != "all" || got.Monitored || got.Seasons[0].Monitored || got.Seasons[0].Episodes[0].Monitored {
		t.Fatalf("migration changed existing choices: %+v", got)
	}
}
