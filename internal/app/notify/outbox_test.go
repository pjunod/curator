package notify

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	adapters "github.com/pjunod/monarr/internal/adapters/notify"
	"github.com/pjunod/monarr/internal/adapters/notify/cinematest"
	"github.com/pjunod/monarr/internal/app/acquisition"
	"github.com/pjunod/monarr/internal/app/pipeline"
	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/bus"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// Pipeline contract v2 section 4, end to end: an import writes one
// sub-delivery per directory, the dispatcher posts each to a fake Cinema with
// the real plurx adapter, retries on the schedule and reads the answer back.
// The clock is the dispatcher's own, moved by hand, so a ten-minute outage
// takes no wall time.

type outboxEnv struct {
	t      *testing.T
	ctx    context.Context
	db     *sqlite.DB
	d      *Dispatcher
	cinema *cinematest.Cinema
	root   string
	clock  time.Time
	dl     int64
}

const outboxTransfer = "t-7-5d0c1b1e2f3a4b5c6d7e8f9a0b1c2d3e"

func newOutboxEnv(t *testing.T) *outboxEnv {
	t.Helper()
	db, ctx := tailDB(t)
	root := t.TempDir()
	cinema := cinematest.New(root)
	t.Cleanup(cinema.Close)
	if _, err := db.AddNotifier(ctx, ports.NotifierConfig{Type: "plurx", Name: "cinema", OnImport: true, Enabled: true,
		Settings: map[string]string{"url": cinema.URL(), "apiKey": "plx_secret"}}); err != nil {
		t.Fatal(err)
	}
	b := bus.New(nil)
	t.Cleanup(b.Close)
	e := &outboxEnv{t: t, ctx: ctx, db: db, cinema: cinema, root: root, clock: time.Now()}
	e.d = New(db, b, nil, adapters.New)
	e.d.now = func() time.Time { return e.clock }
	e.d.sendTimeout = 300 * time.Millisecond
	item, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindSeries, Title: "Test Show", Year: 2020, Monitored: true})
	if err != nil {
		t.Fatal(err)
	}
	e.dl, err = db.InsertDownload(ctx, sqlite.Download{MediaItemID: item, ReleaseTitle: "Test.Show.S01-S03.1080p", State: "imported", Transfer: outboxTransfer})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// place writes a library file under the root and returns its path.
func (e *outboxEnv) place(rel, body string) string {
	e.t.Helper()
	p := filepath.Join(e.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		e.t.Fatal(err)
	}
	return p
}

// imported records the import of these placed files the way the importer
// does: history, one sub-delivery per directory with the files as they stand
// now, and the start of import_to_cinema_visible, in one transaction.
func (e *outboxEnv) imported(paths ...string) {
	e.t.Helper()
	ev := acquisition.ImportCompleted{MediaItemID: 1, Release: "Test.Show.S01-S03.1080p", Files: len(paths),
		MediaItemKind: "series", Title: "Test Show", TmdbID: 1399, DownloadID: e.dl, Transfer: outboxTransfer,
		Expected: map[string][]ports.ExpectedFile{}}
	seen := map[string]bool{}
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			e.t.Fatal(err)
		}
		dir := filepath.Dir(p)
		ev.Paths = append(ev.Paths, p)
		if !seen[dir] {
			seen[dir] = true
			ev.Dirs = append(ev.Dirs, dir)
		}
		ev.Expected[dir] = append(ev.Expected[dir], ports.ExpectedFile{Path: p, Bytes: fi.Size(), MtimeNs: fi.ModTime().UnixNano()})
	}
	notifiers, err := e.db.ListNotifiers(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	rows, starts, err := acquisition.ImportOutbox(notifiers, ev, "gen-1")
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.db.RecordImported(e.ctx, sqlite.HistoryWrite{Type: "imported", MediaItemID: 1, ReleaseTitle: ev.Release}, rows, starts); err != nil {
		e.t.Fatal(err)
	}
	// The rows are due at the wall-clock moment they were written.
	if now := time.Now(); now.After(e.clock) {
		e.clock = now
	}
}

func (e *outboxEnv) rows() []sqlite.Delivery {
	e.t.Helper()
	rows, err := e.db.ListDeliveries(e.ctx, 1, 100)
	if err != nil {
		e.t.Fatal(err)
	}
	// Oldest first: the order the directories were imported in.
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	return rows
}

func (e *outboxEnv) pass() { e.d.deliverDue(e.ctx) }

// runFor moves the clock from due time to due time for d, delivering at each.
func (e *outboxEnv) runFor(d time.Duration) {
	e.t.Helper()
	end := e.clock.Add(d)
	for i := 0; i < 1000; i++ {
		next := int64(0)
		for _, r := range e.rows() {
			if r.Status == "pending" && (next == 0 || r.NextAt < next) {
				next = r.NextAt
			}
		}
		if next == 0 || time.UnixMilli(next).After(end) {
			e.clock = end
			return
		}
		if at := time.UnixMilli(next); at.After(e.clock) {
			e.clock = at
		}
		e.pass()
	}
	e.t.Fatal("runFor did not settle")
}

func (e *outboxEnv) timing() (open, closed int) {
	e.t.Helper()
	if err := e.db.R.QueryRowContext(e.ctx, `SELECT COALESCE(SUM(ended_at=0),0), COALESCE(SUM(ended_at>0),0) FROM pipeline_timings WHERE kind=? AND transfer=?`,
		pipeline.ImportToCinemaVisible, outboxTransfer).Scan(&open, &closed); err != nil {
		e.t.Fatal(err)
	}
	return open, closed
}

func (e *outboxEnv) requireVisible(r sqlite.Delivery) {
	e.t.Helper()
	if r.Status != "ok" || r.VisibleAt == 0 || len(r.ItemIDs) == 0 || !strings.HasPrefix(r.Result, "visible → plurx item ") {
		e.t.Fatalf("sub-delivery %s = status %s visible %d items %v result %q (%s)", r.Directory, r.Status, r.VisibleAt, r.ItemIDs, r.Result, r.LastError)
	}
}

// (a) Cinema stopped for ten minutes, a three-directory import, Cinema
// started: every sub-delivery reaches visible with its item ids, and nobody
// pressed Retry.
func TestSubDeliveriesReachVisibleAfterCinemaOutage(t *testing.T) {
	e := newOutboxEnv(t)
	e.cinema.Set(func(c *cinematest.Cinema) { c.Down = true })
	e.imported(
		e.place("Test Show/Season 01/Test.Show.S01E01.mkv", "one"),
		e.place("Test Show/Season 02/Test.Show.S02E01.mkv", "two"),
		e.place("Test Show/Season 03/Test.Show.S03E01.mkv", "three"),
	)
	rows := e.rows()
	if len(rows) != 3 {
		t.Fatalf("sub-deliveries = %d, want one per directory", len(rows))
	}
	keys := map[string]bool{}
	for _, r := range rows {
		if !strings.HasPrefix(r.IdempotencyKey, outboxTransfer+"/gen-1/") || r.Attempt != 1 || len(r.ExpectedFiles) != 1 || r.PublicationID != 0 {
			t.Errorf("sub-delivery = %+v", r)
		}
		keys[r.IdempotencyKey] = true
	}
	if len(keys) != 3 {
		t.Errorf("keys = %v, want one per directory", keys)
	}

	e.runFor(10 * time.Minute)
	for _, r := range e.rows() {
		if r.Status != "pending" || r.LastError == "" {
			t.Fatalf("during the outage %s = %s (%q), want pending with the reason", r.Directory, r.Status, r.LastError)
		}
		if r.Attempt != 1 {
			t.Errorf("attempt = %d: a connection error is ambiguous and keeps the key", r.Attempt)
		}
	}
	if open, _ := e.timing(); open != 1 {
		t.Errorf("import_to_cinema_visible open = %d, want 1 waiting", open)
	}

	e.cinema.Set(func(c *cinematest.Cinema) { c.Down = false })
	e.runFor(2 * time.Hour)
	for _, r := range e.rows() {
		e.requireVisible(r)
		if want := e.cinema.ItemID(r.ExpectedFiles[0].Path); r.ItemIDs[0] != want {
			t.Errorf("%s item ids = %v, want %d", r.Directory, r.ItemIDs, want)
		}
	}
	if open, closed := e.timing(); open != 0 || closed != 1 {
		t.Errorf("import_to_cinema_visible open %d closed %d, want it ended at visible", open, closed)
	}
	if reqs := e.cinema.Requests(); len(reqs) != 3 {
		t.Errorf("cinema requests = %d, want one per directory", len(reqs))
	}
}

// (a) A lost response — Cinema answers after Curator's deadline — is
// ambiguous: the retry carries the same request_key and Cinema records one
// request, not two.
func TestLostResponseReusesKey(t *testing.T) {
	e := newOutboxEnv(t)
	e.cinema.Set(func(c *cinematest.Cinema) {
		c.Delay = func(_ string, n int) time.Duration {
			if n == 1 {
				return time.Second
			}
			return 0
		}
	})
	e.imported(e.place("Heat (1995)/Heat.mkv", "heat"))
	e.pass()
	r := e.rows()[0]
	if r.Status != "pending" || r.Attempt != 1 {
		t.Fatalf("after the lost response = %s attempt %d (%s)", r.Status, r.Attempt, r.LastError)
	}
	e.runFor(time.Minute)
	e.requireVisible(e.rows()[0])
	posts := e.cinema.Posts()
	if len(posts) != 2 || posts[0] != posts[1] || !strings.HasSuffix(posts[0], "/1") {
		t.Errorf("posts = %v, want the same request_key twice", posts)
	}
	if reqs := e.cinema.Requests(); len(reqs) != 1 {
		t.Errorf("cinema recorded %d requests, want 1", len(reqs))
	}
}

// (a) Three slow directories: each sub-delivery has its own deadline, none
// starves the others, and a directory already visible is never posted again.
func TestThreeSlowDirectoriesDoNotStarve(t *testing.T) {
	e := newOutboxEnv(t)
	e.cinema.Set(func(c *cinematest.Cinema) {
		c.Delay = func(_ string, n int) time.Duration {
			if n == 1 {
				return time.Second // past the 300 ms deadline, every directory
			}
			return 0
		}
	})
	e.imported(
		e.place("Show/Season 01/a.mkv", "a"),
		e.place("Show/Season 02/b.mkv", "b"),
		e.place("Show/Season 03/c.mkv", "c"),
	)
	e.pass()
	if posts := e.cinema.Posts(); len(posts) != 3 {
		t.Fatalf("first pass posted %v, want each directory once under its own deadline", posts)
	}
	for _, r := range e.rows() {
		if r.Status != "pending" || r.Attempts != 1 {
			t.Fatalf("%s = %s after %d tries", r.Directory, r.Status, r.Attempts)
		}
	}
	e.runFor(time.Minute)
	for _, r := range e.rows() {
		e.requireVisible(r)
	}
	e.runFor(3 * time.Hour)
	posts := e.cinema.Posts()
	if len(posts) != 6 {
		t.Fatalf("posts = %d (%v), want 6: two per directory and none after visible", len(posts), posts)
	}
	per := map[string]int{}
	for _, p := range posts {
		per[p]++
	}
	for k, n := range per {
		if n != 2 {
			t.Errorf("key %s posted %d times, want 2", k, n)
		}
	}
}

// (a) Cinema indexes an old sibling and skips the new file: a warning naming
// the file and Cinema's reason, the attempt advances, and once the file is
// restored the next attempt is a fresh scan that ends visible.
func TestSkippedFileWarnsThenFreshScanAfterRestore(t *testing.T) {
	e := newOutboxEnv(t)
	e.place("Show/Season 01/Show.S01E01.mkv", "old sibling")
	fresh := e.place("Show/Season 01/Show.S01E02.mkv", "new")
	e.cinema.Set(func(c *cinematest.Cinema) { c.Skip = map[string]string{fresh: "file vanished during probe"} })
	e.imported(fresh)
	e.pass()
	r := e.rows()[0]
	want := "warning: expected " + fresh + " not placed (file vanished during probe)"
	if r.Status != "pending" || r.Result != want || r.Attempt != 2 || r.VisibleAt != 0 {
		t.Fatalf("after the skip = %s attempt %d result %q, want pending attempt 2 %q", r.Status, r.Attempt, r.Result, want)
	}
	e.cinema.Set(func(c *cinematest.Cinema) { c.Skip = nil })
	e.runFor(time.Minute)
	e.requireVisible(e.rows()[0])
	posts := e.cinema.Posts()
	if len(posts) != 2 || !strings.HasSuffix(posts[0], "/1") || !strings.HasSuffix(posts[1], "/2") ||
		strings.TrimSuffix(posts[0], "/1") != strings.TrimSuffix(posts[1], "/2") {
		t.Errorf("posts = %v, want the same key with attempt 1 then 2", posts)
	}
	if reqs := e.cinema.Requests(); len(reqs) != 2 {
		t.Errorf("cinema requests = %d, want a fresh one for the new attempt", len(reqs))
	}
}

// (a) A same-path replacement with equal size and the same whole-second
// mtime: only mtime_ns tells it from the old file. A Cinema that reports its
// cached record is not visible; the re-probe that reports the new mtime_ns is.
func TestSamePathReplacementNeedsMtimeMatch(t *testing.T) {
	e := newOutboxEnv(t)
	p := e.place("Heat (1995)/Heat.mkv", "AAAA")
	second := time.Unix(1794076138, 0)
	if err := os.Chtimes(p, second, second.Add(100)); err != nil {
		t.Fatal(err)
	}
	old, _ := os.Stat(p)
	e.place("Heat (1995)/Heat.mkv", "BBBB")
	if err := os.Chtimes(p, second, second.Add(900)); err != nil {
		t.Fatal(err)
	}
	now, _ := os.Stat(p)
	if old.Size() != now.Size() || old.ModTime().Unix() != now.ModTime().Unix() || old.ModTime().UnixNano() == now.ModTime().UnixNano() {
		t.Skipf("this filesystem cannot hold two mtimes in one second (%v, %v)", old.ModTime(), now.ModTime())
	}
	e.cinema.Set(func(c *cinematest.Cinema) {
		c.Cached = map[string]ports.PlacedReport{p: {Path: p, State: "placed", ItemID: 1201, Bytes: old.Size(), MtimeNs: old.ModTime().UnixNano()}}
	})
	e.imported(p)
	e.pass()
	r := e.rows()[0]
	if r.Status != "pending" || r.VisibleAt != 0 || !strings.Contains(r.Result, "not placed") || r.Attempt != 2 {
		t.Fatalf("a stale record = %s %q attempt %d, want a warning and a new attempt", r.Status, r.Result, r.Attempt)
	}
	e.cinema.Set(func(c *cinematest.Cinema) { c.Cached = nil })
	e.runFor(time.Minute)
	e.requireVisible(e.rows()[0])
}

// (d) Unmount → import → remount: the 503 path_unreadable is retried with a
// fresh attempt and ends visible, with no manual retry.
func Test503PathUnreadableIsRetriedToVisible(t *testing.T) {
	e := newOutboxEnv(t)
	e.cinema.Set(func(c *cinematest.Cinema) { c.Unreadable = true })
	e.imported(e.place("Heat (1995)/Heat.mkv", "heat"))
	e.runFor(10 * time.Minute)
	r := e.rows()[0]
	if r.Status != "pending" || !strings.Contains(r.LastError, "cannot read") || r.Attempt < 2 {
		t.Fatalf("during the unmount = %s attempt %d (%s)", r.Status, r.Attempt, r.LastError)
	}
	e.cinema.Set(func(c *cinematest.Cinema) { c.Unreadable = false })
	e.runFor(time.Hour)
	e.requireVisible(e.rows()[0])
}

// (d) A path outside every Cinema root is 422 path_outside_roots: permanent,
// once, and said on the download's trace.
func TestNewCinemaOutsideRootsIsPermanent(t *testing.T) {
	e := newOutboxEnv(t)
	outside := filepath.Join(t.TempDir(), "Heat (1995)", "Heat.mkv")
	if err := os.MkdirAll(filepath.Dir(outside), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.imported(outside)
	e.runFor(3 * time.Hour)
	r := e.rows()[0]
	if r.Status != "failed" || r.Attempts != 1 || !strings.Contains(r.LastError, "not have") {
		t.Fatalf("outside = %s after %d tries (%s), want failed once", r.Status, r.Attempts, r.LastError)
	}
	if posts := e.cinema.Posts(); len(posts) != 1 {
		t.Errorf("posts = %v, want exactly one", posts)
	}
	dl, err := e.db.GetDownload(e.ctx, e.dl)
	if err != nil {
		t.Fatal(err)
	}
	var trace string
	for _, h := range dl.Handoff {
		if h.Step == stepNotifyPlurx {
			trace = h.Detail
		}
	}
	if !strings.Contains(trace, "failed after 1 attempt") || !strings.Contains(trace, filepath.Dir(outside)) {
		t.Errorf("trace = %q", trace)
	}
}

// A queued request is polled on the schedule with the same key; while it
// waits the row carries Cinema's waiting_on, and the scan ends visible
// without a second POST.
func TestQueuedRequestIsPolledUntilVisible(t *testing.T) {
	e := newOutboxEnv(t)
	e.cinema.Set(func(c *cinematest.Cinema) { c.Queue, c.WaitingOn = true, "full_scan:3" })
	e.imported(e.place("Heat (1995)/Heat.mkv", "heat"))
	e.pass()
	r := e.rows()[0]
	if r.Status != "pending" || r.RemoteRequestID == "" || r.RemoteState != "queued" {
		t.Fatalf("after 202 = %+v", r)
	}
	e.runFor(35 * time.Minute)
	r = e.rows()[0]
	if r.WaitingOn != "full_scan:3" || !strings.Contains(r.Result, "waiting on full_scan:3") || r.Status != "pending" {
		t.Fatalf("while queued = %s waiting %q result %q", r.Status, r.WaitingOn, r.Result)
	}
	if polls := e.cinema.Polls(); len(polls) < 4 {
		t.Errorf("polls = %d over 35 minutes, want the schedule (5s, 30s, 2m, 5m, 15m…)", len(polls))
	}
	e.cinema.Release()
	e.runFor(2 * time.Hour)
	e.requireVisible(e.rows()[0])
	if posts := e.cinema.Posts(); len(posts) != 1 {
		t.Errorf("posts = %v, want one: a queued request is read back, not re-posted", posts)
	}
}

// Contract section 6, new Curator against an old Cinema: request_key,
// wait_ms and expected_files are ignored, a 202 is handled, and the status
// read is refused to the scan key — the sub-delivery is done, reading
// "queued (cinema does not report results)", and the seam is not measured.
func TestOldCinemaQueuedIsDoneWithoutResults(t *testing.T) {
	e := newOutboxEnv(t)
	e.cinema.Set(func(c *cinematest.Cinema) { c.Old, c.Queue = true, true })
	e.imported(e.place("Heat (1995)/Heat.mkv", "heat"))
	e.runFor(time.Hour)
	r := e.rows()[0]
	if r.Status != "ok" || r.Result != "queued (cinema does not report results)" || r.VisibleAt != 0 {
		t.Fatalf("old cinema = %s %q visible %d", r.Status, r.Result, r.VisibleAt)
	}
	if polls := e.cinema.Polls(); len(polls) != 1 {
		t.Errorf("polls = %d, want one: the 403 settles it", len(polls))
	}
	if open, closed := e.timing(); open != 0 || closed != 0 {
		t.Errorf("timing open %d closed %d, want discarded (not measurable)", open, closed)
	}
}

// An old Cinema that scans in time answers 200 with items and no per-file
// report: done, with the item ids, never "not placed" — or the attempt would
// advance and re-scan hourly forever.
func TestOldCinemaScannedWithoutFilesIsDone(t *testing.T) {
	e := newOutboxEnv(t)
	e.cinema.Set(func(c *cinematest.Cinema) { c.Old = true })
	e.imported(e.place("Heat (1995)/Heat.mkv", "heat"))
	e.runFor(time.Hour)
	r := e.rows()[0]
	if r.Status != "ok" || r.VisibleAt != 0 || !strings.Contains(r.Result, "cinema does not report results") || len(r.ItemIDs) != 1 {
		t.Fatalf("old cinema scanned = %s %q items %v", r.Status, r.Result, r.ItemIDs)
	}
	if posts := e.cinema.Posts(); len(posts) != 1 {
		t.Errorf("posts = %v, want one", posts)
	}
}

// A sub-delivery re-imported (the same directory of the same publication
// written again) refreshes its expected files and takes a new attempt.
func TestReimportedDirectoryTakesANewAttempt(t *testing.T) {
	e := newOutboxEnv(t)
	p := e.place("Heat (1995)/Heat.mkv", "heat")
	e.imported(p)
	e.runFor(time.Minute)
	e.requireVisible(e.rows()[0])
	e.place("Heat (1995)/Heat.mkv", "heat, again and longer")
	e.imported(p)
	rows := e.rows()
	if len(rows) != 1 || rows[0].Status != "pending" || rows[0].Attempt != 2 || rows[0].ExpectedFiles[0].Bytes != int64(len("heat, again and longer")) {
		t.Fatalf("re-import = %+v", rows)
	}
	e.runFor(time.Minute)
	e.requireVisible(e.rows()[0])
}
