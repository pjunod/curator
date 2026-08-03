package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/app/transfers"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

// A process restart leaves the durable row saying "importing" but loses the
// worker and live transfer registry. The next completed observation must
// restart that orphan; treating the word in SQLite as proof of a live worker
// strands it forever.
func TestAnOrphanedImportRecoversOnTheNextCompletedObservation(t *testing.T) {
	client := &fakeClient{}
	svc, db, _ := setup(t, nil, client)
	ctx := context.Background()

	id := grabEpisode(t, svc)
	payload := payloadDir(t)
	client.statuses = completed(payload)
	dl, err := db.GetDownload(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	dl.State = "importing"
	dl.SavePath, dl.ImportPath = payload, payload
	if err := db.UpdateDownloadHandoff(ctx, dl); err != nil {
		t.Fatal(err)
	}

	if err := svc.RefreshQueue(ctx); err != nil {
		t.Fatal(err)
	}
	dl, err = db.GetDownload(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if dl.State != "imported" {
		t.Fatalf("orphan recovery state = %q (%s)", dl.State, dl.Error)
	}
	found := false
	for _, h := range dl.Handoff {
		found = found || h.Step == stepImportRecovered
	}
	if !found {
		t.Fatalf("recovered import has no recovery step: %+v", dl.Handoff)
	}
}

// Cancel is a data-plane operation, not a row-delete shortcut: it stops the
// worker, leaves the source payload intact, and returns the row to a state the
// Restart button can act on.
func TestARunningImportCanBeCancelledAndRestartedLater(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setup(t, nil, client)
	reg := transfers.New()
	svc.SetRegistry(reg)
	workerCtx, stopWorkers := context.WithCancel(context.Background())
	svc.StartImporters(workerCtx)
	t.Cleanup(func() { stopWorkers(); svc.WaitImporters() })

	payload := t.TempDir()
	src := filepath.Join(payload, "Test.Show.S01E01.1080p.WEB-DL-CANCEL.mkv")
	if err := os.WriteFile(src, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	clients, _ := db.ListDownloadClients(context.Background())
	id, err := db.InsertDownload(context.Background(), sqlite.Download{
		MediaItemID: itemID, WantableIDs: []string{"episode:" + itoa(itemID) + ":1:1"},
		ReleaseTitle: "Test.Show.S01E01.1080p.WEB-DL-CANCEL", Indexer: "idx",
		Protocol: "torrent", ClientID: clients[0].ID, Handle: "h-cancel", State: "downloaded",
	})
	if err != nil {
		t.Fatal(err)
	}
	dl, _ := db.GetDownload(context.Background(), id)
	dl.ImportPath = payload
	if err := db.UpdateDownloadHandoff(context.Background(), dl); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	previous := placeFile
	placeFile = func(ctx context.Context, _, _ string, _ func(done, total int64)) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	t.Cleanup(func() { placeFile = previous })

	if err := svc.ImportNow(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("import worker did not start")
	}
	if err := svc.CancelImport(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	dl, err = db.GetDownload(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if dl.State != "downloaded" || dl.Error != "" {
		t.Fatalf("cancelled row = state %q error %q", dl.State, dl.Error)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("cancel removed the source payload: %v", err)
	}
}

// The poll must hand the import off and return, not perform it.
//
// This is the control/data split, asserted. Imports used to run inside
// RefreshQueue: a 20 GB copy across a network mount held the sweep open for
// seventeen minutes, and every control-plane signal that reads that loop —
// the contact clock, the task duration, the next-run time — reported a file
// copy instead of a connection. The Connections panel then degraded the
// download client it was importing FROM.
//
// The assertion is deliberately not "the sweep finished quickly". A fast
// import would satisfy that while still running on the poll goroutine, so the
// test would pass on the very code it exists to reject. Instead the workers
// are left unstarted: the queue exists, nothing drains it, and the sweep must
// still return with the job parked and the row untouched. That can only be
// true if RefreshQueue handed the work away.
func TestThePollDoesNotWaitForTheImport(t *testing.T) {
	client := &fakeClient{}
	svc, _, _ := setup(t, nil, client)
	ctx := context.Background()
	svc.SetRegistry(transfers.New())

	// The queue, with no workers behind it.
	svc.importOnce.Do(func() { svc.importCh = make(chan importJob, importQueue) })

	id := grabEpisode(t, svc)
	dir := payloadDir(t)
	client.statuses = completed(dir)

	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := svc.RefreshQueue(ctx); err != nil {
			t.Error(err)
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RefreshQueue did not return — the import is still on the poll goroutine")
	}

	if n := len(svc.importCh); n != 1 {
		t.Fatalf("queued imports = %d, want 1 — the sweep did the work itself", n)
	}
	dl, err := svc.db.GetDownload(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if dl.State == "imported" {
		t.Fatal("the download was imported with no import worker running, " +
			"so the copy happened on the poll goroutine")
	}

	// The import itself still happens, just not there.
	svc.StartImporters(ctx)
	deadline := time.Now().Add(10 * time.Second)
	for {
		dl, err := svc.db.GetDownload(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if dl.State == "imported" || dl.State == "failed" {
			if dl.State != "imported" {
				t.Fatalf("state = %q (%s), want imported", dl.State, dl.Error)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the worker never ran the import; state = %q", dl.State)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// An import in flight is visible while it is happening, not only afterwards.
//
// The failure this replaces: a 20 GB import was reported nowhere at all. Not
// on Activity beyond a state word, not on System, not in metrics. The one
// observable symptom anywhere in the process was the duration column of an
// unrelated scheduled task.
func TestAnImportIsVisibleWhileItRuns(t *testing.T) {
	client := &fakeClient{}
	svc, db, _ := setup(t, nil, client)
	ctx := context.Background()
	reg := transfers.New()
	svc.SetRegistry(reg)

	id := grabEpisode(t, svc)
	dl, err := db.GetDownload(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	dl.Size = 4096

	// Registered for the duration, and gone afterwards.
	h := reg.Begin(transfers.Transfer{
		DownloadID: dl.ID, Title: dl.ReleaseTitle,
		Stage: transfers.StageImporting, Total: dl.Size,
	})
	live := reg.Snapshot()
	if len(live) != 1 {
		t.Fatalf("in flight = %d, want 1", len(live))
	}
	if live[0].Stage != transfers.StageImporting || live[0].Title != dl.ReleaseTitle {
		t.Errorf("wrong transfer reported: %+v", live[0])
	}
	if _, known := live[0].Fraction(); !known {
		t.Error("a transfer with a known total must report a fraction")
	}

	h.Bytes(2048, 4096)
	live = reg.Snapshot()
	if f, _ := live[0].Fraction(); f < 0.49 || f > 0.51 {
		t.Errorf("fraction = %v, want ~0.5", f)
	}

	h.End()
	if n := reg.Count(""); n != 0 {
		t.Errorf("in flight after End = %d, want 0", n)
	}
}

// A stage that cannot measure itself must say so rather than claim 0%.
func TestAnUnmeasurableStageIsNotZeroPercent(t *testing.T) {
	reg := transfers.New()
	reg.Begin(transfers.Transfer{DownloadID: 1, Stage: transfers.StageNotifying})
	live := reg.Snapshot()
	if len(live) != 1 {
		t.Fatalf("in flight = %d", len(live))
	}
	if _, known := live[0].Fraction(); known {
		t.Error("a notify has no byte count and must not report a fraction — " +
			"a 0% bar reads as 'nothing has happened', which is a different claim")
	}
}
