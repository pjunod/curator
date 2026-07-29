package transfers

import (
	"context"
	"testing"
	"time"
)

// Ordering exists so a status page can sort rows into pipeline order. The
// interesting case is a stage this build has never heard of: it sorts LAST,
// because a name we do not recognise is far more likely to be a newer stage
// nzbd grew than an older one we forgot — and sorting it first would put the
// unknown row above the download that is actually running.
func TestTailOrderPutsUnknownStagesLast(t *testing.T) {
	if Order(StageDownloading) != 0 {
		t.Errorf("downloading is not first: %d", Order(StageDownloading))
	}
	if Order(StageUnpack) <= Order(StageParVerify) {
		t.Errorf("unpack (%d) must sort after par verify (%d)",
			Order(StageUnpack), Order(StageParVerify))
	}
	if Order(StageImporting) <= Order(StageScript) {
		t.Errorf("importing (%d) must sort after the client's own stages (%d)",
			Order(StageImporting), Order(StageScript))
	}
	// Notifying is not in the exclusive pipeline but still has a home, just
	// past the end of it.
	if got, want := Order(StageNotifying), Order(StageImporting)+1; got != want {
		t.Errorf("notifying sorts at %d, want %d (just past the pipeline)", got, want)
	}
	if Order("par_repair_v2") <= Order(StageNotifying) {
		t.Error("an unrecognised stage must sort after every known one")
	}
}

// A UI has to render "no idea how far along" differently from "0% done", so
// the absence of a measurement is reported as ok=false rather than as zero.
func TestTailFractionSaysWhenProgressIsUnknowable(t *testing.T) {
	if _, ok := (Transfer{Bytes: 0, Total: 0}).Fraction(); ok {
		t.Error("a stage that cannot measure itself reported a real fraction")
	}
	if _, ok := (Transfer{Bytes: -1, Total: 100}).Fraction(); ok {
		t.Error("a negative byte count is not a fraction anybody should render")
	}
	f, ok := (Transfer{Bytes: 25, Total: 100}).Fraction()
	if !ok || f != 0.25 {
		t.Errorf("fraction = %v (%v), want 0.25", f, ok)
	}
	// Over-reporting happens: a client counts the par2 files it fetched on
	// top of the total it announced. Clamp rather than render 140%.
	f, ok = (Transfer{Bytes: 140, Total: 100}).Fraction()
	if !ok || f != 1 {
		t.Errorf("fraction = %v (%v), want a clamp to 1", f, ok)
	}
}

// Elapsed is what the health check reads to spot an import that has been
// running for hours, so it has to count from the transfer's own clock.
func TestTailElapsedCountsFromTheStart(t *testing.T) {
	tr := Transfer{StartedAt: time.Now().Add(-90 * time.Second)}
	if d := tr.Elapsed(); d < 89*time.Second || d > 5*time.Minute {
		t.Errorf("elapsed = %v, want about 90s", d)
	}
}

// The status page renders the snapshot top to bottom, and the thing that has
// been running longest is what a person opening that page came to look at.
func TestTailSnapshotIsOldestFirst(t *testing.T) {
	r := New()
	r.Begin(Transfer{DownloadID: 1, Stage: StageDownloading})
	time.Sleep(2 * time.Millisecond)
	r.Begin(Transfer{DownloadID: 2, Stage: StageDownloading})
	time.Sleep(2 * time.Millisecond)
	r.Begin(Transfer{DownloadID: 3, Stage: StageImporting})

	got := r.Snapshot()
	if len(got) != 3 {
		t.Fatalf("snapshot has %d rows, want 3", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].StartedAt.Before(got[i-1].StartedAt) {
			t.Fatalf("row %d started before row %d — the snapshot is not oldest-first", i, i-1)
		}
	}
	if got[0].DownloadID != 1 || got[2].DownloadID != 3 {
		t.Errorf("order = %d,%d,%d, want 1,2,3",
			got[0].DownloadID, got[1].DownloadID, got[2].DownloadID)
	}
}

// Count exists so the metrics endpoint never has to materialise a slice, and
// it has to be able to answer per-stage as well as in total.
func TestTailCountByStage(t *testing.T) {
	r := New()
	r.Begin(Transfer{DownloadID: 1, Stage: StageImporting})
	r.Begin(Transfer{DownloadID: 2, Stage: StageImporting})
	r.Begin(Transfer{DownloadID: 3, Stage: StageDownloading})

	if n := r.Count(""); n != 3 {
		t.Errorf("total = %d, want 3", n)
	}
	if n := r.Count(StageImporting); n != 2 {
		t.Errorf("importing = %d, want 2", n)
	}
	if n := r.Count(StageNotifying); n != 0 {
		t.Errorf("notifying = %d, want 0", n)
	}
}

// Oldest is the health check's question — "has anything been stuck for
// hours" — and it has to be answerable for one stage without the answer
// being contaminated by a different stage that started earlier.
func TestTailOldestIsPerStage(t *testing.T) {
	r := New()
	if _, ok := New().Oldest(StageImporting); ok {
		t.Error("an empty registry reported an oldest transfer")
	}
	r.Begin(Transfer{DownloadID: 1, Stage: StageDownloading})
	time.Sleep(3 * time.Millisecond)
	r.Begin(Transfer{DownloadID: 2, Stage: StageImporting})

	all, ok := r.Oldest("")
	if !ok {
		t.Fatal(`Oldest("") found nothing with two transfers in flight`)
	}
	imp, ok := r.Oldest(StageImporting)
	if !ok {
		t.Fatal("Oldest(importing) found nothing")
	}
	if imp >= all {
		t.Errorf("importing age %v >= overall age %v — the download's earlier "+
			"start leaked into the per-stage answer", imp, all)
	}
	if _, ok := r.Oldest(StageNotifying); ok {
		t.Error("a stage with nothing at it reported an age")
	}
}

// The copy loop calls Bytes on every buffer — tens of thousands of times a
// minute for a 20 GB file. All but the final one are dropped, because taking
// the registry mutex that often is a cost paid by the very thing this
// package exists to avoid slowing down.
func TestTailProgressIsCoalescedExceptTheLastOne(t *testing.T) {
	r := New()
	h := r.Begin(Transfer{DownloadID: 7, Stage: StageImporting})

	h.Bytes(1, 1000) // first call always lands
	for i := 2; i < 500; i++ {
		h.Bytes(int64(i), 1000)
	}
	got, _ := r.Stage(7)
	if got.Bytes != 1 {
		t.Errorf("bytes = %d, want 1 — the flush interval is not holding back "+
			"the copy loop's updates", got.Bytes)
	}

	// done == total is the final report and is never dropped: a transfer
	// that finished must not be left showing 0.1%.
	h.Bytes(1000, 1000)
	got, _ = r.Stage(7)
	if got.Bytes != 1000 || got.Total != 1000 {
		t.Errorf("final report = %d/%d, want 1000/1000", got.Bytes, got.Total)
	}
}

// A rate is only meaningful once there is enough elapsed time to divide by;
// below that the field stays zero rather than reporting a wild number from a
// millisecond of sampling.
func TestTailRateNeedsEnoughElapsedTimeToBeHonest(t *testing.T) {
	r := New()
	h := r.Begin(Transfer{DownloadID: 7, Stage: StageImporting})
	h.Bytes(1000, 1000)
	if got, _ := r.Stage(7); got.BytesPerSecond != 0 {
		t.Errorf("rate = %v after no elapsed time, want 0", got.BytesPerSecond)
	}

	r2 := New()
	h2 := r2.Begin(Transfer{DownloadID: 8, Stage: StageImporting})
	// Backdate the clock rather than sleeping: the threshold is half a
	// second and a test should not spend it.
	r2.mu.Lock()
	tr := r2.m[key{8, StageImporting}]
	tr.StartedAt = time.Now().Add(-2 * time.Second)
	r2.m[key{8, StageImporting}] = tr
	r2.mu.Unlock()
	h2.Bytes(1000, 1000)
	if got, _ := r2.Stage(8); got.BytesPerSecond < 400 || got.BytesPerSecond > 600 {
		t.Errorf("rate = %v, want about 500 B/s", got.BytesPerSecond)
	}
}

// Detail is the one-line "what is this doing right now" a person reads, and
// End has to be safe to defer and safe to call twice — the import path does
// both.
func TestTailDetailAndEndAreIdempotent(t *testing.T) {
	r := New()
	h := r.Begin(Transfer{DownloadID: 7, Stage: StageNotifying})
	h.Detail("telling plurx what landed")
	if got, _ := r.Stage(7); got.Detail != "telling plurx what landed" {
		t.Errorf("detail = %q", got.Detail)
	}
	h.End()
	h.End()
	if n := r.Count(""); n != 0 {
		t.Errorf("in flight = %d after End, want 0", n)
	}
	// Reporting on an ended transfer is a no-op, not a resurrection: a copy
	// loop can outlive the row it was reporting on.
	h.Bytes(1, 2)
	h.Detail("still going")
	if n := r.Count(""); n != 0 {
		t.Errorf("in flight = %d — reporting after End recreated the row", n)
	}
}

// The handle rides the context so the code that eventually copies bytes can
// report them without every function in between growing a parameter it does
// not care about. Its absence has to be a working no-op, because plenty of
// callers have no registry at all.
func TestTailProgressCallbackRidesTheContext(t *testing.T) {
	if Progress(context.Background()) != nil {
		t.Error("a context with no handle handed out a progress callback")
	}

	r := New()
	h := r.Begin(Transfer{DownloadID: 7, Stage: StageImporting})
	ctx := WithHandle(context.Background(), h)
	report := Progress(ctx)
	if report == nil {
		t.Fatal("a context carrying a handle handed out no callback")
	}
	report(500, 500)
	if got, _ := r.Stage(7); got.Bytes != 500 {
		t.Errorf("bytes = %d, want 500 — the context callback did not reach "+
			"the registry", got.Bytes)
	}
}

// Every method has to work on a nil registry and a zero handle, because that
// is what a caller wired before the registry existed passes — and a status
// nicety must never be the thing that panics the import path.
func TestTailNilRegistryAndZeroHandleAreUsable(t *testing.T) {
	var r *Registry
	h := r.Begin(Transfer{DownloadID: 1, Stage: StageImporting})
	if h == nil {
		t.Fatal("Begin on a nil registry returned a nil handle to dereference")
	}
	h.Bytes(1, 2)
	h.Detail("x")
	h.End()
	r.EndClientStages(1)
	if _, ok := r.Stage(1); ok {
		t.Error("a nil registry reported a stage")
	}
	if r.Snapshot() != nil {
		t.Error("a nil registry returned a snapshot")
	}
	if r.Count("") != 0 {
		t.Error("a nil registry counted transfers")
	}
	if _, ok := r.Oldest(""); ok {
		t.Error("a nil registry reported an oldest transfer")
	}

	var zero *Handle
	zero.Bytes(1, 2)
	zero.Detail("x")
	zero.End()
}

// A notify still retrying is not the download client's problem, so ending
// the client's stages must leave it — and must leave the import alone too.
func TestTailEndClientStagesSparesImportAndNotify(t *testing.T) {
	r := New()
	r.Begin(Transfer{DownloadID: 7, Stage: StageDownloading})
	r.Begin(Transfer{DownloadID: 7, Stage: StageNotifying})
	r.Begin(Transfer{DownloadID: 8, Stage: StageUnpack})
	r.EndClientStages(7)

	if n := r.Count(StageNotifying); n != 1 {
		t.Errorf("notifying = %d, want 1 — a retry that outlives the download "+
			"was swept away with the client's stages", n)
	}
	if n := r.Count(StageUnpack); n != 1 {
		t.Errorf("unpack = %d, want 1 — another download's stages were cleared", n)
	}
}

// Re-observing a stage keeps the bytes it already had. A poll that reports
// no byte count (post-processing stages do not measure themselves) must not
// wipe the progress the previous observation recorded.
func TestTailReobservationKeepsProgressWhenTheNewOneHasNone(t *testing.T) {
	r := New()
	h := r.Begin(Transfer{DownloadID: 7, Stage: StageDownloading, Total: 1000})
	h.Bytes(400, 1000)

	r.Begin(Transfer{DownloadID: 7, Stage: StageDownloading}) // a poll with no numbers
	got, _ := r.Stage(7)
	if got.Bytes != 400 || got.Total != 1000 {
		t.Errorf("progress = %d/%d after a silent re-observation, want 400/1000",
			got.Bytes, got.Total)
	}
}
