package sqlite

import (
	"context"
	"testing"
	"time"

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

// An import accepted by an older binary carries only the library-wide
// revision; it must be previewed again, never run unleased.
func TestLegacyRecoveryRevisionIsNeverCurrent(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	item, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Legacy", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.W.ExecContext(ctx, `INSERT INTO recovery_imports(id,state,data,updated_at) VALUES('legacy','importing',json_object('request',json_object('media_item_id',?),'accepted_revision',(SELECT revision FROM lifecycle_library_revision WHERE id=1)),0)`, item); err != nil {
		t.Fatal(err)
	}
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err = checkRecoveryRevision(ctx, tx, "legacy"); err == nil {
		t.Fatal("a library-wide revision from an older binary was accepted as a title lease")
	}
}
