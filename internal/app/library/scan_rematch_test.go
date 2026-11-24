package library

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/pjunod/monarr/internal/domain"
)

// A scan leaves alone the titles a Re-match is moving files for, and the
// folder it chose (ADR 0027): a file mid-move is neither pruned nor offered
// for adoption. Once the move is over the next scan picks them up.
func TestScanSkipsTitlesWithAnOpenRematch(t *testing.T) {
	svc, db, _ := newService(t)
	ctx := context.Background()
	root := t.TempDir()
	rf, _ := svc.AddRootFolder(ctx, root, domain.KindMixed)
	item, err := svc.Add(ctx, AddRequest{Kind: domain.KindMovie, TMDBID: 550, RootFolderID: rf.ID, Monitored: true})
	if err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(item.Path, 0o755)
	gone := filepath.Join(item.Path, "Fight Club (1999) [Bluray-1080p].mkv")
	if _, err := db.UpsertFile(ctx, item.ID, 0, gone, 10); err != nil {
		t.Fatal(err) // its bytes are mid-move: not on disk at this path
	}
	chosen := filepath.Join(root, "Fight Club (1999) {tmdb-551}")
	_ = os.MkdirAll(chosen, 0o755)
	if _, err := db.W.ExecContext(ctx, `INSERT INTO rematch_operations(operation_id,request_id,fingerprint,state,source_item_id,target_item_id,plan,created_at,updated_at)
		VALUES('rm-x','r','f','moving',?,?,?,1,1)`, item.ID, item.ID+1, `{"targetPath":"`+chosen+`"}`); err != nil {
		t.Fatal(err)
	}
	report, err := svc.Scan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := svc.Get(ctx, item.ID)
	if len(got.Files) != 1 {
		t.Fatalf("files = %+v, want the row of a file mid-move kept", got.Files)
	}
	for _, u := range report.UnmatchedDirs {
		if u.Path == chosen {
			t.Fatal("the folder a re-match chose was offered for adoption")
		}
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE rematch_operations SET state='complete' WHERE operation_id='rm-x'`); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ = svc.Get(ctx, item.ID); len(got.Files) != 0 {
		t.Fatalf("files = %+v, want the vanished file pruned once the move is over", got.Files)
	}
}

// A move that opens after the scan took its snapshot still wins: scanItem
// re-reads before it writes or prunes, and leaves the rows as they are.
func TestScanItemYieldsToAMoveThatStartedMidScan(t *testing.T) {
	svc, db, _ := newService(t)
	ctx := context.Background()
	root := t.TempDir()
	rf, _ := svc.AddRootFolder(ctx, root, domain.KindMixed)
	item, err := svc.Add(ctx, AddRequest{Kind: domain.KindMovie, TMDBID: 550, RootFolderID: rf.ID, Monitored: true})
	if err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(item.Path, 0o755)
	gone := filepath.Join(item.Path, "Fight Club (1999) [Bluray-1080p].mkv")
	if _, err := db.UpsertFile(ctx, item.ID, 0, gone, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `INSERT INTO rematch_operations(operation_id,request_id,fingerprint,state,source_item_id,target_item_id,plan,created_at,updated_at)
		VALUES('rm-late','r','f','planned',?,?,'{}',1,1)`, item.ID, item.ID+1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.scanItem(ctx, item, nil); !errors.Is(err, errRematchStarted) {
		t.Fatalf("scanItem err = %v, want it to yield to the move", err)
	}
	if got, _ := svc.Get(ctx, item.ID); len(got.Files) != 1 {
		t.Fatalf("files = %+v, want the row kept", got.Files)
	}
}

// A title whose folder looks missing while a move runs is not reported
// missing: the move check comes before the folder check.
func TestScanDoesNotReportAMovingTitleMissing(t *testing.T) {
	svc, db, _ := newService(t)
	ctx := context.Background()
	root := t.TempDir()
	rf, _ := svc.AddRootFolder(ctx, root, domain.KindMixed)
	item, err := svc.Add(ctx, AddRequest{Kind: domain.KindMovie, TMDBID: 550, RootFolderID: rf.ID, Monitored: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `INSERT INTO rematch_operations(operation_id,request_id,fingerprint,state,source_item_id,target_item_id,plan,created_at,updated_at)
		VALUES('rm-m','r','f','moving',?,?,'{}',1,1)`, item.ID, item.ID+1); err != nil {
		t.Fatal(err)
	}
	report, err := svc.Scan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.MissingItems) != 0 {
		t.Fatalf("missing = %+v, want a moving title left out", report.MissingItems)
	}
}
