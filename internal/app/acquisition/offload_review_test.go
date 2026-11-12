package acquisition

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// offloadedMovie is a movie whose Offload has completed.
func (f offloadFixture) offloadedMovie(t *testing.T) int64 {
	t.Helper()
	m, _, _ := f.movieWithFiles(t)
	req := OffloadRequest{ItemID: m.ID, Scope: "item"}
	f.wait(t, f.start(t, req, f.preview(t, req)).ID)
	return m.ID
}

// An Offload that starts between Re-acquire's first look and its reservation
// must win: Re-acquire looks again once it holds the title and answers
// `offloading` without clearing anything.
func TestReacquireRechecksForAnOffloadAfterReserving(t *testing.T) {
	f := newOffloadFixture(t)
	ctx := context.Background()
	id := f.offloadedMovie(t)
	unlock, err := f.svc.reserveItem(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	type answer struct {
		res ReacquireResult
		err error
	}
	got := make(chan answer, 1)
	go func() {
		res, err := f.svc.Reacquire(ctx, ReacquireRequest{ItemID: id, Scope: "item"})
		got <- answer{res, err}
	}()
	time.Sleep(100 * time.Millisecond) // it is past its first look, waiting for the title
	if _, _, err := f.db.CreateOffloadOperation(ctx, sqlite.OffloadRecord{Op: sqlite.OffloadOperation{
		ID: "of-race", RequestID: "race", Fingerprint: "x", State: "deleting", ItemID: id, Scope: "item"}}); err != nil {
		t.Fatal(err)
	}
	unlock()
	a := <-got
	if !errors.Is(a.err, ErrOffloadBusy) || a.res.State != ReacquireOffloading || a.res.Cleared {
		t.Fatalf("answer = %+v, %v", a.res, a.err)
	}
	if !mustItem(t, f.db, id).Offloaded() {
		t.Fatal("Re-acquire cleared a title whose Offload had just started")
	}
}

// Cinema's webhook call is synchronous: a held title is answered (state
// `offloaded`, not started), never awaited indefinitely.
func TestReacquireAnswersInsteadOfHangingWhenTheTitleIsHeld(t *testing.T) {
	f := newOffloadFixture(t)
	ctx := context.Background()
	id := f.offloadedMovie(t)
	old := reacquireReserveWait
	reacquireReserveWait = 50 * time.Millisecond
	t.Cleanup(func() { reacquireReserveWait = old })
	unlock, err := f.svc.reserveItem(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	start := time.Now()
	res, err := f.svc.Reacquire(ctx, ReacquireRequest{ItemID: id, Scope: "item"})
	if err != nil || res.State != ReacquireOffloaded || res.Cleared || time.Since(start) > 2*time.Second {
		t.Fatalf("answer = %+v, %v after %v", res, err, time.Since(start))
	}
}

// The two "not back yet" answers are told apart: a running Offload is
// `offloading`; a scope that is offloaded with none running is `offloaded`.
func TestReacquireStatusTellsOffloadingFromOffloaded(t *testing.T) {
	f := newOffloadFixture(t)
	ctx := context.Background()
	id := f.offloadedMovie(t)
	res, err := f.svc.ReacquireStatus(ctx, ReacquireRequest{ItemID: id, Scope: "item"})
	if err != nil || res.State != ReacquireOffloaded {
		t.Fatalf("still offloaded: %+v %v", res, err)
	}
	if _, _, err := f.db.CreateOffloadOperation(ctx, sqlite.OffloadRecord{Op: sqlite.OffloadOperation{
		ID: "of-run", RequestID: "run", Fingerprint: "y", State: "deleting", ItemID: id, Scope: "item"}}); err != nil {
		t.Fatal(err)
	}
	res, err = f.svc.ReacquireStatus(ctx, ReacquireRequest{ItemID: id, Scope: "item"})
	if err != nil || res.State != ReacquireOffloading {
		t.Fatalf("offload running: %+v %v", res, err)
	}
	if res, err = f.svc.Reacquire(ctx, ReacquireRequest{ItemID: id, Scope: "item"}); !errors.Is(err, ErrOffloadBusy) || res.State != ReacquireOffloading {
		t.Fatalf("reacquire while running: %+v %v", res, err)
	}
}

// Re-acquiring one episode of a finished Offload narrows Cinema's unconfirmed
// request to the paths that stay gone; it does not cancel the others.
func TestReacquireOfOneEpisodeNarrowsTheCinemaRequest(t *testing.T) {
	f := newOffloadFixture(t)
	ctx := context.Background()
	f.cinema(t, ports.FeatureRemovedPaths, ports.FeatureOffload)
	show := mustItem(t, f.db, f.show)
	ep1, ep2 := show.Seasons[0].Episodes[0].ID, show.Seasons[0].Episodes[1].ID
	e1 := filepath.Join(show.Path, "Season 1", "e1.mkv")
	e2 := filepath.Join(show.Path, "Season 1", "e2.mkv")
	f.file(t, show.ID, e1, "one", "", ep1)
	f.file(t, show.ID, e2, "two", "", ep2)
	req := OffloadRequest{ItemID: show.ID, Scope: "season", Season: 1}
	op := f.start(t, req, f.preview(t, req))
	f.wait(t, op.ID)
	if res, err := f.svc.Reacquire(ctx, ReacquireRequest{ItemID: show.ID, Scope: "episode", EpisodeID: ep1}); err != nil || !res.Cleared {
		t.Fatalf("reacquire: %+v %v", res, err)
	}
	dels, _ := f.db.OperationDeliveries(ctx, op.ID)
	pending := 0
	for _, d := range dels {
		if d.Status != "pending" {
			continue
		}
		pending++
		if strings.Contains(d.Payload, e1) || !strings.Contains(d.Payload, e2) {
			t.Fatalf("the request must name only the path that stays gone: %s", d.Payload)
		}
	}
	if pending != 1 {
		t.Fatalf("deliveries: %+v", dels)
	}
	// Re-acquiring the rest leaves nothing to tell Cinema.
	if _, err := f.svc.Reacquire(ctx, ReacquireRequest{ItemID: show.ID, Scope: "episode", EpisodeID: ep2}); err != nil {
		t.Fatal(err)
	}
	dels, _ = f.db.OperationDeliveries(ctx, op.ID)
	for _, d := range dels {
		if d.Status == "pending" {
			t.Fatalf("a request for paths that are back is still pending: %+v", d)
		}
	}
}

// An item-scope Offload of a show stores the whole-show intent on the item
// (never inferred); a season-scope one does not, and any Re-acquire ends it.
func TestOffloadStoresTheWholeShowIntent(t *testing.T) {
	f := newOffloadFixture(t)
	ctx := context.Background()
	show := mustItem(t, f.db, f.show)
	req := OffloadRequest{ItemID: show.ID, Scope: "season", Season: 1}
	f.wait(t, f.start(t, req, f.preview(t, req)).ID)
	if mustItem(t, f.db, show.ID).Offloaded() {
		t.Fatal("a season-scope Offload recorded a whole-show intent")
	}
	if _, err := f.svc.Reacquire(ctx, ReacquireRequest{ItemID: show.ID, Scope: "season", Season: 1}); err != nil {
		t.Fatal(err)
	}
	req = OffloadRequest{ItemID: show.ID, Scope: "item"}
	f.wait(t, f.start(t, req, f.preview(t, req)).ID)
	if !mustItem(t, f.db, show.ID).Offloaded() {
		t.Fatal("an item-scope Offload did not record the whole-show intent")
	}
	if _, err := f.svc.Reacquire(ctx, ReacquireRequest{ItemID: show.ID, Scope: "season", Season: 1}); err != nil {
		t.Fatal(err)
	}
	if mustItem(t, f.db, show.ID).Offloaded() {
		t.Fatal("a Re-acquire left the whole-show intent")
	}
}

// The panel never says "In cinema" for a request that was settled as not
// sent: it says Cinema was not told.
func TestOffloadStatusSaysCinemaWasNotTold(t *testing.T) {
	f := newOffloadFixture(t)
	ctx := context.Background()
	f.cinema(t) // no features
	m, _, _ := f.movieWithFiles(t)
	req := OffloadRequest{ItemID: m.ID, Scope: "item"}
	op := f.start(t, req, f.preview(t, req))
	f.wait(t, op.ID)
	dels, _ := f.db.OperationDeliveries(ctx, op.ID)
	if len(dels) != 1 {
		t.Fatalf("deliveries: %+v", dels)
	}
	d := dels[0]
	d.Status, d.NextAt, d.VisibleAt, d.Result = "ok", 0, 0, "warning: cinema not told: it supports neither offload nor removed paths; its next full scan drops the title"
	d.FileResults = []sqlite.DeliveryFileResult{{Path: "/x", State: "retained", Code: "not_told", Warning: "cinema not told: it supports neither offload nor removed paths; its next full scan drops the title"}}
	if err := f.db.SettleDelivery(ctx, d, sqlite.DeliveryTiming{Discard: true}); err != nil {
		t.Fatal(err)
	}
	st, err := f.svc.GetOffload(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(st.CinemaLine, "In cinema") || !strings.Contains(st.CinemaLine, "not told") {
		t.Fatalf("cinema line = %q", st.CinemaLine)
	}
}

// Stop while the runner is deleting: every file that is gone from disk is in
// the journal and in Cinema's narrowed request, and none is lost between the
// runner's last check and the stop.
func TestOffloadStopWhileRunningLosesNoDeletedFile(t *testing.T) {
	f := newOffloadFixture(t)
	ctx := context.Background()
	f.cinema(t, ports.FeatureRemovedPaths, ports.FeatureOffload)
	show := mustItem(t, f.db, f.show)
	ep1, ep2 := show.Seasons[0].Episodes[0].ID, show.Seasons[0].Episodes[1].ID
	paths := []string{filepath.Join(show.Path, "Season 1", "e1.mkv"), filepath.Join(show.Path, "Season 1", "e2.mkv")}
	f.file(t, show.ID, paths[0], "one", "", ep1)
	f.file(t, show.ID, paths[1], "two", "", ep2)
	req := OffloadRequest{ItemID: show.ID, Scope: "season", Season: 1}
	op := f.start(t, req, f.preview(t, req))
	if err := f.svc.StopOffload(ctx, op.ID); err != nil && !errors.Is(err, ErrInvalidOffload) {
		t.Fatal(err)
	}
	st := f.wait(t, op.ID)
	got := mustItem(t, f.db, show.ID)
	dels, _ := f.db.OperationDeliveries(ctx, op.ID)
	for i, p := range paths {
		_, statErr := os.Stat(p)
		gone := os.IsNotExist(statErr)
		told := false
		for _, d := range dels {
			told = told || (d.Status == "pending" && strings.Contains(d.Payload, p))
		}
		if gone != told {
			t.Fatalf("%s: gone=%v but told cinema=%v (op %+v)", p, gone, told, st)
		}
		if gone == got.Seasons[0].Episodes[i].HasFile {
			t.Fatalf("%s: gone=%v but the library says HasFile=%v", p, gone, got.Seasons[0].Episodes[i].HasFile)
		}
		if !gone && got.Seasons[0].Episodes[i].Offloaded() {
			t.Fatalf("%s is still on disk but its episode is offloaded", p)
		}
	}
}
