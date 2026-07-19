package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monarr-media/monarr/internal/infra/sqlite"
	"github.com/monarr-media/monarr/internal/ports"
)

const epRelease = "Test.Show.S01E01.1080p.WEB-DL.x264-GRP"

func stepSeq(dl sqlite.Download) []string {
	out := make([]string, 0, len(dl.Handoff))
	for _, e := range dl.Handoff {
		out = append(out, e.Step)
	}
	return out
}

func hasStep(steps []string, want string) bool {
	for _, s := range steps {
		if s == want {
			return true
		}
	}
	return false
}

// payloadDir writes a single episode video under a fresh dir named for the
// release, returning the dir (what a client reports as its save path).
func payloadDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), epRelease)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, epRelease+".mkv"), []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func completed(save string) []ports.DownloadStatus {
	return []ports.DownloadStatus{{Handle: "h1", Name: epRelease, State: ports.StateCompleted, Progress: 1, SavePath: save}}
}

func grabEpisode(t *testing.T, svc *Service) int64 {
	t.Helper()
	id, err := svc.Grab(context.Background(), GrabRequest{
		MediaItemID: 1, Season: 1, Episode: 1, Title: epRelease,
		DownloadURL: "magnet:x", Indexer: "idx", Protocol: "torrent",
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// The handoff is laid out step by step and the paths are captured.
func TestHandoffTraceRecorded(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setup(t, nil, client)
	ctx := context.Background()
	_ = itemID

	id := grabEpisode(t, svc)

	dl, _ := db.GetDownload(ctx, id)
	if len(dl.Handoff) == 0 || dl.Handoff[0].Step != stepGrabbed {
		t.Fatalf("grab should open the trace, got %+v", dl.Handoff)
	}

	client.statuses = []ports.DownloadStatus{{Handle: "h1", Name: epRelease, State: ports.StateDownloading, Progress: 0.5}}
	if err := svc.RefreshQueue(ctx); err != nil {
		t.Fatal(err)
	}
	if dl, _ = db.GetDownload(ctx, id); dl.State != "downloading" {
		t.Fatalf("state = %s, want downloading", dl.State)
	}

	save := payloadDir(t)
	client.statuses = completed(save)
	if err := svc.RefreshQueue(ctx); err != nil {
		t.Fatal(err)
	}

	dl, _ = db.GetDownload(ctx, id)
	if dl.State != "imported" {
		t.Fatalf("state = %s (%s)", dl.State, dl.Error)
	}
	if dl.SavePath != save || dl.ImportPath != save {
		t.Errorf("paths not captured: save=%q import=%q", dl.SavePath, dl.ImportPath)
	}
	steps := stepSeq(dl)
	for _, want := range []string{stepGrabbed, stepDownloading, stepDownloaded, stepImporting, stepImported} {
		if !hasStep(steps, want) {
			t.Errorf("missing %q step in trace %v", want, steps)
		}
	}
}

// Manual approval parks a completed download; ImportNow releases it.
func TestManualApprovalHoldsThenImports(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setup(t, nil, client)
	ctx := context.Background()

	cfg, _ := db.GetDownloadClient(ctx, 1)
	cfg.ManualApproval = true
	if err := db.UpdateDownloadClient(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	id := grabEpisode(t, svc)
	client.statuses = completed(payloadDir(t))

	if err := svc.RefreshQueue(ctx); err != nil {
		t.Fatal(err)
	}
	dl, _ := db.GetDownload(ctx, id)
	if dl.State != "awaiting_import" {
		t.Fatalf("state = %s, want awaiting_import", dl.State)
	}
	if item, _ := db.GetMediaItemFull(ctx, itemID); len(item.Files) != 0 {
		t.Fatalf("must not import while held: %+v", item.Files)
	}

	// A second poll keeps it held without re-logging the hold.
	if err := svc.RefreshQueue(ctx); err != nil {
		t.Fatal(err)
	}
	dl, _ = db.GetDownload(ctx, id)
	held := 0
	for _, e := range dl.Handoff {
		if e.Step == stepAwaiting {
			held++
		}
	}
	if held != 1 {
		t.Errorf("awaiting logged %d times, want 1", held)
	}

	if err := svc.ImportNow(ctx, id); err != nil {
		t.Fatal(err)
	}
	if dl, _ = db.GetDownload(ctx, id); dl.State != "imported" {
		t.Fatalf("after approval state = %s (%s)", dl.State, dl.Error)
	}
	if item, _ := db.GetMediaItemFull(ctx, itemID); len(item.Files) != 1 {
		t.Errorf("expected import after approval: %+v", item.Files)
	}
}

// A completed download Monarr can't place surfaces as failed WITHOUT being
// blocklisted — the release is fine, the placement isn't.
func TestImportFailureSurfacesWithoutBlocklist(t *testing.T) {
	client := &fakeClient{}
	svc, db, _ := setup(t, nil, client)
	ctx := context.Background()

	id := grabEpisode(t, svc)
	client.statuses = completed(t.TempDir()) // empty dir: no media

	if err := svc.RefreshQueue(ctx); err != nil {
		t.Fatal(err)
	}
	dl, _ := db.GetDownload(ctx, id)
	if dl.State != "failed" {
		t.Fatalf("state = %s, want failed", dl.State)
	}
	if !strings.Contains(dl.Error, "no media files") {
		t.Errorf("error = %q, want a 'no media files' reason", dl.Error)
	}
	if blocked, _ := db.IsBlocklisted(ctx, epRelease, "idx"); blocked {
		t.Error("an import failure must not blocklist the release")
	}
	if steps := stepSeq(dl); steps[len(steps)-1] != stepFailed {
		t.Errorf("trace should end at failed: %v", steps)
	}

	// The failed row is not re-polled (it left the active set), so it waits
	// for the user rather than churning replacements.
	if err := svc.RefreshQueue(ctx); err != nil {
		t.Fatal(err)
	}
	if dl, _ = db.GetDownload(ctx, id); dl.State != "failed" {
		t.Errorf("failed import should stay put, got %s", dl.State)
	}
}

// Remote path mapping is applied and recorded: the client reports its own
// path, Monarr imports from the mapped local path.
func TestRemotePathMappingRecorded(t *testing.T) {
	client := &fakeClient{}
	svc, db, _ := setup(t, nil, client)
	ctx := context.Background()

	localBase := t.TempDir()
	cfg, _ := db.GetDownloadClient(ctx, 1)
	cfg.PathMappings = []ports.PathMapping{{Remote: "/remote/dl", Local: localBase}}
	if err := db.UpdateDownloadClient(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	id := grabEpisode(t, svc)
	localDir := filepath.Join(localBase, epRelease)
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localDir, epRelease+".mkv"), []byte("v"), 0o644); err != nil {
		t.Fatal(err)
	}
	client.statuses = completed("/remote/dl/" + epRelease)

	if err := svc.RefreshQueue(ctx); err != nil {
		t.Fatal(err)
	}
	dl, _ := db.GetDownload(ctx, id)
	if dl.State != "imported" {
		t.Fatalf("state = %s (%s)", dl.State, dl.Error)
	}
	if dl.SavePath != "/remote/dl/"+epRelease {
		t.Errorf("savePath = %q, want the reported remote path", dl.SavePath)
	}
	if !strings.HasPrefix(dl.ImportPath, localBase) || dl.ImportPath == dl.SavePath {
		t.Errorf("importPath = %q, want it mapped under %q", dl.ImportPath, localBase)
	}
}

// Manual import scans a loose folder (skipping samples) and imports it into
// a chosen item, with no download row involved.
func TestManualImportLooseFiles(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setup(t, nil, client)
	ctx := context.Background()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, epRelease+".mkv"), []byte("v"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sample.mkv"), []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}

	scanned, err := svc.ScanImportPath(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(scanned) != 1 {
		t.Fatalf("scan found %d, samples should be skipped: %+v", len(scanned), scanned)
	}
	if scanned[0].Season != 1 || len(scanned[0].Episodes) != 1 || scanned[0].Episodes[0] != 1 {
		t.Errorf("scan parse wrong: %+v", scanned[0])
	}

	n, err := svc.ManualImport(ctx, ManualImportRequest{Path: dir, MediaItemID: itemID})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("imported %d files, want 1", n)
	}
	if item, _ := db.GetMediaItemFull(ctx, itemID); len(item.Files) != 1 {
		t.Errorf("expected 1 imported file: %+v", item.Files)
	}
}

// Updating a client persists the manual-approval toggle and other fields.
func TestUpdateClientTogglesManualApproval(t *testing.T) {
	client := &fakeClient{}
	_, db, _ := setup(t, nil, client)
	ctx := context.Background()

	cfg, err := db.GetDownloadClient(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ManualApproval {
		t.Fatal("manual approval should default off")
	}
	cfg.ManualApproval = true
	cfg.Category = "changed"
	if err := db.UpdateDownloadClient(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetDownloadClient(ctx, 1)
	if !got.ManualApproval {
		t.Error("manual approval not persisted")
	}
	if got.Category != "changed" {
		t.Error("category not updated")
	}
}
