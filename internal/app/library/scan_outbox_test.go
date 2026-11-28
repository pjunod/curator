package library

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/ports"
)

func TestScanQueuesCinemaOncePerDirectory(t *testing.T) {
	svc, db, _ := newService(t)
	ctx := context.Background()
	root, err := svc.AddRootFolder(ctx, t.TempDir(), domain.RootKindOf(domain.KindMovie))
	if err != nil {
		t.Fatal(err)
	}
	item, err := svc.Add(ctx, AddRequest{Kind: domain.KindMovie, TMDBID: 550, RootFolderID: root.ID})
	if err != nil {
		t.Fatal(err)
	}
	cinema, err := db.AddNotifier(ctx, ports.NotifierConfig{Type: "plurx", Name: "cinema", Enabled: true, OnImport: true})
	if err != nil {
		t.Fatal(err)
	}
	optedOut, err := db.AddNotifier(ctx, ports.NotifierConfig{Type: "plurx", Name: "no imports", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	webhook, err := db.AddNotifier(ctx, ports.NotifierConfig{Type: "webhook", Name: "webhook", Enabled: true, OnImport: true})
	if err != nil {
		t.Fatal(err)
	}
	secondDir := filepath.Join(item.Path, "another copy")
	if err := os.MkdirAll(secondDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(item.Path, "title_t00.mkv"),
		filepath.Join(item.Path, "title_t01.mkv"),
		filepath.Join(secondDir, "title_t02.mkv"),
	} {
		if err := os.WriteFile(path, []byte("fixture"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []int{3, 0} {
		report, err := svc.Scan(ctx)
		if err != nil || report.FilesLinked != want {
			t.Fatalf("scan linked %d, want %d, err %v", report.FilesLinked, want, err)
		}
	}
	rows, err := db.ListDeliveries(ctx, cinema, 10)
	if err != nil || len(rows) != 2 {
		t.Fatalf("want two Cinema deliveries across both scans, got %v, err %v", rows, err)
	}
	if rows[0].IdempotencyKey == rows[1].IdempotencyKey {
		t.Fatal("different directories received the same request key")
	}
	counts := map[string]int{item.Path: 2, secondDir: 1}
	for _, row := range rows {
		var notice ports.Notification
		if err := json.Unmarshal([]byte(row.Payload), &notice); err != nil {
			t.Fatal(err)
		}
		if row.MediaItemID != item.ID || row.DownloadID != 0 || row.Event != "scan" ||
			notice.Import == nil || notice.Import.TmdbID != 550 || notice.Import.Kind != "movie" ||
			len(notice.Import.Dirs) != 1 || notice.Import.Dirs[0] != row.Directory ||
			len(row.ExpectedFiles) != counts[row.Directory] || len(notice.Import.Paths) != counts[row.Directory] {
			t.Fatalf("incorrect Cinema scan request: %+v, %+v", row, notice.Import)
		}
		for _, file := range row.ExpectedFiles {
			info, err := os.Stat(file.Path)
			if err != nil {
				t.Fatal(err)
			}
			if file.Bytes != info.Size() || file.MtimeNs != info.ModTime().UnixNano() {
				t.Fatalf("expected file facts differ from disk: %+v", file)
			}
		}
	}
	for _, id := range []int64{optedOut, webhook} {
		rows, err := db.ListDeliveries(ctx, id, 10)
		if err != nil || len(rows) != 0 {
			t.Fatalf("unexpected scan deliveries to notifier %d: %v, err %v", id, rows, err)
		}
	}
}

func TestAdoptionCanRetryAfterCinemaOutboxWriteFails(t *testing.T) {
	svc, db, _ := newService(t)
	ctx := context.Background()
	root, err := svc.AddRootFolder(ctx, t.TempDir(), domain.RootKindOf(domain.KindMovie))
	if err != nil {
		t.Fatal(err)
	}
	folder := filepath.Join(root.Path, "Fight Club (1999)")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(folder, "title_t00.mkv")
	if err := os.WriteFile(path, []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	cinema, err := db.AddNotifier(ctx, ports.NotifierConfig{Type: "plurx", Name: "cinema", Enabled: true, OnImport: true})
	if err != nil {
		t.Fatal(err)
	}
	const reject = "CREATE TRIGGER reject_scan_delivery BEFORE INSERT ON notifier_deliveries WHEN NEW.event = 'scan' BEGIN SELECT RAISE(ABORT, 'test outbox failure'); END"
	if _, err := db.W.ExecContext(ctx, reject); err != nil {
		t.Fatal(err)
	}
	pick := movie(550, "Fight Club", 1999)
	if err := svc.AdoptOne(ctx, folder, pick, false); err == nil {
		t.Fatal("adoption reported success after the delivery write failed")
	}
	items, err := db.ListMediaItems(ctx, "")
	if err != nil || len(items) != 1 || items[0].Path != folder {
		t.Fatalf("adoption did not preserve the binding for retry: %v, err %v", items, err)
	}
	id := items[0].ID
	files, err := db.ListFilesForItem(ctx, id)
	if err != nil || len(files) != 0 {
		t.Fatalf("file links survived a failed outbox insert: %v, err %v", files, err)
	}
	if _, err := db.W.ExecContext(ctx, "DROP TRIGGER reject_scan_delivery"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := svc.AdoptOne(ctx, folder, pick, false); err != nil {
			t.Fatal(err)
		}
	}
	files, err = db.ListFilesForItem(ctx, id)
	if err != nil || len(files) != 1 || files[0].Path != path {
		t.Fatalf("retry did not link the adopted file: %v, err %v", files, err)
	}
	rows, err := db.ListDeliveries(ctx, cinema, 10)
	if err != nil || len(rows) != 1 || rows[0].Directory != folder || rows[0].MediaItemID != id {
		t.Fatalf("want one Cinema delivery after retry, got %v, err %v", rows, err)
	}
}
