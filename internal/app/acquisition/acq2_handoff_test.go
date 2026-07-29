package acquisition

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/monarr-media/monarr/internal/app/transfers"
	"github.com/monarr-media/monarr/internal/infra/sqlite"
	"github.com/monarr-media/monarr/internal/ports"
)

// stubbornClient will not delete anything. A client that is down, or a job it
// no longer recognises, must not be surfaced as a failure — the import it
// follows already succeeded.
type stubbornClient struct{ fakeClient }

func (c *stubbornClient) Remove(context.Context, ports.Handle, bool) error {
	return errors.New("no such job")
}

// ImportNow is the button behind "approve this" and "retry that". Each of its
// refusals is a distinct message the user acts on, so each has to be a distinct
// answer rather than a generic failure.
func TestAcq2ImportNowRefusesWhatItCannotImport(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setup(t, nil, client)
	ctx := context.Background()

	if err := svc.ImportNow(ctx, 31337); err == nil {
		t.Error("imported a download that does not exist")
	}

	// Grabbed, but the client has not said where the payload is yet.
	id, err := svc.Grab(ctx, GrabRequest{
		MediaItemID: itemID, Season: 1, Episode: 1,
		Title: "Test.Show.S01E01.1080p.WEB-DL-NOPATH", DownloadURL: "m",
		Indexer: "idx", Protocol: "torrent",
	})
	if err != nil {
		t.Fatal(err)
	}
	err = svc.ImportNow(ctx, id)
	if err == nil || !strings.Contains(err.Error(), "no download path recorded") {
		t.Errorf("err = %v, want the missing path named", err)
	}

	// Already imported is a no-op, not an error and not a second import.
	if err := db.UpdateDownloadState(ctx, id, "imported", 1, ""); err != nil {
		t.Fatal(err)
	}
	if err := svc.ImportNow(ctx, id); err != nil {
		t.Errorf("re-importing a finished download = %v, want a quiet no-op", err)
	}
}

// runImport falls back to the raw save path when no mapped import path was
// recorded. Without the fallback, any client whose completion arrived before
// path mapping existed would be unimportable.
func TestAcq2ImportNowFallsBackToTheRawSavePath(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setup(t, nil, client)
	ctx := context.Background()
	release := "Test.Show.S01E01.1080p.WEB-DL-RAW"

	payload := t.TempDir()
	if err := os.WriteFile(filepath.Join(payload, release+".mkv"), []byte("v"), 0o644); err != nil {
		t.Fatal(err)
	}
	clients, _ := db.ListDownloadClients(ctx)
	id, err := db.InsertDownload(ctx, sqlite.Download{
		MediaItemID: itemID, WantableIDs: []string{"episode:" + itoa(itemID) + ":1:1"},
		ReleaseTitle: release, Indexer: "idx", Protocol: "torrent",
		ClientID: clients[0].ID, Handle: "h1", State: "downloaded",
	})
	if err != nil {
		t.Fatal(err)
	}
	dl, _ := db.GetDownload(ctx, id)
	dl.SavePath = payload // …and no ImportPath at all

	if err := svc.runImport(ctx, dl); err != nil {
		t.Fatalf("import from the raw save path failed: %v", err)
	}
	got, _ := db.GetDownload(ctx, id)
	if got.State != "imported" {
		t.Errorf("state = %q (%s)", got.State, got.Error)
	}
}

// A partial import is not a failure, but the user has to be able to see what
// was left behind without reading the server log — which is why the skipped
// count and reasons go onto the trace itself.
func TestAcq2APartialImportPutsTheSkippedFilesOnTheTrace(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setup(t, nil, client)
	ctx := context.Background()
	release := "Test.Show.S01.1080p.WEB-DL-PARTIAL"

	payload := t.TempDir()
	if err := os.WriteFile(filepath.Join(payload, "Test.Show.S01E01.1080p.mkv"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	// An episode this show does not have: importable-looking, unplaceable.
	if err := os.WriteFile(filepath.Join(payload, "Test.Show.S01E09.1080p.mkv"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	clients, _ := db.ListDownloadClients(ctx)
	id, err := db.InsertDownload(ctx, sqlite.Download{
		MediaItemID: itemID, WantableIDs: []string{"season:" + itoa(itemID) + ":1"},
		ReleaseTitle: release, Indexer: "idx", Protocol: "torrent",
		ClientID: clients[0].ID, Handle: "h1", State: "downloaded",
	})
	if err != nil {
		t.Fatal(err)
	}
	dl, _ := db.GetDownload(ctx, id)
	dl.ImportPath = payload

	if err := svc.runImport(ctx, dl); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetDownload(ctx, id)
	var detail string
	for _, h := range got.Handoff {
		if h.Step == stepImported {
			detail = h.Detail
		}
	}
	if !strings.Contains(detail, "skipped") {
		t.Errorf("the trace does not mention the skipped file: %q", detail)
	}
	if !strings.Contains(detail, "S01E09") {
		t.Errorf("the trace does not name what was skipped: %q", detail)
	}
}

// Blocklist & replace is the explicit escalation: throw the payload away,
// declare the release bad, and go and find another. A blocklist that does not
// blocklist is the one outcome this button must not have — so it is never
// treated as blameless, however the download ended.
func TestAcq2BlocklistReplaceRemovesBansAndResearches(t *testing.T) {
	client := &fakeClient{}
	svc, db, movieID := autoSetup(t, []ports.Release{
		rel("Test.Movie.2024.1080p.WEB-DL.x264-FIRST", 50),
		rel("Test.Movie.2024.1080p.WEB-DL.x264-SECOND", 10),
	}, client)
	ctx := context.Background()

	if err := svc.SyncRSS(ctx); err != nil {
		t.Fatal(err)
	}
	active, _ := db.ListActiveDownloads(ctx)
	if len(active) != 1 {
		t.Fatalf("setup grab = %+v", active)
	}
	first := active[0]

	if err := svc.BlocklistReplace(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if len(client.removed) != 1 || !client.removed[0].DeleteData {
		t.Errorf("the payload was not thrown away: %+v", client.removed)
	}
	if blocked, _ := db.IsBlocklisted(ctx, first.ReleaseTitle, "idx"); !blocked {
		t.Error("the release was not blocklisted")
	}
	if len(client.added) != 2 || !strings.HasSuffix(client.added[1], "SECOND") {
		t.Errorf("no replacement was grabbed: %v", client.added)
	}
	got, _ := db.GetDownload(ctx, first.ID)
	if got.State != "failed" || got.Error != "blocklisted by user" {
		t.Errorf("row = %q / %q", got.State, got.Error)
	}
	if err := svc.BlocklistReplace(ctx, 8675309); err == nil {
		t.Error("blocklisted a download that does not exist")
	}
	_ = movieID
}

// Trace details are read by a person. An unnamed client falls back to its type
// and a missing path says so in words, because "sent to " and "payload at "
// with nothing after them read as a bug in monarr.
func TestAcq2TraceDetailsNameTheClientAndTheMissingPath(t *testing.T) {
	if got := clientLabel(ports.ClientConfig{Type: "nzbd"}); got != "nzbd" {
		t.Errorf("unnamed client label = %q, want its type", got)
	}
	if got := clientLabel(ports.ClientConfig{Type: "nzbd", Name: "attic"}); got != "attic" {
		t.Errorf("client label = %q", got)
	}
	if got := orNone(""); got != "(no path reported)" {
		t.Errorf("orNone = %q", got)
	}
	if got := orNone("/downloads/x"); got != "/downloads/x" {
		t.Errorf("orNone = %q", got)
	}
}

// The workers are the data plane. Starting them, draining them and reporting
// what they are doing is the whole lifecycle the process depends on at
// shutdown — a worker that outlives its context is an import writing into a
// database that is closing.
func TestAcq2ImportWorkersStartReportAndDrain(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setup(t, nil, client)
	reg := transfers.New()
	svc.SetRegistry(reg)
	ctx, cancel := context.WithCancel(context.Background())

	release := "Test.Show.S01E01.1080p.WEB-DL-WORKER"
	payload := t.TempDir()
	if err := os.WriteFile(filepath.Join(payload, release+".mkv"), []byte("v"), 0o644); err != nil {
		t.Fatal(err)
	}
	clients, _ := db.ListDownloadClients(ctx)
	id, err := db.InsertDownload(ctx, sqlite.Download{
		MediaItemID: itemID, WantableIDs: []string{"episode:" + itoa(itemID) + ":1:1"},
		ReleaseTitle: release, Indexer: "idx", Protocol: "torrent",
		ClientID: clients[0].ID, Handle: "h1", State: "downloaded",
	})
	if err != nil {
		t.Fatal(err)
	}
	dl, _ := db.GetDownload(ctx, id)
	dl.ImportPath = payload

	// Nothing running yet, so nothing is in flight and nothing is stalled.
	if len(svc.Transfers()) != 0 {
		t.Errorf("in flight before anything started: %+v", svc.Transfers())
	}
	if d, ok := svc.StalledImports(); ok {
		t.Errorf("reported a stalled import of %v with no imports running", d)
	}

	svc.StartImporters(ctx)
	svc.enqueueImport(ctx, dl, clients[0])

	deadline := time.Now().Add(10 * time.Second)
	for {
		got, err := db.GetDownload(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.State == "imported" {
			break
		}
		if got.State == "failed" {
			t.Fatalf("the worker failed the import: %s", got.Error)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the worker never ran the import; state = %q", got.State)
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	done := make(chan struct{})
	go func() { defer close(done); svc.WaitImporters() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("WaitImporters did not drain after the context was cancelled")
	}
	if len(svc.Transfers()) != 0 {
		t.Errorf("a finished import is still reported in flight: %+v", svc.Transfers())
	}
}

// A full queue costs latency, not correctness: the poll is idempotent, so the
// next sweep offers the same completed download again. Dropping the enqueue is
// deliberate — an unbounded channel would trade this for unbounded memory.
func TestAcq2AFullImportQueueDefersRatherThanBlocking(t *testing.T) {
	svc, db, itemID := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	clients, _ := db.ListDownloadClients(ctx)

	// The queue, with no workers behind it, filled to the brim.
	svc.importOnce.Do(func() { svc.importCh = make(chan importJob, importQueue) })
	for i := 0; i < importQueue; i++ {
		svc.importCh <- importJob{dl: sqlite.Download{ID: int64(i)}, cfg: clients[0]}
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.enqueueImport(ctx, sqlite.Download{ID: 999, MediaItemID: itemID}, clients[0])
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("enqueueImport blocked on a full queue — the poll goroutine is stuck")
	}
	if n := len(svc.importCh); n != importQueue {
		t.Errorf("queue depth = %d, want it capped at %d", n, importQueue)
	}
}

// The in-flight view has to carry byte progress for the one stage that has a
// meaningful byte count, and nothing at all for the stages that do not — a
// percentage invented for post-processing is a guess wearing a progress bar.
func TestAcq2TheInFlightViewOnlyCountsBytesForTheFetch(t *testing.T) {
	svc, _, _ := setup(t, nil, &fakeClient{})
	reg := transfers.New()
	svc.SetRegistry(reg)
	dl := sqlite.Download{ID: 7, ReleaseTitle: "Test.Show.S01E01", Size: 1000}

	svc.watchDownloading(dl, ports.DownloadStatus{State: ports.StateDownloading, Progress: 0.5}, "")
	live, ok := reg.Stage(dl.ID)
	if !ok {
		t.Fatal("a downloading job is not in the in-flight view")
	}
	if f, known := live.Fraction(); !known || f < 0.49 || f > 0.51 {
		t.Errorf("fraction = %v known=%v, want ~0.5", f, known)
	}

	// nzbd's own post-processing stage replaces "downloading" and reports no
	// total: twenty minutes of par2 repair is not "downloading".
	svc.watchDownloading(dl, ports.DownloadStatus{State: ports.StateDownloading, Progress: 0.5}, "par_repair")
	live, _ = reg.Stage(dl.ID)
	if live.Stage != "par_repair" {
		t.Errorf("stage = %q, want the client's own stage name", live.Stage)
	}

	// Completed: nothing of this job is in flight at the client any more.
	svc.watchDownloading(dl, ports.DownloadStatus{State: ports.StateCompleted}, "")
	if _, ok := reg.Stage(dl.ID); ok {
		t.Error("a finished job is still on the in-flight view")
	}
}

// Cleanup is best-effort throughout: no handle, a client that has vanished from
// the settings, or one that refuses the delete are all things that get retried
// next sweep rather than surfaced as a failure of an import that succeeded.
func TestAcq2CleanupIsBestEffort(t *testing.T) {
	stubborn := &stubbornClient{}
	svc, db, itemID := setupUsenet(t, &stubborn.fakeClient)
	svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return stubborn }
	ctx := context.Background()
	clients, _ := db.ListDownloadClients(ctx)

	if svc.removePayload(ctx, downloadRef{ID: 1, ClientID: clients[0].ID}) {
		t.Error("claimed to remove a payload for a download with no handle")
	}
	if svc.removePayload(ctx, downloadRef{ID: 1, ClientID: 4242, Handle: "h1"}) {
		t.Error("claimed to remove a payload via a client that is not configured")
	}
	if svc.removePayload(ctx, downloadRef{
		ID: 1, MediaItemID: itemID, ClientID: clients[0].ID, Handle: "h1",
	}) {
		t.Error("claimed success when the client refused the delete")
	}

	// And a refusal leaves the row pending, so the next sweep tries again.
	dl := insertImportable(t, db, itemID, t.TempDir())
	if err := db.UpdateDownloadState(ctx, dl.ID, "imported", 1, ""); err != nil {
		t.Fatal(err)
	}
	if err := svc.CleanupPayloads(ctx); err != nil {
		t.Fatal(err)
	}
	pending, err := db.ImportedWithPayload(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Errorf("a refused delete marked the payload collected: %+v", pending)
	}
}
