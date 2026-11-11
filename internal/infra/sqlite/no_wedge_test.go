package sqlite

import (
	"context"
	"testing"
	"time"
)

// A publication has no deadline until its first import starts; the deadline
// is then six hours from that start and a retry never resets it; only
// attempts that failed on the payload are counted.
func TestPublicationDeadlineStartsOnceAndAttemptsCount(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id, err := db.InsertPublication(ctx, PublicationWrite{Kind: "salvage", Path: "/completed/p", Generation: "g"})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := db.GetPublication(ctx, id)
	if !p.DeadlineAt.IsZero() {
		t.Fatalf("deadline at discovery = %v, want none until the import starts", p.DeadlineAt)
	}
	if over, _ := db.OverduePublications(ctx, time.Now().Add(100*time.Hour)); len(over) != 0 {
		t.Fatal("a never-started publication is overdue")
	}
	if n, err := db.StartPublicationImport(ctx, id); err != nil || n != 0 {
		t.Fatalf("first start: failed attempts %d err %v", n, err)
	}
	first, _ := db.GetPublication(ctx, id)
	if first.DeadlineAt.Before(time.Now().Add(5*time.Hour)) || first.State != "importing" {
		t.Fatalf("after start: %s deadline %v", first.State, first.DeadlineAt)
	}
	if err := db.RequeueFailedPublicationAttempt(ctx, id, "boom"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	n, err := db.StartPublicationImport(ctx, id)
	if err != nil || n != 1 {
		t.Fatalf("second start: failed attempts %d err %v, want 1", n, err)
	}
	second, _ := db.GetPublication(ctx, id)
	if !second.DeadlineAt.Equal(first.DeadlineAt) {
		t.Fatalf("a retry moved the deadline: %v -> %v", first.DeadlineAt, second.DeadlineAt)
	}
}

// Ending a completed publication for a regrab writes the replacement grab
// with it; a publication that already moved on is left alone.
func TestEndPublicationForRegrabWritesTheGrab(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if _, err := db.W.ExecContext(ctx, `INSERT INTO media_items(id,kind,title,sort_title,added_at,updated_at) VALUES(1,'movie','M','m',0,0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `INSERT INTO downloads(id,media_item_id,protocol,release_title,state,added_at,updated_at,wantables) VALUES(1,1,'usenet','r','importing',1,1,'["movie:1"]')`); err != nil {
		t.Fatal(err)
	}
	id, err := db.InsertPublication(ctx, PublicationWrite{DownloadID: 1, Kind: "completed", Path: "/completed/m", Generation: "g"})
	if err != nil {
		t.Fatal(err)
	}
	dl, _ := db.GetDownload(ctx, 1)
	if ok, err := db.EndPublicationForRegrab(ctx, id, "stalled", dl); err != nil || !ok {
		t.Fatalf("end = %v %v", ok, err)
	}
	p, _ := db.GetPublication(ctx, id)
	var grabs int
	_ = db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM replacement_grabs WHERE download_id=1 AND wantable_ids='["movie:1"]'`).Scan(&grabs)
	if p.State != "import_failed" || p.Error != "stalled" || grabs != 1 {
		t.Fatalf("publication %s %q grabs %d", p.State, p.Error, grabs)
	}
	if ok, _ := db.EndPublicationForRegrab(ctx, id, "again", dl); ok {
		t.Fatal("an ended publication was ended twice")
	}
}

// The subtitles Curator placed beside a video are found by path prefix,
// whatever characters the title has.
func TestPlacedLibraryFilesWithPrefixMatchesNonASCII(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id, err := db.InsertPublication(ctx, PublicationWrite{Kind: "salvage", Path: "/completed/a", Generation: "g"})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []struct{ path, lib, outcome string }{
		{"a.srt", "/lib/Amélie (2001)/Amélie.en.srt", "placed"},
		{"b.srt", "/lib/Amélie (2001)/Amélie.fr.srt", "placed"},
		{"c.srt", "/lib/Amélie (2001)/Amélie 2.en.srt", "placed"},
		{"d.srt", "/lib/Amélie (2001)/Amélie.de.srt", "discarded"},
	} {
		if _, err := db.W.ExecContext(ctx, `INSERT INTO publication_files(publication_id,path,bytes,sha256,outcome,library_path,updated_at) VALUES(?,?,1,'d',?,?,1)`, id, f.path, f.outcome, f.lib); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := db.PlacedLibraryFilesWithPrefix(ctx, "/lib/Amélie (2001)/Amélie.")
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows = %+v (%v), want the two placed subtitles of that video", rows, err)
	}
}
