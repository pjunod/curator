package sqlite

import (
	"context"
	"fmt"
	"testing"

	"github.com/pressly/goose/v3"
)

func TestLegacyInflightImportUsesOriginalCutoverEvidenceOnly(t *testing.T) {
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
	if err := goose.UpToContext(ctx, db.W, "migrations", 39); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `INSERT INTO media_items(id,kind,title,sort_title,quality_profile_id,added_at,updated_at) VALUES(1,'movie','Evidence','evidence',1,0,0)`); err != nil {
		t.Fatal(err)
	}
	// A deliberate fixed original cutover avoids dependence on test wall clocks.
	const cutoff = int64(1700000000000)
	traces := []string{
		fmt.Sprintf(`[{"step":"grabbed","at":%d},{"step":"downloading","at":%d},{"step":"downloaded","at":%d}]`, cutoff-2000, cutoff-1500, cutoff+2000),
		fmt.Sprintf(`[{"step":"grabbed","at":%d},{"step":"downloaded","at":%d}]`, cutoff-3000, cutoff-2000),
		fmt.Sprintf(`[{"step":"grabbed","at":%d},{"step":"failed","at":%d}]`, cutoff-3000, cutoff-2000),
		`[]`,
		fmt.Sprintf(`[{"step":"grabbed","at":%d},{"step":"downloaded","at":%d}]`, cutoff-3000, cutoff+500),
	}
	for i, trace := range traces {
		if _, err := db.W.ExecContext(ctx, `INSERT INTO downloads(id,media_item_id,release_title,protocol,state,handoff_log,added_at,updated_at) VALUES(?,1,?,'usenet','failed',?,0,0)`, i+1, fmt.Sprintf("case-%d", i), trace); err != nil {
			t.Fatal(err)
		}
	}
	if err := goose.UpToContext(ctx, db.W, "migrations", 40); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE goose_db_version SET tstamp='2023-11-14 22:13:20' WHERE version_id=40`); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for i := range traces {
		dl, err := db.GetDownload(ctx, int64(i+1))
		if err != nil {
			t.Fatal(err)
		}
		held, err := db.LegacyAutomaticImportReview(ctx, dl.GenerationKey, "/completed/newly-finished")
		if err != nil || held != (i != 0) {
			t.Fatalf("case %d held=%v err=%v", i, held, err)
		}
		cleanupHeld, err := db.LegacyCleanupReview(ctx, dl.GenerationKey, "/completed/newly-finished")
		if err != nil || !cleanupHeld {
			t.Fatalf("case %d gained old cleanup authority: %v %v", i, cleanupHeld, err)
		}
	}
	var generation string
	if err := db.R.QueryRowContext(ctx, `SELECT generation_key FROM legacy_inflight_import_evidence`).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE legacy_inflight_import_evidence SET evidence='changed' WHERE generation_key=?`, generation); err == nil {
		t.Fatal("cutover evidence was mutable")
	}
	// Editing a currently failed row into downloading cannot enroll old backlog.
	if _, err := db.W.ExecContext(ctx, `UPDATE downloads SET state='downloading',handoff_log=? WHERE id=2`, traces[0]); err != nil {
		t.Fatal(err)
	}
	dl, err := db.GetDownload(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	held, err := db.LegacyAutomaticImportReview(ctx, dl.GenerationKey, "")
	if err != nil || !held {
		t.Fatalf("mutable current state changed cohort: %v %v", held, err)
	}
}
