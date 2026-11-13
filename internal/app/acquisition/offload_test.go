package acquisition

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// offloadFixture is the Re-match fixture plus helpers for Offload.
type offloadFixture struct {
	rematchFixture
}

func newOffloadFixture(t *testing.T) offloadFixture {
	t.Helper()
	f := offloadFixture{newRematchFixture(t)}
	// Background work (the Re-acquire search, a resumed delete) stops with the
	// test and is waited for before the database closes: no "database is
	// closed" noise from a search that outlives it.
	ctx, cancel := context.WithCancel(context.Background())
	f.svc.rematchMu.Lock()
	f.svc.rematchCtx = ctx
	f.svc.rematchMu.Unlock()
	t.Cleanup(func() { cancel(); f.svc.importWG.Wait() })
	return f
}

func (f offloadFixture) cinema(t *testing.T, features ...string) {
	t.Helper()
	id, err := f.db.AddNotifier(context.Background(), ports.NotifierConfig{Type: "plurx", Name: "cinema", Settings: map[string]string{"url": "http://c", "apiKey": "k"}, OnImport: true, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(features) > 0 {
		if err := f.db.SetNotifierFeatures(context.Background(), id, features); err != nil {
			t.Fatal(err)
		}
	}
}

// movieWithFiles is an item with a video and a subtitle sidecar on disk.
func (f offloadFixture) movieWithFiles(t *testing.T) (domain.MediaItem, string, string) {
	t.Helper()
	m := f.movie(t, "Dune", 2021, 438631, "")
	video := filepath.Join(m.Path, "Dune (2021) [Bluray-2160p].mkv")
	f.file(t, m.ID, video, "dune video bytes", "")
	sub := strings.TrimSuffix(video, ".mkv") + ".en.srt"
	writeFile(t, sub, "subs")
	// A neighbour that shares the stem but is media in its own right stays.
	return m, video, sub
}

func (f offloadFixture) preview(t *testing.T, req OffloadRequest) OffloadPreview {
	t.Helper()
	p, err := f.svc.PreviewOffload(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (f offloadFixture) start(t *testing.T, req OffloadRequest, p OffloadPreview) sqlite.OffloadOperation {
	t.Helper()
	req.RequestID, req.PlanFingerprint = "req-"+p.Fingerprint, p.Fingerprint
	op, err := f.svc.StartOffload(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func (f offloadFixture) wait(t *testing.T, id string) OffloadStatus {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st, err := f.svc.GetOffload(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		switch st.State {
		case "complete", "cancelled", "failed":
			time.Sleep(20 * time.Millisecond) // the goroutine releases the lease just after
			return st
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("offload %s did not finish", id)
	return OffloadStatus{}
}

func (f offloadFixture) waitState(t *testing.T, id, state string) OffloadStatus {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st, err := f.svc.GetOffload(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if st.State == state {
			return st
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("offload %s never reached %s", id, state)
	return OffloadStatus{}
}

func wantedIDs(t *testing.T, svc *Service) []string {
	t.Helper()
	w, err := svc.Wanted(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, x := range w {
		ids = append(ids, string(x.ID()))
	}
	return ids
}

// A movie Offload deletes the video and its sidecar, keeps the folder and the
// title, marks it offloaded without touching `monitored`, records history and
// tells Cinema only after the files are gone (the request is held until then).
func TestOffloadMovieDeletesFilesKeepsTitleAndTellsCinemaLast(t *testing.T) {
	f := newOffloadFixture(t)
	ctx := context.Background()
	f.cinema(t, ports.FeatureRemovedPaths, ports.FeatureOffload)
	m, video, sub := f.movieWithFiles(t)
	other := filepath.Join(m.Path, "Dune (2021) trailer.mkv") // another video: never a sidecar
	writeFile(t, other, "trailer")

	p := f.preview(t, OffloadRequest{ItemID: m.ID, Scope: "item"})
	if len(p.Files) != 2 || p.Bytes != int64(len("dune video bytes")+len("subs")) || p.Blocked != nil {
		t.Fatalf("preview: %+v", p)
	}
	if p.CinemaWarning || !strings.Contains(p.Cinema, "keeps the title") {
		t.Fatalf("cinema line: %q %v", p.Cinema, p.CinemaWarning)
	}
	if p.FreeBefore < 0 || p.FreeAfter != p.FreeBefore+p.Freed {
		t.Fatalf("free space: %+v", p)
	}
	// Nothing happened yet.
	if _, err := os.Stat(video); err != nil {
		t.Fatal(err)
	}
	op := f.start(t, OffloadRequest{ItemID: m.ID, Scope: "item"}, p)
	st := f.wait(t, op.ID)
	if st.State != "complete" || st.BytesFreed != p.Bytes {
		t.Fatalf("operation: %+v", st)
	}
	for _, gone := range []string{video, sub} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatalf("%s still there: %v", gone, err)
		}
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("a neighbouring video was deleted: %v", err)
	}
	if _, err := os.Stat(m.Path); err != nil {
		t.Fatalf("the item folder must stay: %v", err)
	}
	got, err := f.db.GetMediaItemFull(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Offloaded() || !got.Monitored || len(got.Files) != 0 {
		t.Fatalf("item: offloaded=%v monitored=%v files=%d", got.Offloaded(), got.Monitored, len(got.Files))
	}
	hist, _ := f.db.ListHistory(ctx)
	var found bool
	for _, h := range hist {
		found = found || h.Type == HistoryOffloaded
	}
	if !found {
		t.Fatalf("no offloaded history event: %+v", hist)
	}
	// Cinema's request: operation-keyed, event offload, probe-only shape, due
	// now that the files are gone.
	dels, err := f.db.OperationDeliveries(ctx, op.ID)
	if err != nil || len(dels) != 1 {
		t.Fatalf("deliveries: %v %+v", err, dels)
	}
	d := dels[0]
	if d.Event != "offload" || !strings.HasPrefix(d.IdempotencyKey, "op/"+op.ID+"/") || d.Status != "pending" || d.NextAt > time.Now().UnixMilli()+1000 {
		t.Fatalf("delivery: %+v", d)
	}
	if !strings.Contains(d.Payload, `"offloadedPaths"`) || !strings.Contains(d.Payload, video) {
		t.Fatalf("payload: %s", d.Payload)
	}
}

// Without the offload feature, Cinema is sent plain removed paths and the
// dialog said beforehand it will drop the title (advisory, never a gate).
func TestOffloadAlwaysWritesTheOffloadEventAndWarnsBeforehand(t *testing.T) {
	f := newOffloadFixture(t)
	ctx := context.Background()
	f.cinema(t, ports.FeatureRemovedPaths)
	m, _, _ := f.movieWithFiles(t)
	p := f.preview(t, OffloadRequest{ItemID: m.ID, Scope: "item"})
	if !p.CinemaWarning || !strings.Contains(p.Cinema, "drop the title and its watch history") {
		t.Fatalf("no advisory: %q", p.Cinema)
	}
	op := f.start(t, OffloadRequest{ItemID: m.ID, Scope: "item"}, p)
	f.wait(t, op.ID)
	dels, _ := f.db.OperationDeliveries(ctx, op.ID)
	// The event is fixed (offload + offloadedPaths); the adapter downgrades at
	// send time from the features it re-reads then, so a Cinema that changes
	// between Start and send is still told the right way.
	if len(dels) != 1 || dels[0].Event != "offload" || !strings.Contains(dels[0].Payload, `"offloadedPaths"`) || strings.Contains(dels[0].Payload, `"removedPaths"`) {
		t.Fatalf("deliveries: %+v", dels)
	}
}

// Every wanted path follows the one Mon computation: an offloaded movie, season
// and episode are not wanted, a monitored toggle is untouched, and Re-acquire
// brings them back.
func TestOffloadStateFoldsIntoWantedForMovieSeasonAndEpisode(t *testing.T) {
	f := newOffloadFixture(t)
	ctx := context.Background()
	m := f.movie(t, "Dune", 2021, 438631, "") // monitored, no file: wanted
	if ids := wantedIDs(t, f.svc); !hasID(ids, "movie:"+itoa(m.ID)) {
		t.Fatalf("movie not wanted to begin with: %v", ids)
	}
	p := f.preview(t, OffloadRequest{ItemID: m.ID, Scope: "item"})
	f.wait(t, f.start(t, OffloadRequest{ItemID: m.ID, Scope: "item"}, p).ID)
	if ids := wantedIDs(t, f.svc); hasID(ids, "movie:"+itoa(m.ID)) {
		t.Fatalf("an offloaded movie is wanted: %v", ids)
	}
	w, err := f.svc.target(ctx, mustItem(t, f.db, m.ID), 0, 0)
	if err != nil || w.Monitored() {
		t.Fatalf("Mon must fold offloaded: %v %v", w, err)
	}

	show := mustItem(t, f.db, f.show)
	ep1 := show.Seasons[0].Episodes[0].ID
	// One episode.
	req := OffloadRequest{ItemID: show.ID, Scope: "episode", EpisodeID: ep1}
	p = f.preview(t, req)
	f.wait(t, f.start(t, req, p).ID)
	ids := wantedIDs(t, f.svc)
	if hasID(ids, "episode:"+itoa(show.ID)+":1:1") || !hasID(ids, "episode:"+itoa(show.ID)+":1:2") {
		t.Fatalf("episode offload: %v", ids)
	}
	// The rest of the season (season scope): nothing of the show is wanted.
	req = OffloadRequest{ItemID: show.ID, Scope: "season", Season: 1}
	p = f.preview(t, req)
	f.wait(t, f.start(t, req, p).ID)
	if ids := wantedIDs(t, f.svc); hasID(ids, "episode:"+itoa(show.ID)+":1:2") {
		t.Fatalf("season offload leaves an episode wanted: %v", ids)
	}
	got := mustItem(t, f.db, show.ID)
	if !got.Seasons[0].Offloaded() || !got.Seasons[0].Episodes[1].Offloaded() || !got.Monitored || !got.Seasons[0].Monitored {
		t.Fatalf("season state: %+v", got.Seasons[0])
	}
	// Re-acquire the one episode: the season flag stays (ep2 is still offloaded).
	res, err := f.svc.Reacquire(ctx, ReacquireRequest{ItemID: show.ID, Scope: "episode", EpisodeID: ep1})
	if err != nil || !res.Cleared || res.State != ReacquireSearching {
		t.Fatalf("reacquire: %+v %v", res, err)
	}
	got = mustItem(t, f.db, show.ID)
	if got.Seasons[0].Episodes[0].Offloaded() || !got.Seasons[0].Offloaded() || !got.Seasons[0].Episodes[1].Offloaded() {
		t.Fatalf("partial re-acquire: %+v", got.Seasons[0])
	}
	// B1: the re-acquired episode is WANTED again while its sibling stays
	// offloaded under the (still flagged) season.
	ids = wantedIDs(t, f.svc)
	if !hasID(ids, "episode:"+itoa(show.ID)+":1:1") || hasID(ids, "episode:"+itoa(show.ID)+":1:2") {
		t.Fatalf("partial re-acquire wantedness: %v", ids)
	}
	if w, err := f.svc.target(ctx, mustItem(t, f.db, show.ID), 1, 1); err != nil || !w.Monitored() {
		t.Fatalf("episode Mon after partial re-acquire: %v %v", w, err)
	}
	// Re-acquire the season clears whatever is left inside it (partly offloaded works).
	res, err = f.svc.Reacquire(ctx, ReacquireRequest{ItemID: show.ID, Scope: "season", Season: 1})
	if err != nil || !res.Cleared {
		t.Fatalf("reacquire season: %+v %v", res, err)
	}
	got = mustItem(t, f.db, show.ID)
	if got.Seasons[0].Offloaded() || got.Seasons[0].Episodes[1].Offloaded() {
		t.Fatalf("season still offloaded: %+v", got.Seasons[0])
	}
	// Asking again is harmless: it answers the title's state.
	res, err = f.svc.Reacquire(ctx, ReacquireRequest{ItemID: show.ID, Scope: "season", Season: 1})
	if err != nil || res.Cleared || res.State != ReacquireSearching {
		t.Fatalf("second reacquire: %+v %v", res, err)
	}
}

func hasID(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func mustItem(t *testing.T, db *sqlite.DB, id int64) domain.MediaItem {
	t.Helper()
	item, err := db.GetMediaItemFull(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return item
}

// Any file attached to an offloaded wantable clears it in the store, whatever
// the path that attached it.
func TestOffloadClearsWhenAFileIsAttached(t *testing.T) {
	f := newOffloadFixture(t)
	m, _, _ := f.movieWithFiles(t)
	p := f.preview(t, OffloadRequest{ItemID: m.ID, Scope: "item"})
	f.wait(t, f.start(t, OffloadRequest{ItemID: m.ID, Scope: "item"}, p).ID)
	if !mustItem(t, f.db, m.ID).Offloaded() {
		t.Fatal("not offloaded")
	}
	f.file(t, m.ID, filepath.Join(m.Path, "Dune (2021) back.mkv"), "back", "")
	if mustItem(t, f.db, m.ID).Offloaded() {
		t.Fatal("a file attached but the movie is still offloaded")
	}

	show := mustItem(t, f.db, f.show)
	ep1, ep2 := show.Seasons[0].Episodes[0].ID, show.Seasons[0].Episodes[1].ID
	req := OffloadRequest{ItemID: show.ID, Scope: "season", Season: 1}
	p = f.preview(t, req)
	f.wait(t, f.start(t, req, p).ID)
	f.file(t, show.ID, filepath.Join(show.Path, "Season 1", "e1.mkv"), "e1", "", ep1)
	got := mustItem(t, f.db, show.ID)
	if got.Seasons[0].Episodes[0].Offloaded() || !got.Seasons[0].Episodes[1].Offloaded() || !got.Seasons[0].Offloaded() {
		t.Fatalf("one episode back: %+v", got.Seasons[0])
	}
	f.file(t, show.ID, filepath.Join(show.Path, "Season 1", "e2.mkv"), "e2", "", ep2)
	if got = mustItem(t, f.db, show.ID); got.Seasons[0].Offloaded() || got.Seasons[0].Episodes[1].Offloaded() {
		t.Fatalf("every episode back, season flag must clear: %+v", got.Seasons[0])
	}
}

// A new episode found in an offloaded season inherits offloaded.
func TestOffloadNewEpisodeInOffloadedSeasonInherits(t *testing.T) {
	f := newOffloadFixture(t)
	ctx := context.Background()
	show := mustItem(t, f.db, f.show)
	req := OffloadRequest{ItemID: show.ID, Scope: "season", Season: 1}
	f.wait(t, f.start(t, req, f.preview(t, req)).ID)
	if _, err := f.db.W.ExecContext(ctx, `INSERT INTO episodes(media_item_id,season_number,episode_number,title,air_date,monitored) VALUES(?,?,?,?,?,1)`, show.ID, 1, 3, "New", "2020-02-01"); err != nil {
		t.Fatal(err)
	}
	got := mustItem(t, f.db, show.ID)
	if len(got.Seasons[0].Episodes) != 3 || !got.Seasons[0].Episodes[2].Offloaded() {
		t.Fatalf("new episode: %+v", got.Seasons[0].Episodes)
	}
	if ids := wantedIDs(t, f.svc); hasID(ids, "episode:"+itoa(show.ID)+":1:3") {
		t.Fatalf("an inherited-offloaded episode is wanted: %v", ids)
	}
}

// An episode of a multi-episode file widens the scope to every episode in the
// file, and the preview says so.
func TestOffloadMultiEpisodeFileWidensScope(t *testing.T) {
	f := newOffloadFixture(t)
	show := mustItem(t, f.db, f.show)
	ep1, ep2 := show.Seasons[0].Episodes[0].ID, show.Seasons[0].Episodes[1].ID
	file := filepath.Join(show.Path, "Season 1", "Test Show - S01E01-E02.mkv")
	f.file(t, show.ID, file, "double", "", ep1, ep2)
	req := OffloadRequest{ItemID: show.ID, Scope: "episode", EpisodeID: ep1}
	p := f.preview(t, req)
	if len(p.Widened) != 1 || p.Widened[0] != "S01E02" || len(p.Episodes) != 2 {
		t.Fatalf("preview did not widen: %+v", p)
	}
	f.wait(t, f.start(t, req, p).ID)
	got := mustItem(t, f.db, show.ID)
	if !got.Seasons[0].Episodes[0].Offloaded() || !got.Seasons[0].Episodes[1].Offloaded() {
		t.Fatalf("scope not widened: %+v", got.Seasons[0])
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("the shared file is still there: %v", err)
	}
}

// A crash mid-delete resumes from the journal: the rest is deleted, the
// already-deleted files (ENOENT) are fine, and the operation completes.
func TestOffloadResumesAfterCrashMidDelete(t *testing.T) {
	f := newOffloadFixture(t)
	ctx := context.Background()
	m, video, sub := f.movieWithFiles(t)
	p := f.preview(t, OffloadRequest{ItemID: m.ID, Scope: "item"})
	// Record the operation exactly as Start does, but do not run it: the
	// process "crashed" after deleting the video's bytes but before the rest.
	req := OffloadRequest{ItemID: m.ID, Scope: "item", RequestID: "req-crash", PlanFingerprint: p.Fingerprint}
	op, err := f.svc.startOffloadLocked(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(video); err != nil {
		t.Fatal(err)
	}
	f.svc.driveOffloads(ctx)
	st := f.wait(t, op.ID)
	if st.State != "complete" || st.FilesDone != 2 {
		t.Fatalf("resume: %+v", st)
	}
	if _, err := os.Stat(sub); !os.IsNotExist(err) {
		t.Fatalf("sidecar survived: %v", err)
	}
	if n := len(mustItem(t, f.db, m.ID).Files); n != 0 {
		t.Fatalf("library rows left: %d", n)
	}
}

// An unmounted share (the library root lists empty) makes the operation wait
// with a plain reason and mark nothing deleted; it resumes when the share is back.
func TestOffloadWaitsWhileTheShareIsNotMounted(t *testing.T) {
	f := newOffloadFixture(t)
	ctx := context.Background()
	m, video, sub := f.movieWithFiles(t)
	p := f.preview(t, OffloadRequest{ItemID: m.ID, Scope: "item"})
	req := OffloadRequest{ItemID: m.ID, Scope: "item", RequestID: "req-mount", PlanFingerprint: p.Fingerprint}
	op, err := f.svc.startOffloadLocked(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	// "Unmount": the share's contents vanish from under the mount point.
	hidden := f.root.Path + ".hidden"
	if err := os.Rename(f.root.Path, hidden); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.root.Path, 0o755); err != nil {
		t.Fatal(err)
	}
	f.svc.driveOffloads(ctx)
	st := f.waitState(t, op.ID, "waiting")
	if !strings.Contains(st.LastError, "unmounted") || st.FilesDone != 0 {
		t.Fatalf("waiting: %+v", st)
	}
	files, _ := f.db.ListOffloadFiles(ctx, op.ID)
	for _, fl := range files {
		if fl.State != "pending" {
			t.Fatalf("a file was marked %s while the share was away: %+v", fl.State, fl)
		}
	}
	// The preview says so too.
	if q, err := f.svc.PreviewOffload(ctx, OffloadRequest{ItemID: m.ID, Scope: "item"}); err == nil && q.Blocked == nil {
		t.Fatalf("a second preview must be refused while the operation is open: %+v", q)
	}
	// The share comes back.
	if err := os.Remove(f.root.Path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(hidden, f.root.Path); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	f.svc.driveOffloads(ctx)
	st = f.wait(t, op.ID)
	if st.State != "complete" {
		t.Fatalf("after the share came back: %+v", st)
	}
	for _, gone := range []string{video, sub} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatalf("%s: %v", gone, err)
		}
	}
}

// An unlink that fails is an operation state with its reason, retried; the row
// stays until the bytes are gone; the operation completes once it works.
func TestOffloadUnlinkFailureIsAStateAndRetried(t *testing.T) {
	f := newOffloadFixture(t)
	ctx := context.Background()
	m, video, _ := f.movieWithFiles(t)
	p := f.preview(t, OffloadRequest{ItemID: m.ID, Scope: "item"})
	req := OffloadRequest{ItemID: m.ID, Scope: "item", RequestID: "req-unlink", PlanFingerprint: p.Fingerprint}
	op, err := f.svc.startOffloadLocked(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	// Permission fixtures pass as root, so make the unlink itself fail: the
	// path is swapped for a non-empty directory (ENOTEMPTY), then swapped back.
	held := video + ".held"
	if err := os.Rename(video, held); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(video, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(video, "x"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.svc.driveOffloads(ctx)
	st := f.waitState(t, op.ID, "retrying")
	if !strings.Contains(st.LastError, "could not be deleted") || len(st.Failed) == 0 {
		t.Fatalf("retrying: %+v", st)
	}
	if n := len(mustItem(t, f.db, m.ID).Files); n != 1 {
		t.Fatalf("the row must stay while the bytes do: %d", n)
	}
	if err := os.RemoveAll(video); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(held, video); err != nil {
		t.Fatal(err)
	}
	f.svc.driveOffloads(ctx)
	if st = f.wait(t, op.ID); st.State != "complete" {
		t.Fatalf("after the fix: %+v", st)
	}
}

// Stop ends the remaining deletes: what is deleted stays deleted and Cinema is
// told only that; a title that still has a file is no longer offloaded.
func TestOffloadStopKeepsWhatIsNotYetDeleted(t *testing.T) {
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
	p := f.preview(t, req)
	// Record without running; delete only e1, as an interrupted run would.
	req.RequestID, req.PlanFingerprint = "req-stop", p.Fingerprint
	op, err := f.svc.startOffloadLocked(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.offloadDeleteFile(ctx, offloadStored{}, sqlite.OffloadFile{Path: e1, Kind: "media", FileID: fileIDOf(t, f, e1)}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.MarkOffloadFile(ctx, op.ID, e1, "deleted", "", 3); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.StopOffload(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	st, _ := f.svc.GetOffload(ctx, op.ID)
	if st.State != "cancelled" || st.Stoppable {
		t.Fatalf("stopped: %+v", st)
	}
	if _, err := os.Stat(e2); err != nil {
		t.Fatalf("a remaining file was deleted after Stop: %v", err)
	}
	got := mustItem(t, f.db, show.ID)
	if !got.Seasons[0].Episodes[0].Offloaded() || got.Seasons[0].Episodes[1].Offloaded() || !got.Seasons[0].Offloaded() {
		t.Fatalf("after Stop: %+v", got.Seasons[0])
	}
	dels, _ := f.db.OperationDeliveries(ctx, op.ID)
	if len(dels) != 1 || dels[0].Status != "pending" || !strings.Contains(dels[0].Payload, e1) || strings.Contains(dels[0].Payload, e2) {
		t.Fatalf("cinema must be told only what was deleted: %+v", dels)
	}
	// And Stop cannot be repeated.
	if err := f.svc.StopOffload(ctx, op.ID); !errors.Is(err, ErrInvalidOffload) {
		t.Fatalf("second stop: %v", err)
	}
}

func fileIDOf(t *testing.T, f offloadFixture, path string) int64 {
	t.Helper()
	var id int64
	if err := f.db.R.QueryRowContext(context.Background(), `SELECT id FROM media_files WHERE path=?`, path).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// Re-acquire and Offload serialise on the item's per-copy reservation, and an
// Offload refuses while a download is in flight for its scope.
func TestOffloadSerialisesWithReacquireAndFencesDownloads(t *testing.T) {
	f := newOffloadFixture(t)
	ctx := context.Background()
	m, _, _ := f.movieWithFiles(t)
	unlock, err := f.svc.reserveItem(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := f.svc.Reacquire(ctx, ReacquireRequest{ItemID: m.ID, Scope: "item"})
		done <- err
	}()
	select {
	case <-done:
		t.Fatal("Re-acquire ran while the item's reservation was held")
	case <-time.After(150 * time.Millisecond):
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// A download in flight blocks the preview.
	if _, err := f.db.W.ExecContext(ctx, `INSERT INTO downloads(media_item_id,protocol,release_title,state,client_id,added_at,updated_at) VALUES(?,?,?,?,?,?,?)`, m.ID, "torrent", "Dune.2021", "downloading", 1, time.Now().UnixMilli(), time.Now().UnixMilli()); err != nil {
		t.Fatalf("insert download: %v", err)
	}
	p := f.preview(t, OffloadRequest{ItemID: m.ID, Scope: "item"})
	if p.Blocked == nil || p.Blocked.Reason != "download" {
		t.Fatalf("download not fenced: %+v", p.Blocked)
	}
	req := OffloadRequest{ItemID: m.ID, Scope: "item", RequestID: "r", PlanFingerprint: p.Fingerprint}
	var blocked *OffloadBlockedError
	if _, err := f.svc.StartOffload(ctx, req); !errors.As(err, &blocked) {
		t.Fatalf("start must be refused: %v", err)
	}
}

// Re-acquire of an offloaded scope cancels the Offload's unconfirmed Cinema
// requests, and an open Offload refuses it.
func TestReacquireCancelsUnconfirmedOffloadRequests(t *testing.T) {
	f := newOffloadFixture(t)
	ctx := context.Background()
	f.cinema(t, ports.FeatureRemovedPaths, ports.FeatureOffload)
	m, _, _ := f.movieWithFiles(t)
	p := f.preview(t, OffloadRequest{ItemID: m.ID, Scope: "item"})
	op := f.start(t, OffloadRequest{ItemID: m.ID, Scope: "item"}, p)
	f.wait(t, op.ID)
	if _, err := f.svc.Reacquire(ctx, ReacquireRequest{ItemID: m.ID, Scope: "item"}); err != nil {
		t.Fatal(err)
	}
	dels, _ := f.db.OperationDeliveries(ctx, op.ID)
	if len(dels) != 1 || dels[0].Status != "failed" || !strings.HasPrefix(dels[0].LastError, "cancelled") {
		t.Fatalf("delivery not cancelled: %+v", dels)
	}
	got := mustItem(t, f.db, m.ID)
	if got.Offloaded() || !got.Monitored {
		t.Fatalf("after re-acquire: %+v", got)
	}
}

// The status panel shows the Offload as a block, ending in the final stage
// "Offloaded <date> · <bytes> freed", and Done does not claim the panel can
// collapse while it is deleting.
func TestOffloadShowsInTheStatusPanel(t *testing.T) {
	f := newOffloadFixture(t)
	ctx := context.Background()
	m, _, _ := f.movieWithFiles(t)
	p := f.preview(t, OffloadRequest{ItemID: m.ID, Scope: "item"})
	op := f.start(t, OffloadRequest{ItemID: m.ID, Scope: "item"}, p)
	f.wait(t, op.ID)
	st, err := f.svc.ItemStatus(ctx, m.ID, -1)
	if err != nil {
		t.Fatal(err)
	}
	if st.Offload == nil || !st.Offload.Ended || st.Offload.State != "complete" ||
		!strings.HasPrefix(st.Offload.Headline, "Offloaded ") || !strings.HasSuffix(st.Offload.Headline, " freed") {
		t.Fatalf("status block = %+v", st.Offload)
	}
	if err := f.svc.DismissOffload(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	if st, _ = f.svc.ItemStatus(ctx, m.ID, -1); st.Offload != nil {
		t.Fatalf("a dismissed Offload is still on the panel: %+v", st.Offload)
	}
}

// Stop is a state the worker finishes: a crash after the flip leaves a
// `stopping` operation that the next pass completes (flags, history and the
// Cinema outbox in one transaction), and episodes that had no file (nothing of
// them was deleted) are un-offloaded.
func TestOffloadStopIsResumableAndUnoffloadsFilelessEpisodes(t *testing.T) {
	f := newOffloadFixture(t)
	ctx := context.Background()
	f.cinema(t, ports.FeatureRemovedPaths, ports.FeatureOffload)
	show := mustItem(t, f.db, f.show)
	ep1, ep2 := show.Seasons[0].Episodes[0].ID, show.Seasons[0].Episodes[1].ID
	e1 := filepath.Join(show.Path, "Season 1", "e1.mkv")
	f.file(t, show.ID, e1, "one", "", ep1)
	req := OffloadRequest{ItemID: show.ID, Scope: "season", Season: 1}
	p := f.preview(t, req)
	req.RequestID, req.PlanFingerprint = "req-stop2", p.Fingerprint
	op, err := f.svc.startOffloadLocked(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.offloadDeleteFile(ctx, offloadStored{}, sqlite.OffloadFile{Path: e1, Kind: "media", FileID: fileIDOf(t, f, e1)}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.MarkOffloadFile(ctx, op.ID, e1, "deleted", "", 3); err != nil {
		t.Fatal(err)
	}
	// The process dies right after the flip.
	if ok, err := f.db.RequestStopOffload(ctx, op.ID); err != nil || !ok {
		t.Fatalf("flip: %v %v", ok, err)
	}
	if id, _ := f.db.OpenOffloadForItem(ctx, show.ID); id != op.ID {
		t.Fatalf("a stopping operation still holds the title: %q", id)
	}
	f.svc.driveOffloads(ctx)
	st := f.wait(t, op.ID)
	if st.State != "cancelled" {
		t.Fatalf("resumed stop: %+v", st)
	}
	got := mustItem(t, f.db, show.ID)
	if !got.Seasons[0].Episodes[0].Offloaded() {
		t.Fatalf("the episode whose file was deleted must stay offloaded: %+v", got.Seasons[0])
	}
	if got.Seasons[0].Episodes[1].Offloaded() || got.Seasons[0].Episodes[1].ID != ep2 {
		t.Fatalf("a fileless episode must be un-offloaded by Stop: %+v", got.Seasons[0])
	}
	hist, _ := f.db.ListHistory(ctx)
	found := false
	for _, h := range hist {
		found = found || h.Type == HistoryOffloaded
	}
	if !found {
		t.Fatalf("a stop that deleted a file writes its history: %+v", hist)
	}
	dels, _ := f.db.OperationDeliveries(ctx, op.ID)
	pending := 0
	for _, d := range dels {
		if d.Status == "pending" {
			pending++
			if d.NextAt >= 1<<59 {
				t.Fatalf("the kept delivery must be released: %+v", d)
			}
		}
	}
	if pending != 1 {
		t.Fatalf("deliveries: %+v", dels)
	}
}
