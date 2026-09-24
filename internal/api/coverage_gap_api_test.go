package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apigen "github.com/pjunod/monarr/internal/api/gen"
	"github.com/pjunod/monarr/internal/app/transfers"
	"github.com/pjunod/monarr/internal/domain"
)

// The handlers the wired harness could reach but nothing exercised: the
// Activity page's summary, paging and clear-finished; the history timeline;
// the data plane (/system/transfers); backups; the manual-import preview;
// automatic search; wanted-search result pages; manual aliases; and the
// saved-notifier test button.
//
// Every test asserts what the body SAYS as well as the status, and pins the
// refusal branches (bad ids → 404, bad input → 400, nothing there → an empty
// array rather than null), because those are the branches that rot when
// nothing reads them.

// ---- search → grab provenance ----

// A grab that omits `season` (a movie) keeps the evidence the search
// retained for its candidate token.
//
// Regression: the search issues a movie's token with season 0 (the query
// has no season) while the grab handler defaults an omitted season to -1.
// consumeCandidateToken compared them raw, so every whole-item grab from the
// candidate list was downgraded to "manual_override" — the queue row said
// the operator overrode a match the engine had in fact made.
func TestGapGrabWithoutSeasonKeepsRetainedSearchEvidence(t *testing.T) {
	e := newAPIEnv(t)
	acqAddIndexer(t, e)
	acqAddClient(t, e, "qbittorrent")
	item := e.addMovie(t)

	var cands []apigen.ReleaseCandidate
	e.get(t, "/api/v1/library/"+acqItoa(item)+"/releases").
		expect(t, http.StatusOK).into(t, &cands)
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want the one the indexer answers with: %+v", len(cands), cands)
	}
	c := cands[0]
	if c.CandidateToken == "" {
		t.Fatal("candidate carries no token; the grab cannot cite its search")
	}
	if !c.Match.Matched {
		t.Fatalf("precondition: the harness release must match its item; match = %+v", c.Match)
	}
	if c.Match.Method == nil || *c.Match.Method == "manual_override" {
		t.Fatalf("precondition: search evidence method = %v, want a real matcher method", c.Match.Method)
	}

	// No "season" field at all: the handler will default it to -1.
	rr := e.post(t, "/api/v1/grab", fmt.Sprintf(`{
		"mediaItemId":%d,"title":%q,"downloadUrl":%q,"protocol":%q,
		"indexer":%q,"size":%d,"candidateToken":%q}`,
		item, c.Title, c.DownloadUrl, c.Protocol, c.Indexer, c.Size, c.CandidateToken)).
		expect(t, http.StatusCreated)
	var grabbed struct {
		ID int64 `json:"id"`
	}
	rr.into(t, &grabbed)

	var queue []apigen.QueueItem
	e.get(t, "/api/v1/queue").expect(t, http.StatusOK).into(t, &queue)
	if len(queue) != 1 || queue[0].Id != grabbed.ID {
		t.Fatalf("queue = %+v, want the one row just grabbed (%d)", queue, grabbed.ID)
	}
	m := queue[0].Match
	if m == nil {
		t.Fatal("queue row carries no match evidence")
	}
	if !m.Matched {
		t.Errorf("matched = false; the retained search evidence said true: %+v", m)
	}
	if m.Method == nil || *m.Method != *c.Match.Method {
		t.Errorf("method = %v, want the search's %q (a grab without season must not degrade to manual_override)",
			m.Method, *c.Match.Method)
	}
	if m.Reason != c.Match.Reason {
		t.Errorf("reason = %q, want the search's %q", m.Reason, c.Match.Reason)
	}
}

// A grab whose token does not describe the release it names is recorded as
// a manual override, with the code that says the identity was never
// resolved. The token is provenance, not authorisation: a mismatch is not a
// refusal, it is a row that admits it was not searched for.
func TestGapGrabWithAForeignTokenIsRecordedAsManualOverride(t *testing.T) {
	e := newAPIEnv(t)
	acqAddIndexer(t, e)
	acqAddClient(t, e, "qbittorrent")
	item := e.addMovie(t)

	var cands []apigen.ReleaseCandidate
	e.get(t, "/api/v1/library/"+acqItoa(item)+"/releases").
		expect(t, http.StatusOK).into(t, &cands)
	if len(cands) != 1 {
		t.Fatalf("candidates = %+v", cands)
	}
	// Same token, different title: the record does not describe this grab.
	e.post(t, "/api/v1/grab", fmt.Sprintf(`{
		"mediaItemId":%d,"title":"Fight.Club.1999.2160p.WEB-DL.x265-OTHER",
		"downloadUrl":"http://indexer.invalid/other","protocol":"torrent",
		"indexer":"nzb.invalid","candidateToken":%q}`, item, cands[0].CandidateToken)).
		expect(t, http.StatusCreated)

	var queue []apigen.QueueItem
	e.get(t, "/api/v1/queue").expect(t, http.StatusOK).into(t, &queue)
	if len(queue) != 1 || queue[0].Match == nil {
		t.Fatalf("queue = %+v, want one row with evidence", queue)
	}
	m := queue[0].Match
	if m.Method == nil || *m.Method != "manual_override" {
		t.Errorf("method = %v, want manual_override for a token that names another release", m.Method)
	}
	if m.Code == nil || *m.Code != "identity_unresolved" {
		t.Errorf("code = %v, want identity_unresolved", m.Code)
	}
	if m.OriginalTitle == nil || *m.OriginalTitle != "Fight.Club.1999.2160p.WEB-DL.x265-OTHER" {
		t.Errorf("originalTitle = %v, want the grabbed title", m.OriginalTitle)
	}
	if m.ParsedTitle == nil || *m.ParsedTitle == "" {
		t.Errorf("parsedTitle = %v, want the parser's reading of the title", m.ParsedTitle)
	}
}

// ---- queue: summary, paging, clear finished ----

// gapMarkImported flips a queue row to imported straight in the store, so
// the finished-side handlers have a row without a download having landed.
func gapMarkImported(t *testing.T, e *apiEnv, id int64) {
	t.Helper()
	if err := e.db.UpdateDownloadState(context.Background(), id, "imported", 1, ""); err != nil {
		t.Fatal(err)
	}
}

// gapGrabTitled grabs a distinct release for the item, so a test can hold
// two rows in different states without the in-flight guard collapsing them.
func gapGrabTitled(t *testing.T, e *apiEnv, itemID int64, title string) int64 {
	t.Helper()
	rr := e.post(t, "/api/v1/grab", fmt.Sprintf(`{
		"mediaItemId":%d,"title":%q,"downloadUrl":%q,
		"protocol":"torrent","indexer":"nzb.invalid","size":8589934592}`,
		itemID, title, "http://indexer.invalid/"+url.PathEscape(title))).
		expect(t, http.StatusCreated)
	var out struct {
		ID int64 `json:"id"`
	}
	rr.into(t, &out)
	if out.ID == 0 {
		t.Fatalf("grab returned no queue id: %s", rr.Body.String())
	}
	return out.ID
}

// The summary counts every state, splits active from finished, and carries
// the retention window; clearing finished removes exactly the imported rows
// and reports how many.
func TestGapQueueSummaryAndClearFinished(t *testing.T) {
	e := newAPIEnv(t)
	acqAddClient(t, e, "qbittorrent")
	item := e.addMovie(t)

	// Empty: zero of everything, and still a retention window.
	var empty apigen.QueueSummary
	e.get(t, "/api/v1/queue/summary").expect(t, http.StatusOK).into(t, &empty)
	if empty.Total != 0 || empty.Active != 0 || len(empty.Counts) != 0 {
		t.Errorf("empty summary = %+v, want zeros", empty)
	}
	if empty.RetentionDays == nil {
		t.Error("retentionDays absent; the Activity page shows the window next to the counts")
	}

	done := gapGrabTitled(t, e, item, "Fight.Club.1999.1080p.WEB-DL.x264-DONE")
	gapMarkImported(t, e, done)
	gapGrabTitled(t, e, item, "Fight.Club.1999.1080p.WEB-DL.x264-LIVE")

	var sum apigen.QueueSummary
	e.get(t, "/api/v1/queue/summary").expect(t, http.StatusOK).into(t, &sum)
	if sum.Counts["imported"] != 1 || sum.Counts["grabbed"] != 1 {
		t.Errorf("counts = %v, want imported:1 grabbed:1", sum.Counts)
	}
	if sum.Total != 2 {
		t.Errorf("total = %d, want 2", sum.Total)
	}
	if sum.Active != 1 {
		t.Errorf("active = %d, want only the grabbed row", sum.Active)
	}

	var cleared apigen.ClearedCount
	e.del(t, "/api/v1/queue/finished").expect(t, http.StatusOK).into(t, &cleared)
	if cleared.Cleared != 1 {
		t.Errorf("cleared = %d, want the one imported row", cleared.Cleared)
	}

	var after apigen.QueueSummary
	e.get(t, "/api/v1/queue/summary").expect(t, http.StatusOK).into(t, &after)
	if after.Total != 1 || after.Counts["imported"] != 0 || after.Counts["grabbed"] != 1 {
		t.Errorf("summary after clear = %+v, want only the grabbed row left", after)
	}

	// Clearing again is a no-op that says so.
	e.del(t, "/api/v1/queue/finished").expect(t, http.StatusOK).into(t, &cleared)
	if cleared.Cleared != 0 {
		t.Errorf("second clear = %d, want 0", cleared.Cleared)
	}
}

// With a filter, a search or a page the queue is one page of one group;
// without any of them it is the whole recent list. The filtered views must
// agree with the summary's counts, or the section header lies about its
// contents.
func TestGapQueueFilterSearchAndPaging(t *testing.T) {
	e := newAPIEnv(t)
	acqAddClient(t, e, "qbittorrent")
	item := e.addMovie(t)
	done := gapGrabTitled(t, e, item, "Fight.Club.1999.1080p.WEB-DL.x264-DONE")
	gapMarkImported(t, e, done)
	live := gapGrabTitled(t, e, item, "Fight.Club.1999.1080p.WEB-DL.x264-LIVE")

	ids := func(path string) []int64 {
		t.Helper()
		var rows []apigen.QueueItem
		e.get(t, path).expect(t, http.StatusOK).into(t, &rows)
		out := make([]int64, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.Id)
		}
		return out
	}

	if got := ids("/api/v1/queue"); len(got) != 2 {
		t.Errorf("unfiltered queue = %v, want both rows", got)
	}
	if got := ids("/api/v1/queue?filter=active"); len(got) != 1 || got[0] != live {
		t.Errorf("filter=active = %v, want only the live row %d", got, live)
	}
	if got := ids("/api/v1/queue?filter=imported"); len(got) != 1 || got[0] != done {
		t.Errorf("filter=imported = %v, want only the imported row %d", got, done)
	}
	if got := ids("/api/v1/queue?filter=failed"); len(got) != 0 {
		t.Errorf("filter=failed = %v, want nothing", got)
	}
	if got := ids("/api/v1/queue?q=LIVE"); len(got) != 1 || got[0] != live {
		t.Errorf("q=LIVE = %v, want the row whose title carries it", got)
	}
	if got := ids("/api/v1/queue?q=nothing-is-called-this"); len(got) != 0 {
		t.Errorf("q=<miss> = %v, want an empty page", got)
	}
	if got := ids("/api/v1/queue?limit=1&offset=0"); len(got) != 1 {
		t.Errorf("limit=1 = %v, want one row", got)
	}
	if got := ids("/api/v1/queue?limit=1&offset=5"); len(got) != 0 {
		t.Errorf("offset past the end = %v, want an empty page", got)
	}
	// An empty page is an array, not null.
	body := strings.TrimSpace(e.get(t, "/api/v1/queue?filter=failed").expect(t, http.StatusOK).Body.String())
	if body != "[]" {
		t.Errorf("empty page body = %s, want []", body)
	}
}

// ---- history ----

// A grab writes a "grabbed" event, and /history hands it over with its data
// decoded as JSON. Filtering by an item nobody has is an empty array.
func TestGapHistoryListsTheGrab(t *testing.T) {
	e := newAPIEnv(t)
	acqAddClient(t, e, "qbittorrent")
	item := e.addMovie(t)

	body := strings.TrimSpace(e.get(t, "/api/v1/history").expect(t, http.StatusOK).Body.String())
	if body != "[]" {
		t.Errorf("history before any grab = %s, want []", body)
	}

	acqGrab(t, e, item)

	var events []apigen.HistoryEvent
	e.get(t, "/api/v1/history").expect(t, http.StatusOK).into(t, &events)
	if len(events) == 0 {
		t.Fatal("history is empty after a grab; the timeline has nothing to show")
	}
	var grabbed *apigen.HistoryEvent
	for i := range events {
		if events[i].Type == "grabbed" {
			grabbed = &events[i]
		}
	}
	if grabbed == nil {
		t.Fatalf("no grabbed event in %+v", events)
	}
	if grabbed.MediaItemId != item {
		t.Errorf("mediaItemId = %d, want %d", grabbed.MediaItemId, item)
	}
	if grabbed.ReleaseTitle != "Fight.Club.1999.1080p.WEB-DL.x264-TEST" {
		t.Errorf("releaseTitle = %q, want the grabbed release", grabbed.ReleaseTitle)
	}
	if grabbed.Ts.IsZero() {
		t.Error("ts is zero; a timeline needs a time")
	}
	if grabbed.Data == nil {
		t.Fatal("data absent; the grab recorded the indexer and protocol")
	}
	if (*grabbed.Data)["indexer"] != "nzb.invalid" || (*grabbed.Data)["protocol"] != "torrent" {
		t.Errorf("data = %v, want indexer and protocol from the grab", *grabbed.Data)
	}

	// Per-item filter, paging, and an item with no events.
	var mine []apigen.HistoryEvent
	e.get(t, "/api/v1/history?mediaItemId="+acqItoa(item)+"&limit=1&offset=0").
		expect(t, http.StatusOK).into(t, &mine)
	if len(mine) != 1 {
		t.Errorf("limit=1 for the item = %d rows, want 1", len(mine))
	}
	var none []apigen.HistoryEvent
	e.get(t, "/api/v1/history?mediaItemId=9999").expect(t, http.StatusOK).into(t, &none)
	if len(none) != 0 {
		t.Errorf("history for an unknown item = %+v, want nothing", none)
	}
	// Out-of-range paging values fall back to the defaults rather than
	// erroring: a limit of 0 is not "no rows".
	var fallback []apigen.HistoryEvent
	e.get(t, "/api/v1/history?limit=0&offset=-3").expect(t, http.StatusOK).into(t, &fallback)
	if len(fallback) != len(events) {
		t.Errorf("limit=0 = %d rows, want the default page (%d)", len(fallback), len(events))
	}
}

// ---- transfers ----

// Nothing moving is an empty array; a stage recorded in the registry is
// reported with its peer, direction and progress, and the queue row for the
// same download carries the live stage.
func TestGapTransfersReportTheDataPlane(t *testing.T) {
	e := newAPIEnv(t)
	acqAddClient(t, e, "qbittorrent")
	item := e.addMovie(t)
	id := acqGrab(t, e, item)

	body := strings.TrimSpace(e.get(t, "/api/v1/system/transfers").expect(t, http.StatusOK).Body.String())
	if body != "[]" {
		t.Errorf("transfers with nothing moving = %s, want []", body)
	}

	reg := transfers.New()
	e.acq.SetRegistry(reg)
	reg.Begin(transfers.Transfer{
		DownloadID: id, Transfer: "abc123", Title: "Fight.Club.1999.1080p.WEB-DL.x264-TEST",
		Stage: transfers.StageDownloading, Peer: "qbittorrent", Outbound: false,
		Bytes: 1 << 20, Total: 8 << 20, BytesPerSecond: 512, Detail: "40 seeds",
	})

	var live []apigen.Transfer
	e.get(t, "/api/v1/system/transfers").expect(t, http.StatusOK).into(t, &live)
	if len(live) != 1 {
		t.Fatalf("transfers = %+v, want the one stage begun", live)
	}
	tr := live[0]
	if tr.DownloadId != id || tr.Title != "Fight.Club.1999.1080p.WEB-DL.x264-TEST" {
		t.Errorf("transfer = %+v, want it attributed to download %d", tr, id)
	}
	if string(tr.Stage) != transfers.StageDownloading {
		t.Errorf("stage = %q, want downloading", tr.Stage)
	}
	if tr.Transfer == nil || *tr.Transfer != "abc123" {
		t.Errorf("transfer id = %v, want abc123", tr.Transfer)
	}
	if tr.Peer == nil || *tr.Peer != "qbittorrent" {
		t.Errorf("peer = %v, want qbittorrent", tr.Peer)
	}
	if tr.Detail == nil || *tr.Detail != "40 seeds" {
		t.Errorf("detail = %v, want the stage detail", tr.Detail)
	}
	if tr.Total == nil || *tr.Total != 8<<20 || tr.Bytes == nil || *tr.Bytes != 1<<20 {
		t.Errorf("bytes/total = %v/%v, want 1MiB of 8MiB", tr.Bytes, tr.Total)
	}
	if tr.BytesPerSecond == nil || *tr.BytesPerSecond != 512 {
		t.Errorf("bytesPerSecond = %v, want 512", tr.BytesPerSecond)
	}
	if tr.StartedAt.IsZero() {
		t.Error("startedAt is zero")
	}

	// The queue row is the same job, and carries the stage inline.
	var queue []apigen.QueueItem
	e.get(t, "/api/v1/queue").expect(t, http.StatusOK).into(t, &queue)
	if len(queue) != 1 {
		t.Fatalf("queue = %+v", queue)
	}
	q := queue[0]
	if q.Stage == nil || *q.Stage != transfers.StageDownloading {
		t.Errorf("queue stage = %v, want downloading", q.Stage)
	}
	if q.Bytes == nil || *q.Bytes != 1<<20 || q.Total == nil || *q.Total != 8<<20 {
		t.Errorf("queue bytes/total = %v/%v", q.Bytes, q.Total)
	}
	if q.BytesPerSecond == nil || *q.BytesPerSecond != 512 {
		t.Errorf("queue bytesPerSecond = %v, want 512", q.BytesPerSecond)
	}
	if q.StageDetail == nil || *q.StageDetail != "40 seeds" {
		t.Errorf("queue stageDetail = %v", q.StageDetail)
	}
	if q.StagePeer == nil || *q.StagePeer != "qbittorrent" {
		t.Errorf("queue stagePeer = %v", q.StagePeer)
	}
	if q.StageSince == nil || q.StageSince.IsZero() {
		t.Error("queue stageSince absent")
	}

	// A stage that cannot measure itself reports no bytes at all, not zero.
	reg.EndClientStages(id)
	reg.Begin(transfers.Transfer{DownloadID: id, Title: "x", Stage: transfers.StageImporting})
	live = nil
	e.get(t, "/api/v1/system/transfers").expect(t, http.StatusOK).into(t, &live)
	if len(live) != 1 || live[0].Total != nil || live[0].Bytes != nil || live[0].BytesPerSecond != nil ||
		live[0].Peer != nil || live[0].Transfer != nil || live[0].Detail != nil {
		t.Errorf("unmeasured stage = %+v, want every optional field absent", live)
	}
}

// ---- backups ----

// No backups directory is an empty array; files named like backups next to
// the database are listed, and anything else in the directory is not.
func TestGapListBackups(t *testing.T) {
	e := newAPIEnv(t)

	body := strings.TrimSpace(e.get(t, "/api/v1/system/backups").expect(t, http.StatusOK).Body.String())
	if body != "[]" {
		t.Errorf("backups with no directory = %s, want []", body)
	}

	dir := filepath.Join(filepath.Dir(e.db.Path), "backups")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"monarr-20260101-000000.db": "older",
		"monarr-20260102-000000.db": "newest!",
		"notes.txt":                 "not a backup",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "monarr-dir.db"), 0o755); err != nil {
		t.Fatal(err)
	}

	var backups []apigen.BackupInfo
	e.get(t, "/api/v1/system/backups").expect(t, http.StatusOK).into(t, &backups)
	if len(backups) != 2 {
		t.Fatalf("backups = %+v, want the two monarr-*.db files", backups)
	}
	if backups[0].Name != "monarr-20260102-000000.db" || backups[1].Name != "monarr-20260101-000000.db" {
		t.Errorf("order = %q, %q; want newest first", backups[0].Name, backups[1].Name)
	}
	if backups[0].SizeBytes != int64(len("newest!")) {
		t.Errorf("sizeBytes = %d, want %d", backups[0].SizeBytes, len("newest!"))
	}
	if backups[0].CreatedAt.IsZero() {
		t.Error("createdAt is zero")
	}
}

// ---- manual import: scan + default path ----

// The preview lists the media files under a path with what the name says
// about them, skips samples, and refuses a path it cannot see.
func TestGapScanImportPath(t *testing.T) {
	e := newAPIEnv(t)

	for name, q := range map[string]string{
		"no path":      "",
		"blank path":   "?path=%20%20",
		"missing path": "?path=" + url.QueryEscape(filepath.Join(t.TempDir(), "nope")),
	} {
		t.Run(name, func(t *testing.T) {
			acqMessage(t, e.get(t, "/api/v1/import/scan"+q).expect(t, http.StatusBadRequest))
		})
	}

	dir := t.TempDir()
	files := map[string]string{
		"Fight.Club.1999.1080p.WEB-DL.x264-GRP.mkv": "video",
		"Some.Show.S02E03.720p.HDTV.x264-GRP.mkv":   "episode",
		"Fight.Club.1999.1080p.sample.mkv":          "sample",
		"Andy Weir - Project Hail Mary.epub":        "book",
		"README.txt":                                "not media",
		"Some.Show.S02E04.720p.HDTV.x264-GRP.nfo":   "sidecar",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var scanned []apigen.ScannedFile
	e.get(t, "/api/v1/import/scan?path="+url.QueryEscape(dir)).
		expect(t, http.StatusOK).into(t, &scanned)
	byName := map[string]apigen.ScannedFile{}
	for _, f := range scanned {
		byName[f.Name] = f
	}
	if len(scanned) != 3 {
		t.Fatalf("scanned %d files %v, want the movie, the episode and the book", len(scanned), byName)
	}
	movie, ok := byName["Fight.Club.1999.1080p.WEB-DL.x264-GRP.mkv"]
	if !ok {
		t.Fatalf("movie missing from %v", byName)
	}
	if movie.Kind != "video" || movie.Season != -1 || len(movie.Episodes) != 0 || movie.Episodes == nil {
		t.Errorf("movie = %+v, want kind video, season -1, episodes []", movie)
	}
	if movie.Size != int64(len("video")) || movie.Path != filepath.Join(dir, movie.Name) {
		t.Errorf("movie size/path = %d/%q", movie.Size, movie.Path)
	}
	if !strings.Contains(movie.Quality, "1080p") {
		t.Errorf("movie quality = %q, want it to say 1080p", movie.Quality)
	}
	ep, ok := byName["Some.Show.S02E03.720p.HDTV.x264-GRP.mkv"]
	if !ok {
		t.Fatalf("episode missing from %v", byName)
	}
	if ep.Season != 2 || len(ep.Episodes) != 1 || ep.Episodes[0] != 3 {
		t.Errorf("episode = season %d episodes %v, want S02 [3]", ep.Season, ep.Episodes)
	}
	book, ok := byName["Andy Weir - Project Hail Mary.epub"]
	if !ok {
		t.Fatalf("book missing from %v", byName)
	}
	if book.Kind != "book" || book.Quality == "" {
		t.Errorf("book = %+v, want kind book with a format", book)
	}

	// A single media file is a one-entry preview; a non-media file is an
	// empty one rather than an error.
	var one []apigen.ScannedFile
	e.get(t, "/api/v1/import/scan?path="+url.QueryEscape(filepath.Join(dir, movie.Name))).
		expect(t, http.StatusOK).into(t, &one)
	if len(one) != 1 || one[0].Name != movie.Name {
		t.Errorf("single-file scan = %+v", one)
	}
	body := strings.TrimSpace(e.get(t, "/api/v1/import/scan?path="+url.QueryEscape(filepath.Join(dir, "README.txt"))).
		expect(t, http.StatusOK).Body.String())
	if body != "[]" {
		t.Errorf("non-media file scan = %s, want []", body)
	}
}

// The default import path is the download client's local mapping when one
// is configured, and something non-empty when nothing is.
func TestGapManualImportDefaultPath(t *testing.T) {
	e := newAPIEnv(t)

	var def apigen.ImportPathDefault
	e.get(t, "/api/v1/import/default-path").expect(t, http.StatusOK).into(t, &def)
	if def.Path == "" {
		t.Error("default path is empty with nothing configured; the form needs a starting point")
	}

	e.post(t, "/api/v1/downloadclients", `{
		"type":"qbittorrent","name":"qb","url":"box.invalid","password":"x",
		"pathMappings":[{"remote":"/downloads","local":"/mnt/dl/complete/"}]}`).
		expect(t, http.StatusCreated)

	var mapped apigen.ImportPathDefault
	e.get(t, "/api/v1/import/default-path").expect(t, http.StatusOK).into(t, &mapped)
	if mapped.Path != "/mnt/dl/complete" {
		t.Errorf("default path = %q, want the client's cleaned local mapping", mapped.Path)
	}
}

// ---- automatic search ----

// Automatic search on an item runs to completion and reports what it did
// per target; an unknown item is a 404 and no indexers is a 503, both
// decided before anything is searched.
func TestGapAutoSearchLibraryItem(t *testing.T) {
	e := newAPIEnv(t)
	item := e.addMovie(t)

	e.post(t, "/api/v1/library/9999/autosearch", "").expect(t, http.StatusNotFound)

	rr := e.post(t, "/api/v1/library/"+acqItoa(item)+"/autosearch", "").
		expect(t, http.StatusServiceUnavailable)
	if msg := acqMessage(t, rr); !strings.Contains(msg, "indexer") {
		t.Errorf("message = %q, want it to name the missing indexers", msg)
	}

	acqAddIndexer(t, e)
	acqAddClient(t, e, "qbittorrent")

	var out apigen.AutoSearchResult
	e.post(t, "/api/v1/library/"+acqItoa(item)+"/autosearch", "").
		expect(t, http.StatusOK).into(t, &out)
	if len(out.Targets) != 1 {
		t.Fatalf("targets = %+v, want the movie itself", out.Targets)
	}
	tgt := out.Targets[0]
	if tgt.WantableId == "" || tgt.Label == "" {
		t.Errorf("target = %+v, want it identified and labelled", tgt)
	}
	if tgt.Seen != 1 {
		t.Errorf("seen = %d, want the one release the indexer answers with", tgt.Seen)
	}
	if tgt.Error != nil {
		t.Errorf("target error = %q, want none", *tgt.Error)
	}
	if out.Grabbed != 1 || tgt.Grabbed == nil || *tgt.Grabbed != "Fight.Club.1999.1080p.WEB-DL.x264-TEST" {
		t.Errorf("grabbed = %d / %v, want the harness release taken", out.Grabbed, tgt.Grabbed)
	}
	var queue []apigen.QueueItem
	e.get(t, "/api/v1/queue").expect(t, http.StatusOK).into(t, &queue)
	if len(queue) != 1 {
		t.Errorf("queue = %+v, want the automatic grab", queue)
	}

	// Searching again finds the release already in flight: whatever the
	// target reports, a second queue row would be the duplicate-grab bug.
	var again apigen.AutoSearchResult
	e.post(t, "/api/v1/library/"+acqItoa(item)+"/autosearch", "").
		expect(t, http.StatusOK).into(t, &again)
	if len(again.Targets) != 1 {
		t.Fatalf("second run targets = %+v", again.Targets)
	}
	queue = nil
	e.get(t, "/api/v1/queue").expect(t, http.StatusOK).into(t, &queue)
	if len(queue) != 1 {
		t.Errorf("second automatic search added a row: %+v", queue)
	}
}

// ---- wanted search results ----

// The results page of a run echoes its paging and lists nothing until a
// target finishes; a run nobody started is a 404.
func TestGapWantedSearchResultsPage(t *testing.T) {
	e := newAPIEnv(t)
	e.get(t, "/api/v1/wanted/searches/no-such-run/results").expect(t, http.StatusNotFound)

	item := e.addMovie(t)
	acqAddIndexer(t, e)
	rr := e.post(t, "/api/v1/wanted/searches",
		`{"scope":"group","mediaItemId":`+acqItoa(item)+`,"targetDelayMs":0}`).
		expect(t, http.StatusAccepted)
	var run apigen.WantedSearchRun
	rr.into(t, &run)
	if run.RunId == "" {
		t.Fatalf("run has no id: %s", rr.Body.String())
	}

	var page apigen.WantedSearchResultsPage
	e.get(t, "/api/v1/wanted/searches/"+run.RunId+"/results").
		expect(t, http.StatusOK).into(t, &page)
	if page.Limit != 100 || page.Offset != 0 {
		t.Errorf("default paging = %d/%d, want 100/0", page.Limit, page.Offset)
	}
	if page.Items == nil {
		t.Error("items is null; the page iterates it")
	}
	if len(page.Items) != 0 {
		t.Errorf("items = %+v, want none before a target finishes", page.Items)
	}

	e.get(t, "/api/v1/wanted/searches/"+run.RunId+"/results?limit=5&offset=2").
		expect(t, http.StatusOK).into(t, &page)
	if page.Limit != 5 || page.Offset != 2 {
		t.Errorf("paging = %d/%d, want the requested 5/2 echoed", page.Limit, page.Offset)
	}
}

// ---- manual aliases ----

// An operator alias round-trips with its source and role fixed to manual,
// a duplicate is a 409, and removing it twice is a 404 the second time.
func TestGapManualAliasRoundTrip(t *testing.T) {
	e := newAPIEnv(t)
	item := e.addMovie(t)
	path := "/api/v1/library/" + acqItoa(item) + "/aliases"

	rr := e.post(t, path, `{"title":"El club de la lucha","searchable":true}`).
		expect(t, http.StatusCreated)
	var alias apigen.TitleAlias
	rr.into(t, &alias)
	if alias.Id == 0 {
		t.Fatalf("alias has no id: %s", rr.Body.String())
	}
	if alias.Title != "El club de la lucha" || !alias.Searchable {
		t.Errorf("alias = %+v, want the submitted title, searchable", alias)
	}
	if alias.Source != "manual" || string(alias.Role) != "manual" || string(alias.Scope) != "work" {
		t.Errorf("alias provenance = source %q role %q scope %q, want manual/manual/work",
			alias.Source, alias.Role, alias.Scope)
	}

	// Searchable defaults to false when omitted.
	var quiet apigen.TitleAlias
	e.post(t, path, `{"title":"Klub golih pesti"}`).expect(t, http.StatusCreated).into(t, &quiet)
	if quiet.Searchable {
		t.Error("an alias added without searchable must default to not searchable")
	}

	e.post(t, path, `{"title":"El club de la lucha"}`).expect(t, http.StatusConflict)
	e.post(t, path, `{`).expect(t, http.StatusBadRequest)
	e.post(t, "/api/v1/library/9999/aliases", `{"title":"Nobody"}`).expect(t, http.StatusNotFound)
	// A blank or oversized alias is the caller's mistake: 400, as documented.
	e.post(t, path, `{"title":"   "}`).expect(t, http.StatusBadRequest)
	e.post(t, path, `{"title":"`+strings.Repeat("x", 257)+`"}`).expect(t, http.StatusBadRequest)

	e.del(t, path+"/"+acqItoa(alias.Id)).expect(t, http.StatusNoContent)
	e.del(t, path+"/"+acqItoa(alias.Id)).expect(t, http.StatusNotFound)
	// An alias id under the wrong item is not that item's alias.
	e.del(t, "/api/v1/library/9999/aliases/"+acqItoa(quiet.Id)).expect(t, http.StatusNotFound)
}

// ---- saved-notifier test ----

// The per-id Test button tests the SAVED notifier: it answers 200 for a
// stored one the factory can reach and 404 for an id nobody stored.
func TestGapTestNotifierByID(t *testing.T) {
	e := newAPIEnv(t)
	e.post(t, "/api/v1/notifiers/9999/test", "").expect(t, http.StatusNotFound)

	rr := e.post(t, "/api/v1/notifiers",
		`{"type":"plurx","name":"living room","settings":{"url":"plurxd"}}`).
		expect(t, http.StatusCreated)
	var created apigen.Notifier
	rr.into(t, &created)
	e.post(t, "/api/v1/notifiers/"+acqItoa(created.Id)+"/test", "").expect(t, http.StatusOK)
}

// ---- library odds and ends ----

// A rating with votes carries them; one without leaves the field absent, so
// "no vote count" is not rendered as "0 votes".
func TestGapRatingsCarryVotesOnlyWhenCounted(t *testing.T) {
	e := newAPIEnv(t)
	id, err := e.db.CreateMediaItem(context.Background(), domain.MediaItem{
		Kind: domain.KindMovie, Title: "Rated", SortTitle: "rated", Year: 2020,
		IDs: domain.ExternalIDs{TMDB: 424242}, QualityProfileID: 1, Monitored: true,
		Path: e.root,
		Ratings: []domain.Rating{
			{Source: "tmdb", Value: 8.4, Scale: 10, Votes: 12345},
			{Source: "metacritic", Value: 66, Scale: 100},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var detail struct {
		Ratings []apigen.Rating `json:"ratings"`
	}
	e.get(t, "/api/v1/library/"+acqItoa(id)).expect(t, http.StatusOK).into(t, &detail)
	if len(detail.Ratings) != 2 {
		t.Fatalf("ratings = %+v, want both stored ratings", detail.Ratings)
	}
	bys := map[string]apigen.Rating{}
	for _, r := range detail.Ratings {
		bys[r.Source] = r
	}
	if r := bys["tmdb"]; r.Votes == nil || *r.Votes != 12345 || r.Scale != 10 || r.Value < 8.39 || r.Value > 8.41 {
		t.Errorf("tmdb rating = %+v, want 8.4/10 with 12345 votes", r)
	}
	if r := bys["metacritic"]; r.Votes != nil || r.Scale != 100 || r.Value != 66 {
		t.Errorf("metacritic rating = %+v, want 66/100 with no vote count", r)
	}
}

// Triggering a scan is accepted, and un-ignoring a path that was never
// ignored is still a 204: the outcome is the same either way.
func TestGapScanTriggerAndUnignore(t *testing.T) {
	e := newAPIEnv(t)
	e.post(t, "/api/v1/library/scan", "").expect(t, http.StatusAccepted)

	dir := filepath.Join(e.root, "Ignored Folder")
	e.post(t, "/api/v1/library/scan/ignored", fmt.Sprintf(`{"path":%q,"reason":"junk"}`, dir)).
		expect(t, http.StatusNoContent)
	e.del(t, "/api/v1/library/scan/ignored?path="+url.QueryEscape(dir)).expect(t, http.StatusNoContent)
	e.del(t, "/api/v1/library/scan/ignored?path="+url.QueryEscape(dir)).expect(t, http.StatusNoContent)
}

// ---- small helpers ----

// The DTO helpers turn zero values into absent fields, and the evidence
// mapper carries the matched id and warnings only when there are any.
func TestGapDTOHelpers(t *testing.T) {
	if got := suffix(""); got != "" {
		t.Errorf("suffix(\"\") = %q, want empty", got)
	}
	if got := suffix("dial tcp: refused"); got != ": dial tcp: refused" {
		t.Errorf("suffix = %q", got)
	}
	if ptrIfSet(0) != nil {
		t.Error("ptrIfSet(0) must be nil: zero is absent, not an id")
	}
	if p := ptrIfSet(7); p == nil || *p != 7 {
		t.Errorf("ptrIfSet(7) = %v", p)
	}
	if ptrIfText("") != nil {
		t.Error("ptrIfText(\"\") must be nil")
	}
	if p := ptrIfText("x"); p == nil || *p != "x" {
		t.Errorf("ptrIfText(x) = %v", p)
	}

	bare := apiMatchEvidence(domain.MatchEvidence{Version: 1, Matched: false, Reason: "no"})
	if bare.Version != 1 || bare.Matched || bare.Reason != "no" {
		t.Errorf("bare evidence = %+v", bare)
	}
	if bare.Method != nil || bare.Code != nil || bare.Country != nil || bare.OriginalTitle != nil ||
		bare.ParsedTitle != nil || bare.TargetTitle != nil || bare.MatchedTitle != nil ||
		bare.MatchedId != nil || bare.Warnings != nil {
		t.Errorf("bare evidence carries fields it has no values for: %+v", bare)
	}

	full := apiMatchEvidence(domain.MatchEvidence{
		Version: 1, Matched: true, Reason: "id", Method: "external_id", Code: "tmdb",
		Country: "US", OriginalTitle: "o", ParsedTitle: "p", TargetTitle: "t", MatchedTitle: "m",
		MatchedID: &domain.ExternalRef{Provider: "tmdb", Value: "550"},
		Warnings:  []string{"year off by one"},
	})
	if full.MatchedId == nil || full.MatchedId.Provider != "tmdb" || full.MatchedId.Value != "550" {
		t.Errorf("matchedId = %+v, want tmdb/550", full.MatchedId)
	}
	if full.Warnings == nil || len(*full.Warnings) != 1 || (*full.Warnings)[0] != "year off by one" {
		t.Errorf("warnings = %v", full.Warnings)
	}
	for name, got := range map[string]*string{
		"method": full.Method, "code": full.Code, "country": full.Country,
		"originalTitle": full.OriginalTitle, "parsedTitle": full.ParsedTitle,
		"targetTitle": full.TargetTitle, "matchedTitle": full.MatchedTitle,
	} {
		if got == nil || *got == "" {
			t.Errorf("%s absent from a fully populated evidence", name)
		}
	}
}

// ---- generated parameter binding ----

// A path or query parameter that does not parse is a 400 that names the
// parameter, decided by the generated router before any handler runs. The
// alternative — a handler seeing a zero where a number was typed — is a
// "no rows" answer for a typo, which is worse than an error.
func TestGapMalformedParametersAreRefusedByTheRouter(t *testing.T) {
	e := newAPIEnv(t)
	item := e.addMovie(t)
	rel := "/api/v1/library/" + acqItoa(item) + "/releases"

	for name, req := range map[string]struct {
		method, path, param string
	}{
		"queue limit":              {http.MethodGet, "/api/v1/queue?limit=ten", "limit"},
		"queue offset":             {http.MethodGet, "/api/v1/queue?offset=x", "offset"},
		"history limit":            {http.MethodGet, "/api/v1/history?limit=ten", "limit"},
		"history offset":           {http.MethodGet, "/api/v1/history?offset=x", "offset"},
		"history item":             {http.MethodGet, "/api/v1/history?mediaItemId=me", "mediaItemId"},
		"releases id":              {http.MethodGet, "/api/v1/library/abc/releases", "id"},
		"releases season":          {http.MethodGet, rel + "?season=one", "season"},
		"releases episode":         {http.MethodGet, rel + "?episode=two", "episode"},
		"releases copy":            {http.MethodGet, rel + "?copyId=three", "copyId"},
		"review limit":             {http.MethodGet, "/api/v1/library/review?limit=ten", "limit"},
		"review offset":            {http.MethodGet, "/api/v1/library/review?offset=x", "offset"},
		"wanted results limit":     {http.MethodGet, "/api/v1/wanted/searches/run/results?limit=ten", "limit"},
		"wanted results offset":    {http.MethodGet, "/api/v1/wanted/searches/run/results?offset=x", "offset"},
		"scan without path":        {http.MethodGet, "/api/v1/import/scan", "path"},
		"unignore without path":    {http.MethodDelete, "/api/v1/library/scan/ignored", "path"},
		"remove file id":           {http.MethodDelete, "/api/v1/library/abc/files/1", "id"},
		"remove file fileId":       {http.MethodDelete, "/api/v1/library/" + acqItoa(item) + "/files/abc", "fileId"},
		"alias id":                 {http.MethodPost, "/api/v1/library/abc/aliases", "id"},
		"alias aliasId":            {http.MethodDelete, "/api/v1/library/" + acqItoa(item) + "/aliases/abc", "aliasId"},
		"autosearch id":            {http.MethodPost, "/api/v1/library/abc/autosearch", "id"},
		"notifier test id":         {http.MethodPost, "/api/v1/notifiers/abc/test", "id"},
		"preview without kind":     {http.MethodGet, "/api/v1/metadata/preview?tmdbId=550", "kind"},
		"preview tmdbId":           {http.MethodGet, "/api/v1/metadata/preview?kind=movie&tmdbId=abc", "tmdbId"},
		"preview tvdbId":           {http.MethodGet, "/api/v1/metadata/preview?kind=series&tvdbId=abc", "tvdbId"},
		"queue remove fromClient":  {http.MethodDelete, "/api/v1/queue/1?fromClient=maybe", "fromClient"},
		"queue filter enumeration": {http.MethodGet, "/api/v1/queue?filter=", ""},
	} {
		t.Run(name, func(t *testing.T) {
			rr := &httptestRecorder{do(t, e.h, req.method, req.path, "")}
			if req.param == "" {
				// An empty filter is not malformed; it is "no filter".
				rr.expect(t, http.StatusOK)
				return
			}
			rr.expect(t, http.StatusBadRequest)
			if msg := acqMessage(t, rr); !strings.Contains(msg, req.param) {
				t.Errorf("message = %q, want it to name the parameter %q", msg, req.param)
			}
		})
	}
}
