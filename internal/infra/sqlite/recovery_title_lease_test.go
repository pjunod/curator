package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/pjunod/monarr/internal/domain"
)

// A recovery's lease covers its own title and the settings every placement
// reads, nothing else: another title importing does not void it.
func TestRecoveryLeaseIsScopedToItsTitle(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	mine, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindSeries, Title: "Recovered show", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	other, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindSeries, Title: "Busy show", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) int64 {
		t.Helper()
		res, err := db.W.ExecContext(ctx, q, args...)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	now := time.Now().UnixMilli()
	myEpisode := exec(`INSERT INTO episodes(media_item_id,season_number,episode_number) VALUES(?,1,1)`, mine)
	otherEpisode := exec(`INSERT INTO episodes(media_item_id,season_number,episode_number) VALUES(?,1,1)`, other)
	otherFile := exec(`INSERT INTO media_files(media_item_id,path,size,added_at) VALUES(?,?,?,?)`, other, "/library/busy/e01.mkv", 1, now)

	const id = "title-lease"
	exec(`INSERT INTO recovery_imports(id,state,data,updated_at) VALUES(?,'importing',?,0)`, id, leasedRecoveryData(t, db, mine))
	current := func() bool {
		t.Helper()
		tx, err := db.W.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		return checkRecoveryRevision(ctx, tx, id) == nil
	}
	if !current() {
		t.Fatal("a freshly accepted lease is not current")
	}

	// Everything another title does leaves the lease alone.
	exec(`UPDATE media_items SET title='Busy show (2026)' WHERE id=?`, other)
	exec(`INSERT INTO media_files(media_item_id,path,size,added_at) VALUES(?,?,?,?)`, other, "/library/busy/e02.mkv", 1, now)
	exec(`INSERT INTO media_file_episodes(media_file_id,episode_id) VALUES(?,?)`, otherFile, otherEpisode)
	exec(`UPDATE episodes SET title='Pilot' WHERE id=?`, otherEpisode)
	exec(`DELETE FROM media_file_episodes WHERE media_file_id=?`, otherFile)
	exec(`DELETE FROM media_files WHERE id=?`, otherFile)
	if _, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "New title", Path: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if !current() {
		t.Fatal("another title's changes voided this title's recovery lease")
	}

	steps := []struct {
		name  string
		apply func()
	}{
		{"its own metadata", func() { exec(`UPDATE media_items SET title='Recovered show (2026)' WHERE id=?`, mine) }},
		{"its own episode", func() { exec(`UPDATE episodes SET title='Pilot' WHERE id=?`, myEpisode) }},
		{"a file placed into it", func() {
			exec(`INSERT INTO media_files(media_item_id,path,size,added_at) VALUES(?,?,?,?)`, mine, "/library/recovered/e01.mkv", 1, now)
		}},
		{"a file of another title linked to its episode", func() {
			f := exec(`INSERT INTO media_files(media_item_id,path,size,added_at) VALUES(?,?,?,?)`, other, "/library/busy/e03.mkv", 1, now)
			acceptCurrentRecoveryLease(t, db, id)
			exec(`INSERT INTO media_file_episodes(media_file_id,episode_id) VALUES(?,?)`, f, myEpisode)
		}},
		{"a file moved in from another title", func() {
			f := exec(`INSERT INTO media_files(media_item_id,path,size,added_at) VALUES(?,?,?,?)`, other, "/library/busy/e04.mkv", 1, now)
			acceptCurrentRecoveryLease(t, db, id)
			exec(`UPDATE media_files SET media_item_id=? WHERE id=?`, mine, f)
		}},
		{"a quality profile", func() {
			exec(`UPDATE quality_profiles SET name=name||'' WHERE id=(SELECT min(id) FROM quality_profiles)`)
		}},
		{"a root folder", func() { exec(`INSERT INTO root_folders(path,added_at) VALUES(?,?)`, t.TempDir(), now) }},
		{"its own copy", func() {
			exec(`INSERT INTO media_copies(media_item_id,name,quality_profile_id,added_at) VALUES(?,'4K',1,?)`, mine, now)
		}},
		{"its own season", func() { exec(`INSERT INTO seasons(media_item_id,number) VALUES(?,2)`, mine) }},
		{"its own episode being deleted", func() {
			ep := exec(`INSERT INTO episodes(media_item_id,season_number,episode_number) VALUES(?,9,9)`, mine)
			acceptCurrentRecoveryLease(t, db, id)
			exec(`DELETE FROM episodes WHERE id=?`, ep)
		}},
		{"its own file linked to another title's episode", func() {
			f := exec(`INSERT INTO media_files(media_item_id,path,size,added_at) VALUES(?,?,?,?)`, mine, "/library/recovered/odd.mkv", 1, now)
			acceptCurrentRecoveryLease(t, db, id)
			exec(`INSERT INTO media_file_episodes(media_file_id,episode_id) VALUES(?,?)`, f, otherEpisode)
		}},
		{"a custom format", func() { exec(`INSERT INTO custom_formats(name,pattern,score) VALUES('x','x',1)`) }},
		{"a download client's path mappings", func() {
			exec(`INSERT INTO download_clients(type,name,url,added_at) VALUES('nzbd','runner','http://runner',?)`, now)
		}},
		{"the title being deleted", func() { exec(`DELETE FROM media_items WHERE id=?`, mine) }},
	}
	for _, step := range steps {
		acceptCurrentRecoveryLease(t, db, id)
		if !current() {
			t.Fatalf("%s: re-accepted lease is not current", step.name)
		}
		step.apply()
		if current() {
			t.Fatalf("a change to %s left the recovery lease current", step.name)
		}
	}
}

// A recovery an older binary accepted carries only the library-wide
// revision. The upgrade sends ones not yet past placement back to review,
// and a lease that was never accepted (zero, as such a row loads) is never
// current, even for a title untouched since the upgrade.
func TestLegacyRecoveriesNeedAFreshPreviewAfterUpgrade(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	item, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Legacy", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.DownToContext(ctx, db.W, "migrations", 69); err != nil {
		t.Fatal(err)
	}
	for _, row := range [][2]string{{"legacy-queued", "queued"}, {"legacy-importing", "importing"}, {"legacy-receipt", "receipt_pending"}} {
		if _, err := db.W.ExecContext(ctx, `INSERT INTO recovery_imports(id,state,data,updated_at) VALUES(?,?,json_object('request',json_object('media_item_id',?),'accepted_revision',(SELECT revision FROM lifecycle_library_revision WHERE id=1)),0)`, row[0], row[1], item); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"legacy-queued": "review", "legacy-importing": "review", "legacy-receipt": "receipt_pending"}
	for id, state := range want {
		var got, message string
		if err := db.R.QueryRowContext(ctx, `SELECT state,IFNULL(json_extract(data,'$.error'),'') FROM recovery_imports WHERE id=?`, id).Scan(&got, &message); err != nil {
			t.Fatal(err)
		}
		if got != state || (state == "review" && message == "") {
			t.Fatalf("%s after upgrade: state=%s error=%q, want %s", id, got, message, state)
		}
	}
	// Loaded into the new struct and written back, a legacy row's lease is
	// zero. The shared revision starts above zero, so that is never current.
	if _, err := db.W.ExecContext(ctx, `UPDATE recovery_imports SET state='importing',data=json_set(data,'$.accepted_lease',json_object('item',0,'shared',0)) WHERE id='legacy-queued'`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err = checkRecoveryRevision(ctx, tx, "legacy-queued"); err == nil {
		t.Fatal("a zero lease was accepted as current")
	}
}

// The lease covers one title; a file row at the destination that belongs to
// anything else is outside it, and committing would hand it to this title.
func TestRecoveryPlacementRefusesADestinationOwnedElsewhere(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	mine, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Recovered", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	other, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Neighbour", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	const target = "/library/shared/name.mkv"
	if _, err := db.W.ExecContext(ctx, `INSERT INTO media_files(media_item_id,path,size,added_at) VALUES(?,?,?,?)`, other, target, 1, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `INSERT INTO recovery_imports(id,state,data,updated_at) VALUES('foreign','importing',?,0)`, leasedRecoveryData(t, db, mine)); err != nil {
		t.Fatal(err)
	}
	p := Placement{ID: "foreign-placement", ItemID: mine, Target: target, Size: 1, SHA256: "digest", RecoveryImport: "foreign", RecoveryFile: "file", State: "prepared"}
	if err := db.PreparePlacement(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CommitPlacement(ctx, p); err == nil {
		t.Fatal("a recovery placement took over another title's file row")
	}
	var owner int64
	if err := db.R.QueryRowContext(ctx, `SELECT media_item_id FROM media_files WHERE path=?`, target).Scan(&owner); err != nil || owner != other {
		t.Fatalf("file row owner=%d err=%v, want %d", owner, err, other)
	}
}
