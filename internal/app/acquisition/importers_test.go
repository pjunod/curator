package acquisition

import (
	"context"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/app/transfers"
)

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
