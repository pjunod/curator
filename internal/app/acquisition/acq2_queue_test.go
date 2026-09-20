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
	"github.com/pjunod/monarr/internal/domain/mediainfo"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// unreachableClient answers nothing: the shape of a download client whose host
// is down, its API key rotated, or its container restarting.
type unreachableClient struct {
	fakeClient
	err error
}

func (c *unreachableClient) Statuses(context.Context) ([]ports.DownloadStatus, error) {
	c.mu.Lock()
	c.polls++
	c.mu.Unlock()
	return nil, c.err
}

// The bus event names are a wire contract: notifiers, the media-server hook and
// the SSE stream all subscribe by string. Renaming one silently unsubscribes
// every consumer, which looks exactly like "notifications stopped working".
func TestAcq2EventTypesAreTheWireNames(t *testing.T) {
	if got := (ReleaseGrabbed{}).EventType(); got != "release.grabbed" {
		t.Errorf("ReleaseGrabbed = %q", got)
	}
	if got := (ImportCompleted{}).EventType(); got != "import.completed" {
		t.Errorf("ImportCompleted = %q", got)
	}
	if got := (ImportFailed{}).EventType(); got != "import.failed" {
		t.Errorf("ImportFailed = %q", got)
	}
}

// A failed poll must record WHY without moving the contact clock: the clock
// means "last time this worked", and advancing it on a failure would make a
// client that has failed every 30 seconds for an hour look freshly contacted.
func TestAcq2AFailedPollRecordsTheReasonAndNotTheTime(t *testing.T) {
	broken := &unreachableClient{err: errors.New("dial tcp: connection refused")}
	svc, _, _ := setup(t, nil, &broken.fakeClient)
	svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return broken }
	ctx := context.Background()

	if err := svc.RefreshQueue(ctx); err != nil {
		t.Fatalf("one unreachable client failed the whole sweep: %v", err)
	}
	contacts := svc.Contacts()
	if len(contacts) != 1 {
		t.Fatalf("contacts = %+v", contacts)
	}
	for id, c := range contacts {
		if c.Error == "" {
			t.Errorf("client %d: no reason recorded for the failure", id)
		}
		if !c.At.IsZero() {
			t.Errorf("client %d: a failure advanced the last-worked clock to %v", id, c.At)
		}
	}
}

// Every enabled client is polled on every tick, even with nothing in flight,
// and the queue reconciler must survive a client that answers with an error.
func TestAcq2QueueAndRemoveDownloadDriveTheClient(t *testing.T) {
	client := &fakeClient{}
	svc, _, itemID := setup(t, nil, client)
	ctx := context.Background()

	id, err := svc.Grab(ctx, GrabRequest{
		MediaItemID: itemID, Season: 1, Episode: 1,
		Title: "Test.Show.S01E01.1080p.WEB-DL-QUEUE", DownloadURL: "m",
		Indexer: "idx", Protocol: "torrent",
	})
	if err != nil {
		t.Fatal(err)
	}

	rows, err := svc.Queue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != id {
		t.Fatalf("queue = %+v", rows)
	}

	// Removing without touching the client leaves the download running there —
	// that is the whole point of the flag.
	if err := svc.RemoveDownload(ctx, id, false); err != nil {
		t.Fatal(err)
	}
	if len(client.removed) != 0 {
		t.Errorf("the client was asked to remove it anyway: %+v", client.removed)
	}
	if rows, _ := svc.Queue(ctx); len(rows) != 0 {
		t.Errorf("the row survived removal: %+v", rows)
	}

	// …and with the flag, the client is told.
	id2, err := svc.Grab(ctx, GrabRequest{
		MediaItemID: itemID, Season: 1, Episode: 2,
		Title: "Test.Show.S01E02.1080p.WEB-DL-QUEUE", DownloadURL: "m2",
		Indexer: "idx", Protocol: "torrent",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RemoveDownload(ctx, id2, true); err != nil {
		t.Fatal(err)
	}
	if len(client.removed) != 1 || client.removed[0].Handle != "h1" {
		t.Errorf("client removals = %+v", client.removed)
	}
	if client.removed[0].DeleteData {
		t.Error("removing a queue row must not delete the client's data by default")
	}

	if err := svc.RemoveDownload(ctx, 4242, false); err == nil {
		t.Error("removing a download that does not exist reported success")
	}
}

// Not every client returns the handle monarr stored (a magnet that resolved to
// a different hash, a job re-added by hand). The normalized title is the
// fallback that keeps those reconciling instead of sitting in the queue forever.
func TestAcq2MatchStatusFallsBackToTheNormalizedTitle(t *testing.T) {
	statuses := []ports.DownloadStatus{
		{Handle: "other", Name: "Something.Else"},
		{Handle: "h9", Name: "test_show-s01e01 1080P.web-dl"},
	}
	dl := sqlite.Download{ReleaseTitle: "Test.Show.S01E01.1080p.WEB-DL"}
	got, ok := matchStatus(dl, statuses)
	if !ok || got.Handle != "h9" {
		t.Errorf("title fallback = %+v ok=%v", got, ok)
	}

	// A stored handle always wins over the name.
	dl.Handle = "other"
	got, ok = matchStatus(dl, statuses)
	if !ok || got.Handle != "other" {
		t.Errorf("handle match = %+v ok=%v", got, ok)
	}

	// Nothing matching is not an error — a magnet still resolving is invisible.
	if _, ok := matchStatus(sqlite.Download{ReleaseTitle: "Nobody.Has.This"}, statuses); ok {
		t.Error("matched a status that is not ours")
	}
}

// Interactive search has to say WHY it cannot search, because every one of
// these is something the user has to go and fix.
func TestAcq2SearchReportsWhatItCouldNotResolve(t *testing.T) {
	svc, db, itemID := setup(t, nil, &fakeClient{})
	ctx := context.Background()

	if _, err := svc.Search(ctx, 98765, 1, 1); err == nil {
		t.Error("searched for an item that does not exist")
	}
	if _, err := svc.Search(ctx, itemID, 9, 0); err == nil {
		t.Error("searched a season the show does not have")
	}
	if _, err := svc.Search(ctx, itemID, 1, 9); err == nil {
		t.Error("searched an episode the season does not have")
	}

	// A profile that has been deleted out from under the item.
	gone := int64(4242)
	if err := db.BulkUpdateItem(ctx, itemID, nil, &gone); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Search(ctx, itemID, 1, 1); err == nil {
		t.Error("searched under a profile that does not exist")
	}
	back := int64(1)
	if err := db.BulkUpdateItem(ctx, itemID, nil, &back); err != nil {
		t.Fatal(err)
	}

	// A DISABLED indexer is not an indexer, so this is still "none configured"
	// — the answer the UI turns into "enable one first".
	indexers, _ := db.ListIndexers(ctx)
	for _, ix := range indexers {
		if err := db.DeleteIndexer(ctx, ix.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.AddIndexer(ctx, ports.IndexerConfig{Name: "off", URL: "http://z",
		Protocol: "torrent", Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Search(ctx, itemID, 1, 1); !errors.Is(err, ErrNoIndexers) {
		t.Errorf("err = %v, want ErrNoIndexers", err)
	}
}

// One indexer failing must not empty the candidate list, and the same release
// offered by one indexer twice must appear once — a duplicated row in the
// interactive list is a second chance to grab the same thing.
func TestAcq2SearchSurvivesAFailedIndexerAndDeduplicates(t *testing.T) {
	dup := ports.Release{
		Title: "Test.Show.S01E01.1080p.WEB-DL.x264-DUP", DownloadURL: "u",
		Protocol: "torrent", Indexer: "idx", Seeders: 5,
	}
	svc, db, itemID := setup(t, []ports.Release{dup, dup}, &fakeClient{})
	ctx := context.Background()
	if _, err := db.AddIndexer(ctx, ports.IndexerConfig{Name: "broken", URL: "http://y",
		Protocol: "torrent", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	svc.newIndexer = func(cfg ports.IndexerConfig) ports.Indexer {
		if cfg.Name == "broken" {
			return &recordingIndexer{fail: errors.New("503")}
		}
		return &recordingIndexer{releases: []ports.Release{dup, dup}}
	}

	got, err := svc.Search(ctx, itemID, 1, 1)
	if err != nil {
		t.Fatalf("a failing indexer emptied the search: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("candidates = %d, want one deduplicated row: %+v", len(got), got)
	}
}

// The same quality is the common case, so the tie-breaks decide what the user
// sees at the top of the list: custom-format score, then seeders.
func TestAcq2SearchRanksTiesOnScoreThenSeeders(t *testing.T) {
	svc, _, itemID := setup(t, []ports.Release{
		{Title: "Test.Show.S01E01.1080p.WEB-DL-LOW", DownloadURL: "a", Indexer: "idx",
			Protocol: "torrent", Seeders: 2},
		{Title: "Test.Show.S01E01.1080p.WEB-DL-HIGH", DownloadURL: "b", Indexer: "idx",
			Protocol: "torrent", Seeders: 200},
	}, &fakeClient{})

	got, err := svc.Search(context.Background(), itemID, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("candidates = %+v", got)
	}
	if got[0].Release.Seeders != 200 {
		t.Errorf("seeder tie-break put %d seeders first", got[0].Release.Seeders)
	}
}

// A size that cannot hold the claim is worth saying out loud even on a release
// the profile would take — as a warning, never a rejection. Gating a manual
// grab is something monarr has never done.
func TestAcq2SearchWarnsButDoesNotRejectAnImplausibleSize(t *testing.T) {
	svc, db, itemID := setup(t, []ports.Release{{
		Title: "Test.Show.S01E01.1080p.BluRay-RUNT", DownloadURL: "u", Indexer: "idx",
		Protocol: "torrent", Seeders: 9, Size: 4 << 20,
	}}, &fakeClient{})
	ctx := context.Background()
	item, _ := db.GetMediaItemFull(ctx, itemID)
	item.Runtime = 60
	if err := db.UpdateMediaItemMetadata(ctx, itemID, item); err != nil {
		t.Fatal(err)
	}

	got, err := svc.Search(ctx, itemID, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("candidates = %+v", got)
	}
	if got[0].Warning == "" {
		t.Error("no warning on a 4 MB '1080p BluRay'")
	}
	if !got[0].Accepted {
		t.Error("the size warning declined the release; it must only caution")
	}
}

// Age is what a person reads to decide whether a release is worth waiting on,
// so each band has to render in the unit that band is about.
func TestAcq2AgeRendersInTheRightUnit(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{20 * time.Minute, "20m"},
		{5 * time.Hour, "5h"},
		{47 * time.Hour, "47h"},
		{72 * time.Hour, "3d"},
	}
	for _, c := range cases {
		if got := age(c.d); got != c.want {
			t.Errorf("age(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

// "completed (event 12)" versus "completed (poll)" is the difference between
// knowing push is working and assuming it is. With no channel named, the
// detail must be left exactly as it was.
func TestAcq2TracedOnlyAppendsAChannelWhenThereIsOne(t *testing.T) {
	if got := traced("finished", ""); got != "finished" {
		t.Errorf("traced with no source = %q", got)
	}
	if got := traced("finished", "poll"); got != "finished (poll)" {
		t.Errorf("traced = %q", got)
	}
}

// Wantable ids are the durable link between a download row and the thing it was
// grabbed for. They survive restarts, so every shape has to round-trip — a
// misparse re-searches something already in flight, or worse, the wrong item.
func TestAcq2WantableIDsRoundTrip(t *testing.T) {
	svc, db, itemID := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	copyID, err := db.AddMediaCopy(ctx, domain.MediaCopy{
		MediaItemID: itemID, QualityProfileID: 1, Path: t.TempDir(), Monitored: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	ep, err := svc.wantableFromID(ctx, "episode:"+itoa(itemID)+":1:2")
	if err != nil {
		t.Fatalf("episode id: %v", err)
	}
	if e, ok := ep.(domain.EpisodeWantable); !ok || e.Season != 1 || e.Episode != 2 {
		t.Errorf("episode wantable = %+v", ep)
	}
	pack, err := svc.wantableFromID(ctx, "season:"+itoa(itemID)+":1")
	if err != nil {
		t.Fatalf("season id: %v", err)
	}
	if s, ok := pack.(domain.SeasonWantable); !ok || s.Season != 1 {
		t.Errorf("season wantable = %+v", pack)
	}
	// The ":c<n>" suffix pins it to a copy, and the copy carries its own name.
	withCopy, err := svc.wantableFromID(ctx, "season:"+itoa(itemID)+":1:c"+itoa(copyID))
	if err != nil {
		t.Fatalf("copy-suffixed id: %v", err)
	}
	if domain.WantableCopy(withCopy) != copyID {
		t.Errorf("copy id lost in the round trip: %+v", withCopy)
	}
	if domain.WantableCopyName(withCopy) != "copy "+itoa(copyID) {
		t.Errorf("unnamed copy label = %q, want \"copy N\"", domain.WantableCopyName(withCopy))
	}

	if _, err := svc.wantableFromID(ctx, "nonsense"); err == nil {
		t.Error("an unparseable wantable id was accepted")
	}
	if _, err := svc.wantableFromID(ctx, "movie:99999"); err == nil {
		t.Error("a wantable for an item that does not exist was accepted")
	}
	if _, err := svc.wantableFromID(ctx, "season:"+itoa(itemID)+":1:c9999"); err == nil {
		t.Error("a wantable for a copy that does not exist was accepted")
	}
}

// A movie id resolves to a movie wantable, and a book id to a book one — the
// two share a parse branch and getting them confused would search the wrong way.
func TestAcq2WantableIDsResolveMoviesAndBooks(t *testing.T) {
	svc, _, movieID := autoSetup(t, nil, &fakeClient{})
	ctx := context.Background()
	w, err := svc.wantableFromID(ctx, "movie:"+itoa(movieID))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := w.(domain.MovieWantable); !ok {
		t.Errorf("movie id resolved to %T", w)
	}
	if _, err := svc.wantableFromID(ctx, "book:"+itoa(movieID)); !errors.Is(err, ErrNotFound) {
		t.Errorf("book id for movie = %v, want ErrNotFound", err)
	}

	bsvc, _, bookID := bookSetup(t, nil, &fakeClient{})
	b, err := bsvc.wantableFromID(context.Background(), "book:"+itoa(bookID))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := b.(domain.BookWantable); !ok {
		t.Errorf("book id resolved to %T", b)
	}
	if _, err := bsvc.wantableFromID(context.Background(), "movie:"+itoa(bookID)); !errors.Is(err, ErrNotFound) {
		t.Errorf("movie id for book = %v, want ErrNotFound", err)
	}
}

// Grab has to refuse rather than guess when it cannot resolve the item or find
// a client that speaks the release's protocol. Guessing a client sends a
// torrent to a usenet daemon and looks like a broken download for hours.
func TestAcq2GrabRefusesWhatItCannotResolve(t *testing.T) {
	svc, _, itemID := setup(t, nil, &fakeClient{})
	ctx := context.Background()

	if _, err := svc.Grab(ctx, GrabRequest{MediaItemID: 5150, Protocol: "torrent"}); err == nil {
		t.Error("grabbed for an item that does not exist")
	}
	_, err := svc.Grab(ctx, GrabRequest{
		MediaItemID: itemID, Season: 1, Episode: 1, Protocol: "usenet",
		Title: "Test.Show.S01E01.1080p.WEB-DL", DownloadURL: "nzb",
	})
	if !errors.Is(err, ErrNoClient) {
		t.Errorf("err = %v, want ErrNoClient — no usenet client is configured", err)
	}
}

// A download deleted in the client is an instruction, not a fault, and it is
// reported even when the client says nothing about why.
func TestAcq2ARemovalWithNoMessageStillGetsAReason(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setup(t, nil, client)
	ctx := context.Background()
	id, err := svc.Grab(ctx, GrabRequest{
		MediaItemID: itemID, Season: 1, Episode: 1,
		Title: "Test.Show.S01E01.1080p.WEB-DL-GONE", DownloadURL: "m",
		Indexer: "idx", Protocol: "torrent",
	})
	if err != nil {
		t.Fatal(err)
	}
	client.statuses = []ports.DownloadStatus{{Handle: "h1", State: ports.StateRemoved}}
	if err := svc.RefreshQueue(ctx); err != nil {
		t.Fatal(err)
	}

	got, _ := db.GetDownload(ctx, id)
	if got.State != "failed" {
		t.Fatalf("state = %q", got.State)
	}
	if !strings.Contains(got.Error, "removed in the download client") {
		t.Errorf("reason = %q, want a default the user can read", got.Error)
	}
	if blocked, _ := db.IsBlocklisted(ctx, "Test.Show.S01E01.1080p.WEB-DL-GONE", "idx"); blocked {
		t.Error("a deletion blocklisted the release")
	}
}

// A download that has already imported is finished. A late failure or removal
// event arriving afterwards — clients do re-report — must not walk it back to
// 'failed' and start re-searching for something already in the library.
func TestAcq2LateFailureEventsCannotUndoAnImport(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setup(t, nil, client)
	ctx := context.Background()
	release := "Test.Show.S01E01.1080p.WEB-DL-DONE"
	id, err := svc.Grab(ctx, GrabRequest{
		MediaItemID: itemID, Season: 1, Episode: 1,
		Title: release, DownloadURL: "m", Indexer: "idx", Protocol: "torrent",
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := t.TempDir()
	if err := os.WriteFile(filepath.Join(payload, release+".mkv"), []byte("v"), 0o644); err != nil {
		t.Fatal(err)
	}
	client.statuses = []ports.DownloadStatus{{Handle: "h1", Name: release,
		State: ports.StateCompleted, Progress: 1, SavePath: payload}}
	if err := svc.RefreshQueue(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.GetDownload(ctx, id); got.State != "imported" {
		t.Fatalf("setup did not import: %q %q", got.State, got.Error)
	}

	for _, late := range []ports.DownloadState{ports.StateFailed, ports.StateRemoved} {
		cfg, _ := db.GetDownloadClient(ctx, 1)
		dl, _ := db.GetDownload(ctx, id)
		svc.reconcileDownload(ctx, dl, cfg, ports.DownloadStatus{
			Handle: "h1", State: late, Message: "late news",
		}, "poll")
		got, _ := db.GetDownload(ctx, id)
		if got.State != "imported" {
			t.Fatalf("a late %v event moved an imported download to %q", late, got.State)
		}
	}
	if blocked, _ := db.IsBlocklisted(ctx, release, "idx"); blocked {
		t.Error("a late failure blocklisted a release that already imported")
	}
}

// A failure with no indexers left to search is still a failure: it must be
// recorded and blocklisted, it just cannot be replaced.
func TestAcq2AFailureWithNoIndexersStillBlocklists(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setup(t, nil, client)
	ctx := context.Background()
	release := "Test.Show.S01E01.1080p.WEB-DL-BAD"
	id, err := svc.Grab(ctx, GrabRequest{
		MediaItemID: itemID, Season: 1, Episode: 1,
		Title: release, DownloadURL: "m", Indexer: "idx", Protocol: "torrent",
	})
	if err != nil {
		t.Fatal(err)
	}
	indexers, _ := db.ListIndexers(ctx)
	for _, ix := range indexers {
		if err := db.DeleteIndexer(ctx, ix.ID); err != nil {
			t.Fatal(err)
		}
	}
	client.statuses = []ports.DownloadStatus{{Handle: "h1", State: ports.StateFailed, Message: "corrupt"}}
	if err := svc.RefreshQueue(ctx); err != nil {
		t.Fatal(err)
	}

	got, _ := db.GetDownload(ctx, id)
	if got.State != "failed" {
		t.Errorf("state = %q", got.State)
	}
	if blocked, _ := db.IsBlocklisted(ctx, release, "idx"); !blocked {
		t.Error("the failed release was not blocklisted")
	}
}

// A copy with no name still needs one to show, and a specific episode that does
// not exist has to be refused rather than silently resolving to something else.
func TestAcq2TargetsNameCopiesAndRefuseUnknownEpisodes(t *testing.T) {
	svc, db, itemID := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	item, _ := db.GetMediaItemFull(ctx, itemID)

	if _, err := svc.target(ctx, item, 1, 42); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound for an episode that does not exist", err)
	}
	unnamed := domain.MediaCopy{ID: 7}
	if got := copyLabel(unnamed); got != "copy 7" {
		t.Errorf("copyLabel = %q, want a fallback name", got)
	}
	if got := copyLabel(domain.MediaCopy{ID: 7, Name: "for dad"}); got != "for dad" {
		t.Errorf("copyLabel = %q", got)
	}
}

// A file monarr measured and decided not to believe does not hold an episode.
// This is the ADR 0013 rule: HasFile exists to stop monarr replacing a file it
// merely could not READ, and an actively disbelieved measurement is not that.
func TestAcq2ADisbelievedFileDoesNotHoldItsEpisode(t *testing.T) {
	svc, db, itemID := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	item, _ := db.GetMediaItemFull(ctx, itemID)

	dir := filepath.Join(item.Path, "Season 1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "Test Show - S01E01 - Pilot [WEB-DL 1080p].mkv")
	if err := os.WriteFile(path, []byte("stump"), 0o644); err != nil {
		t.Fatal(err)
	}
	fid, err := db.UpsertFile(ctx, itemID, 0, path, 5)
	if err != nil {
		t.Fatal(err)
	}
	ep1, _ := db.GetEpisodeID(ctx, itemID, 1, 1)
	if err := db.ReplaceFileEpisodeLinks(ctx, fid, []int64{ep1}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetFileQualityFrom(ctx, fid,
		quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080},
		mediainfo.ProvenanceImplausible, mediainfo.ConfidenceNone); err != nil {
		t.Fatal(err)
	}

	states, err := svc.episodeStates(ctx, item, 0)
	if err != nil {
		t.Fatal(err)
	}
	if st := states[ep1]; st.HasFile {
		t.Errorf("a disbelieved file is holding its episode: %+v", st)
	}
}
