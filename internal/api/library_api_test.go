package api

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Behaviour tests for library_handlers.go, driven through the wired server in
// testenv_test.go rather than through the service directly. The distinction
// matters: almost everything below is about the translation layer — which
// service error becomes which status code, which field lands in the JSON, and
// what the handler does *around* the call (invalidate the wanted index, mask a
// secret, cap a list). None of that is exercised by a service test.

// ---- decode targets ----

// libDetail mirrors the MediaItemDetail fields these tests assert on. Spelled
// out rather than reusing apigen so a field being renamed in the generated
// model shows up here as a failed assertion instead of silently compiling.
type libDetail struct {
	ID               int64  `json:"id"`
	Kind             string `json:"kind"`
	Title            string `json:"title"`
	Year             int    `json:"year"`
	Monitored        bool   `json:"monitored"`
	Path             string `json:"path"`
	Source           string `json:"source"`
	Runtime          int    `json:"runtime"`
	QualityProfileID int64  `json:"qualityProfileId"`
	RootFolderID     int64  `json:"rootFolderId"`
	Genres           []string
	Seasons          []struct {
		Number    int  `json:"number"`
		Monitored bool `json:"monitored"`
		Episodes  []struct {
			ID            int64 `json:"id"`
			SeasonNumber  int   `json:"seasonNumber"`
			EpisodeNumber int   `json:"episodeNumber"`
			Monitored     bool  `json:"monitored"`
		} `json:"episodes"`
	} `json:"seasons"`
	Files []struct {
		ID   int64  `json:"id"`
		Path string `json:"path"`
	} `json:"files"`
	Copies []struct {
		ID               int64  `json:"id"`
		Name             string `json:"name"`
		QualityProfileID int64  `json:"qualityProfileId"`
		RootFolderID     int64  `json:"rootFolderId"`
		Path             string `json:"path"`
		Monitored        bool   `json:"monitored"`
	} `json:"copies"`
}

type libSummary struct {
	ID        int64  `json:"id"`
	Kind      string `json:"kind"`
	Title     string `json:"title"`
	Year      int    `json:"year"`
	Monitored bool   `json:"monitored"`
	Path      string `json:"path"`
}

type libRoot struct {
	ID         int64  `json:"id"`
	Path       string `json:"path"`
	Kind       string `json:"kind"`
	Accessible bool   `json:"accessible"`
	AutoAdopt  bool   `json:"autoAdopt"`
}

type libProfile struct {
	ID              int64  `json:"id"`
	Name            string `json:"name"`
	Sentence        string `json:"sentence"`
	UpgradesAllowed bool   `json:"upgradesAllowed"`
	InUse           *int   `json:"inUse"`
	Target          struct {
		Source     string `json:"source"`
		Resolution int    `json:"resolution"`
		Display    string `json:"display"`
	} `json:"target"`
}

// ---- helpers ----

// libDir makes a directory and returns its path, because every filesystem
// endpoint here needs one and t.TempDir alone cannot make a child.
func libDir(t *testing.T, parts ...string) string {
	t.Helper()
	p := filepath.Join(parts...)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// libVideo writes a plausible video file. Size matters: the scan and the
// plausibility gate both look at it, and a zero-byte file is not a movie.
func libVideo(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, make([]byte, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// libScan runs the reconcile pass synchronously. POST /library/scan hands the
// job to the scheduler, which makes "did the report appear yet" a race; the
// endpoints that *read* the scan's output are what these tests are about, so
// the scan itself is done inline and deterministically.
func libScan(t *testing.T, e *apiEnv) {
	t.Helper()
	if _, err := e.lib.Scan(context.Background()); err != nil {
		t.Fatalf("scan: %v", err)
	}
}

// libAddRoot registers a root of a given kind and returns its id.
func libAddRoot(t *testing.T, e *apiEnv, path, kind string) int64 {
	t.Helper()
	rr := e.post(t, "/api/v1/rootfolders", `{"path":"`+path+`","kind":"`+kind+`"}`).
		expect(t, http.StatusCreated)
	var rf libRoot
	rr.into(t, &rf)
	return rf.ID
}

// libManualSeries files one manual series with a single episode on disk and
// returns the created item. Manual entries are the only series these tests can
// build: the stub metadata provider answers series lookups with an empty
// record (see library_handlers_test.go), so a provider-backed series would
// have no seasons to toggle.
func libManualSeries(t *testing.T, e *apiEnv, name string) libDetail {
	t.Helper()
	dir := libDir(t, e.root, name)
	libVideo(t, dir, name+".S01E01.1080p.WEB-DL.mkv")
	rr := e.post(t, "/api/v1/library/manual",
		`{"kind":"series","title":"`+name+`","path":"`+dir+`"}`).
		expect(t, http.StatusCreated)
	var item libDetail
	rr.into(t, &item)
	return item
}

// ---- library collection ----

// The list endpoint answers with the summary the grid renders, and the kind
// filter is applied by the handler rather than by the client. Asserting the
// content — not just the status — is the point: an empty summary DTO and a
// populated one are both 200s.
func TestLibListReturnsSummariesAndHonoursKindFilter(t *testing.T) {
	e := newAPIEnv(t)
	id := e.addMovie(t)

	var all []libSummary
	e.get(t, "/api/v1/library").expect(t, http.StatusOK).into(t, &all)
	if len(all) != 1 {
		t.Fatalf("library = %d items, want 1", len(all))
	}
	if all[0].ID != id || all[0].Title != "Fight Club" || all[0].Year != 1999 {
		t.Errorf("summary = %+v", all[0])
	}
	if !all[0].Monitored {
		t.Error("an item added without an explicit monitored flag must default to monitored")
	}

	var series []libSummary
	e.get(t, "/api/v1/library?kind=series").expect(t, http.StatusOK).into(t, &series)
	if len(series) != 0 {
		t.Errorf("kind=series returned %d movies; the filter is not being passed through", len(series))
	}
}

// A body the decoder cannot read is the caller's fault, not a 500. This is the
// first branch of every write handler in the file and the easiest one to lose
// when a handler is rewritten around a new DTO.
func TestLibWriteHandlersRejectMalformedJSON(t *testing.T) {
	e := newAPIEnv(t)
	id := e.addMovie(t)
	sid := strconv.FormatInt(id, 10)

	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/library"},
		{http.MethodPatch, "/api/v1/library/" + sid},
		{http.MethodPost, "/api/v1/library/" + sid + "/copies"},
		{http.MethodPatch, "/api/v1/library/" + sid + "/copies/1"},
		{http.MethodPatch, "/api/v1/library/" + sid + "/seasons/1"},
		{http.MethodPatch, "/api/v1/library/" + sid + "/episodes/1"},
		{http.MethodPost, "/api/v1/rootfolders"},
		{http.MethodPatch, "/api/v1/rootfolders/1"},
		{http.MethodPut, "/api/v1/settings"},
		{http.MethodPost, "/api/v1/library/scan/ignored"},
		{http.MethodPost, "/api/v1/library/manual"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rr := &httptestRecorder{do(t, e.h, tc.method, tc.path, "{not json")}
			rr.expect(t, http.StatusBadRequest)
		})
	}
}

// ADR 0009 §1: a root declared to hold one kind refuses an item of another.
// The rule only bites on placement, so the refusal must arrive as a 400 with
// the root named — "internal error" here would send the user hunting a bug
// that is really a misconfigured folder.
func TestLibAddIntoRootOfTheWrongKindIsRefused(t *testing.T) {
	e := newAPIEnv(t)
	seriesRoot := libAddRoot(t, e, t.TempDir(), "series")

	rr := e.post(t, "/api/v1/library",
		`{"kind":"movie","tmdbId":550,"rootFolderId":`+strconv.FormatInt(seriesRoot, 10)+`}`).
		expect(t, http.StatusBadRequest)
	if !strings.Contains(rr.Body.String(), "root folder") {
		t.Errorf("the refusal must say which root and why: %s", rr.Body.String())
	}

	// The library is still empty, so the mixed root added by the harness
	// accepts the same movie and derives the folder from the naming rules.
	var roots []libRoot
	e.get(t, "/api/v1/rootfolders").expect(t, http.StatusOK).into(t, &roots)
	var mixed int64
	for _, rf := range roots {
		if rf.Kind == "mixed" {
			mixed = rf.ID
		}
	}
	if mixed == 0 {
		t.Fatal("the harness registers a mixed root; it is missing")
	}
	var item libDetail
	e.post(t, "/api/v1/library",
		`{"kind":"movie","tmdbId":550,"rootFolderId":`+strconv.FormatInt(mixed, 10)+`}`).
		expect(t, http.StatusCreated).into(t, &item)
	if want := filepath.Join(e.root, "Fight Club (1999)"); item.Path != want {
		t.Errorf("path = %q, want %q", item.Path, want)
	}
}

// The three add-time refusals, each with its own status. They are separate
// cases in libraryErr and have been conflated before: a duplicate that reads
// as 400 makes the UI say "bad request" for a title the user already owns.
func TestLibAddRefusals(t *testing.T) {
	e := newAPIEnv(t)
	e.addMovie(t)

	// Already there — 409, so the client can say so rather than guessing.
	e.post(t, "/api/v1/library", `{"kind":"movie","tmdbId":550}`).expect(t, http.StatusConflict)
	// A kind nothing supports — 400.
	e.post(t, "/api/v1/library", `{"kind":"podcast","tmdbId":1}`).expect(t, http.StatusBadRequest)
	// A real kind with no provider wired — 503 and a sentence naming the fix.
	rr := e.post(t, "/api/v1/library", `{"kind":"book","olid":"OL1W"}`).
		expect(t, http.StatusServiceUnavailable)
	if !strings.Contains(rr.Body.String(), "Settings") {
		t.Errorf("a 503 for a missing key must point at where the key goes: %s", rr.Body.String())
	}
	// No id at all — nothing to look up.
	e.post(t, "/api/v1/library", `{"kind":"movie"}`).expect(t, http.StatusNotFound)
}

// searchNow is fire-and-forget (see searchInBackground): the add must answer
// 201 immediately rather than blocking on a fan-out to every indexer. Asserted
// because "search on add" has twice been implemented inline.
func TestLibAddWithSearchNowStillAnswersImmediately(t *testing.T) {
	e := newAPIEnv(t)
	var item libDetail
	e.post(t, "/api/v1/library", `{"kind":"movie","tmdbId":550,"searchNow":true}`).
		expect(t, http.StatusCreated).into(t, &item)
	if item.ID == 0 || item.Title != "Fight Club" {
		t.Errorf("detail = %+v", item)
	}
}

// The detail DTO carries the fields the item page needs, and the arrays are
// always arrays. `seasons: null` is a blank page in the client, not an empty
// section.
func TestLibGetItemDetailAndMissingItem(t *testing.T) {
	e := newAPIEnv(t)
	id := e.addMovie(t)

	rr := e.get(t, "/api/v1/library/"+strconv.FormatInt(id, 10)).expect(t, http.StatusOK)
	var item libDetail
	rr.into(t, &item)
	if item.Runtime != 139 || item.Kind != "movie" {
		t.Errorf("detail = %+v", item)
	}
	body := rr.Body.String()
	for _, field := range []string{`"seasons":[`, `"files":[`, `"copies":[`, `"genres":[`} {
		if !strings.Contains(body, field) {
			t.Errorf("%s must serialize as an array, got %s", field, body)
		}
	}

	e.get(t, "/api/v1/library/999999").expect(t, http.StatusNotFound)
	// A non-numeric id never reaches the handler; the generated router
	// refuses it, and the shared error handler turns that into a 400.
	e.get(t, "/api/v1/library/not-a-number").expect(t, http.StatusBadRequest)
}

// ---- per-item edit ----

// The edit endpoint applies exactly the fields that were sent and leaves the
// rest alone — partial-PATCH semantics the settings drawer depends on.
func TestLibUpdateItemAppliesOnlyWhatWasSent(t *testing.T) {
	e := newAPIEnv(t)
	id := e.addMovie(t)
	path := "/api/v1/library/" + strconv.FormatInt(id, 10)

	var item libDetail
	e.patch(t, path, `{"monitored":false}`).expect(t, http.StatusOK).into(t, &item)
	if item.Monitored {
		t.Error("monitored:false was not applied")
	}
	if item.Title != "Fight Club" {
		t.Errorf("an unrelated field changed: %+v", item)
	}

	// A profile change is an intentional act, and the handler answers with the
	// stored value rather than echoing the request.
	e.patch(t, path, `{"qualityProfileId":3}`).expect(t, http.StatusOK).into(t, &item)
	if item.QualityProfileID != 3 {
		t.Errorf("qualityProfileId = %d, want 3", item.QualityProfileID)
	}
	if item.Monitored {
		t.Error("the earlier monitored:false was clobbered by an unrelated PATCH")
	}
}

// Placement errors are the user's to fix, so they come back as 400 with the
// reason. Both of these arrive from the service as plain errors, and the
// handler recognises them by message — a fragile seam worth pinning down.
func TestLibUpdateItemPlacementErrorsAre400(t *testing.T) {
	e := newAPIEnv(t)
	id := e.addMovie(t)
	path := "/api/v1/library/" + strconv.FormatInt(id, 10)

	rr := e.patch(t, path, `{"path":"relative/path"}`).expect(t, http.StatusBadRequest)
	if !strings.Contains(rr.Body.String(), "absolute") {
		t.Errorf("the message must say what is wrong with the path: %s", rr.Body.String())
	}
	rr = e.patch(t, path, `{"rootFolderId":4242}`).expect(t, http.StatusBadRequest)
	if !strings.Contains(rr.Body.String(), "root folder") {
		t.Errorf("an unknown root must be named as such: %s", rr.Body.String())
	}

	e.patch(t, "/api/v1/library/999999", `{"monitored":true}`).expect(t, http.StatusNotFound)
}

// Clearing the root folder clears the derived path with it. Leaving a stale
// path behind is how an item ends up pointing at a folder in a root nobody
// manages any more.
func TestLibUpdateItemClearingRootClearsPath(t *testing.T) {
	e := newAPIEnv(t)
	var roots []libRoot
	e.get(t, "/api/v1/rootfolders").expect(t, http.StatusOK).into(t, &roots)

	var item libDetail
	e.post(t, "/api/v1/library",
		`{"kind":"movie","tmdbId":550,"rootFolderId":`+strconv.FormatInt(roots[0].ID, 10)+`}`).
		expect(t, http.StatusCreated).into(t, &item)
	if item.Path == "" {
		t.Fatal("adding into a root must derive a path")
	}

	e.patch(t, "/api/v1/library/"+strconv.FormatInt(item.ID, 10), `{"rootFolderId":0}`).
		expect(t, http.StatusOK).into(t, &item)
	if item.Path != "" || item.RootFolderID != 0 {
		t.Errorf("clearing the root left path=%q root=%d", item.Path, item.RootFolderID)
	}
}

// ---- delete / refresh / rescan / probe ----

func TestLibDeleteItemIsIdempotentlyReportedAsGone(t *testing.T) {
	e := newAPIEnv(t)
	id := e.addMovie(t)
	path := "/api/v1/library/" + strconv.FormatInt(id, 10)

	e.del(t, path).expect(t, http.StatusNoContent)
	e.get(t, path).expect(t, http.StatusNotFound)
	// The second delete is a 404 rather than a second 204: the client needs to
	// know its list is stale.
	e.del(t, path).expect(t, http.StatusNotFound)
}

// Refresh re-hydrates from the provider and hands back the whole detail, so
// the item page can repaint without a follow-up GET. A manual entry has no
// provider to refresh against and is refused with 400 (ADR 0012 §3) rather
// than silently overwritten with whatever id zero returns.
func TestLibRefreshItem(t *testing.T) {
	e := newAPIEnv(t)
	id := e.addMovie(t)

	var item libDetail
	e.post(t, "/api/v1/library/"+strconv.FormatInt(id, 10)+"/refresh", "").
		expect(t, http.StatusOK).into(t, &item)
	if item.Title != "Fight Club" || item.Runtime != 139 {
		t.Errorf("refreshed detail = %+v", item)
	}

	manual := libManualSeries(t, e, "Home Videos")
	rr := e.post(t, "/api/v1/library/"+strconv.FormatInt(manual.ID, 10)+"/refresh", "").
		expect(t, http.StatusBadRequest)
	if !strings.Contains(rr.Body.String(), "manual entry") {
		t.Errorf("the refusal must explain that there is nothing to refresh against: %s", rr.Body.String())
	}

	e.post(t, "/api/v1/library/999999/refresh", "").expect(t, http.StatusNotFound)
}

// Rescan is the manual entry's stand-in for refresh: it re-reads the folder
// and adds episodes for files that appeared since. Pointing it at a
// provider-backed item is refused, because "rescan" there would mean
// something entirely different.
func TestLibRescanManualEntryPicksUpNewFiles(t *testing.T) {
	e := newAPIEnv(t)
	item := libManualSeries(t, e, "Field Notes")
	if len(item.Seasons) != 1 || len(item.Seasons[0].Episodes) != 1 {
		t.Fatalf("manual series should read one episode off disk: %+v", item.Seasons)
	}

	libVideo(t, item.Path, "Field Notes.S01E02.1080p.WEB-DL.mkv")
	var after libDetail
	e.post(t, "/api/v1/library/"+strconv.FormatInt(item.ID, 10)+"/rescan", "").
		expect(t, http.StatusOK).into(t, &after)
	if len(after.Seasons) != 1 || len(after.Seasons[0].Episodes) != 2 {
		t.Errorf("rescan did not pick up the new episode: %+v", after.Seasons)
	}

	provider := e.addMovie(t)
	e.post(t, "/api/v1/library/"+strconv.FormatInt(provider, 10)+"/rescan", "").
		expect(t, http.StatusBadRequest)
	e.post(t, "/api/v1/library/999999/rescan", "").expect(t, http.StatusNotFound)
}

// Reprobe queues the item's video files for re-measurement and reports how
// many. The count is the useful part: "accepted" with nothing queued looks
// identical to success from the UI.
func TestLibReprobeItemReportsFileCount(t *testing.T) {
	e := newAPIEnv(t)
	id := e.addMovie(t)
	dir := libDir(t, e.root, "Fight Club (1999)")
	libVideo(t, dir, "Fight.Club.1999.1080p.WEB-DL.mkv")
	e.patch(t, "/api/v1/library/"+strconv.FormatInt(id, 10),
		`{"path":"`+dir+`"}`).expect(t, http.StatusOK)
	libScan(t, e)

	var res struct {
		Files int `json:"files"`
	}
	e.post(t, "/api/v1/library/"+strconv.FormatInt(id, 10)+"/probe", "").
		expect(t, http.StatusAccepted).into(t, &res)
	if res.Files != 1 {
		t.Errorf("files = %d, want the one video the scan linked", res.Files)
	}

	// The existence check is separate from the reprobe itself, and it is the
	// only reason an unknown id is a 404 rather than an empty 202.
	e.post(t, "/api/v1/library/999999/probe", "").expect(t, http.StatusNotFound)
}

// ---- files ----

// Removing a file is three independent decisions, and the response has to
// report what actually happened to each. A blocklist request against an
// adopted file cannot be honoured — there is no release to condemn — and that
// has to arrive as a note, not as silence or as a failure.
func TestLibRemoveFileReportsEachDecision(t *testing.T) {
	e := newAPIEnv(t)
	id := e.addMovie(t)
	dir := libDir(t, e.root, "Fight Club (1999)")
	onDisk := libVideo(t, dir, "Fight.Club.1999.1080p.WEB-DL.mkv")
	e.patch(t, "/api/v1/library/"+strconv.FormatInt(id, 10),
		`{"path":"`+dir+`"}`).expect(t, http.StatusOK)
	libScan(t, e)

	var item libDetail
	e.get(t, "/api/v1/library/"+strconv.FormatInt(id, 10)).expect(t, http.StatusOK).into(t, &item)
	if len(item.Files) != 1 {
		t.Fatalf("scan linked %d files, want 1", len(item.Files))
	}
	fileID := item.Files[0].ID

	base := "/api/v1/library/" + strconv.FormatInt(id, 10) + "/files/" + strconv.FormatInt(fileID, 10)
	// A file id that belongs to another item is refused before anything is
	// deleted — the id comes off a URL, and a stale tab must not delete a
	// stranger's file.
	other := libManualSeries(t, e, "Unrelated Show")
	e.del(t, "/api/v1/library/"+strconv.FormatInt(other.ID, 10)+"/files/"+strconv.FormatInt(fileID, 10)).
		expect(t, http.StatusNotFound)

	var res struct {
		Path            string `json:"path"`
		DeletedFromDisk bool   `json:"deletedFromDisk"`
		Blocklisted     string `json:"blocklisted"`
		Note            string `json:"note"`
	}
	e.del(t, base+"?fromDisk=true&blocklist=true").expect(t, http.StatusOK).into(t, &res)
	if res.Path != onDisk || !res.DeletedFromDisk {
		t.Errorf("result = %+v, want the deleted path reported", res)
	}
	if res.Blocklisted != "" {
		t.Errorf("an adopted file has no source release to blocklist, got %q", res.Blocklisted)
	}
	if !strings.Contains(res.Note, "blocklist") {
		t.Errorf("the unhonoured blocklist request must be explained: %q", res.Note)
	}
	if _, err := os.Stat(onDisk); !os.IsNotExist(err) {
		t.Errorf("fromDisk=true left the file on disk: %v", err)
	}
	// Gone means gone: the record is not there to remove twice.
	e.del(t, base).expect(t, http.StatusNotFound)
}

// ---- copies ----

// A copy is a second quality target with its own profile and lifecycle. The
// happy path returns the parent's whole detail with the copy embedded, which
// is what lets the item page repaint from one response.
func TestLibMediaCopyLifecycle(t *testing.T) {
	e := newAPIEnv(t)
	id := e.addMovie(t)
	base := "/api/v1/library/" + strconv.FormatInt(id, 10) + "/copies"

	var item libDetail
	e.post(t, base, `{"qualityProfileId":3,"name":"4K for the projector"}`).
		expect(t, http.StatusOK).into(t, &item)
	if len(item.Copies) != 1 {
		t.Fatalf("copies = %+v, want 1", item.Copies)
	}
	cp := item.Copies[0]
	if cp.QualityProfileID != 3 || cp.Name != "4K for the projector" || !cp.Monitored {
		t.Errorf("copy = %+v; a copy defaults to monitored", cp)
	}

	copyPath := base + "/" + strconv.FormatInt(cp.ID, 10)
	e.patch(t, copyPath, `{"monitored":false,"name":"retired"}`).
		expect(t, http.StatusOK).into(t, &item)
	if item.Copies[0].Monitored || item.Copies[0].Name != "retired" {
		t.Errorf("copy after update = %+v", item.Copies[0])
	}

	e.del(t, copyPath).expect(t, http.StatusOK).into(t, &item)
	if len(item.Copies) != 0 {
		t.Errorf("copy survived the delete: %+v", item.Copies)
	}
	e.del(t, copyPath).expect(t, http.StatusNotFound)
	e.patch(t, copyPath, `{"monitored":true}`).expect(t, http.StatusNotFound)
}

// Every way a copy can be rejected, with the status the UI branches on. These
// come back from the service as plain errors matched by message, so a reworded
// message silently turns each of these into a 500.
func TestLibMediaCopyRefusals(t *testing.T) {
	e := newAPIEnv(t)
	var roots []libRoot
	e.get(t, "/api/v1/rootfolders").expect(t, http.StatusOK).into(t, &roots)
	rootID := strconv.FormatInt(roots[0].ID, 10)

	var item libDetail
	e.post(t, "/api/v1/library", `{"kind":"movie","tmdbId":550,"rootFolderId":`+rootID+`}`).
		expect(t, http.StatusCreated).into(t, &item)
	base := "/api/v1/library/" + strconv.FormatInt(item.ID, 10) + "/copies"

	// A profile that does not exist.
	rr := e.post(t, base, `{"qualityProfileId":9999}`).expect(t, http.StatusBadRequest)
	if !strings.Contains(rr.Body.String(), "profile") {
		t.Errorf("the refusal must name the profile: %s", rr.Body.String())
	}
	// A root that does not exist.
	e.post(t, base, `{"qualityProfileId":2,"rootFolderId":9999}`).expect(t, http.StatusBadRequest)
	// The item's own root: the copy folder would be the item's folder, and two
	// targets writing one folder is the collision this check exists for.
	rr = e.post(t, base, `{"qualityProfileId":2,"rootFolderId":`+rootID+`}`).
		expect(t, http.StatusBadRequest)
	if !strings.Contains(rr.Body.String(), "collide") {
		t.Errorf("a colliding copy folder must say so: %s", rr.Body.String())
	}

	e.post(t, "/api/v1/library/999999/copies", `{"qualityProfileId":2}`).expect(t, http.StatusNotFound)
}

// ---- season / episode monitoring ----

// Flipping a season cascades to its episodes — an unmonitored season wants
// none of them — and the response carries the whole item so the client does
// not have to model the cascade itself.
func TestLibSeasonAndEpisodeMonitoring(t *testing.T) {
	e := newAPIEnv(t)
	item := libManualSeries(t, e, "Slow Horses")
	base := "/api/v1/library/" + strconv.FormatInt(item.ID, 10)
	epID := item.Seasons[0].Episodes[0].ID

	// Manual entries start unmonitored (ADR 0012 §4), so switching the season
	// on is the interesting direction.
	var after libDetail
	e.patch(t, base+"/seasons/1", `{"monitored":true}`).expect(t, http.StatusOK).into(t, &after)
	if !after.Seasons[0].Monitored {
		t.Fatal("season 1 did not flip")
	}
	if !after.Seasons[0].Episodes[0].Monitored {
		t.Error("the season flip must cascade to its episodes")
	}

	// One episode can then be excluded without disturbing the season.
	e.patch(t, base+"/episodes/"+strconv.FormatInt(epID, 10), `{"monitored":false}`).
		expect(t, http.StatusOK).into(t, &after)
	if after.Seasons[0].Episodes[0].Monitored {
		t.Error("the episode flip was not applied")
	}
	if !after.Seasons[0].Monitored {
		t.Error("an episode edit must not silently unmonitor the season")
	}

	// A season or episode that is not this item's is a 404, not a no-op 200:
	// a silent success here means the UI shows a toggle that never took.
	e.patch(t, base+"/seasons/9", `{"monitored":true}`).expect(t, http.StatusNotFound)
	e.patch(t, base+"/episodes/999999", `{"monitored":true}`).expect(t, http.StatusNotFound)
	e.patch(t, "/api/v1/library/999999/seasons/1", `{"monitored":true}`).expect(t, http.StatusNotFound)
}

// ---- manual entries (ADR 0012) ----

// A manual entry is the escape hatch from review for media no provider
// carries. It is created unmonitored on purpose, and its folder is claimed —
// a second entry pointing at the same folder is a 409, because the next scan
// would give one of them the other's files.
func TestLibManualEntryRefusals(t *testing.T) {
	e := newAPIEnv(t)
	item := libManualSeries(t, e, "Wedding Tapes")
	if item.Monitored {
		t.Error("a manual entry must start unmonitored (ADR 0012 §4)")
	}
	if item.Source != "manual" {
		t.Errorf("source = %q, want manual", item.Source)
	}

	// Same folder twice.
	rr := e.post(t, "/api/v1/library/manual",
		`{"kind":"series","title":"Wedding Tapes Again","path":"`+item.Path+`"}`).
		expect(t, http.StatusConflict)
	if !strings.Contains(rr.Body.String(), "Wedding Tapes") {
		t.Errorf("the conflict must name the folder's current owner: %s", rr.Body.String())
	}

	// A folder outside every registered root. The library must not be aimable
	// at arbitrary places on the host by anyone who can post JSON.
	outside := libDir(t, t.TempDir(), "Somewhere Else")
	e.post(t, "/api/v1/library/manual",
		`{"kind":"movie","title":"Elsewhere","path":"`+outside+`"}`).
		expect(t, http.StatusNotFound)

	// A folder that is not on disk at all.
	e.post(t, "/api/v1/library/manual",
		`{"kind":"movie","title":"Ghost","path":"`+filepath.Join(e.root, "no-such-folder")+`"}`).
		expect(t, http.StatusNotFound)

	// A kind nothing supports.
	e.post(t, "/api/v1/library/manual",
		`{"kind":"podcast","title":"Nope","path":"`+item.Path+`"}`).
		expect(t, http.StatusBadRequest)

	// ADR 0009 again: a manual entry may not be filed into a root of another
	// kind either.
	seriesRoot := t.TempDir()
	libAddRoot(t, e, seriesRoot, "series")
	dir := libDir(t, seriesRoot, "Some Movie (2001)")
	rr = e.post(t, "/api/v1/library/manual",
		`{"kind":"movie","title":"Some Movie","year":2001,"path":"`+dir+`"}`).
		expect(t, http.StatusBadRequest)
	if !strings.Contains(rr.Body.String(), "holds series") {
		t.Errorf("the refusal must name the root's kind: %s", rr.Body.String())
	}

	// A title that is only whitespace and a path that is not absolute are both
	// refused; the exact statuses differ (see the report accompanying this
	// file), so only the refusal itself is asserted.
	for _, body := range []string{
		`{"kind":"movie","title":"   ","path":"` + item.Path + `"}`,
		`{"kind":"movie","title":"Relative","path":"not/absolute"}`,
	} {
		if rr := e.post(t, "/api/v1/library/manual", body); rr.Code < 400 {
			t.Errorf("body %s was accepted with %d", body, rr.Code)
		}
	}
}

// ---- root folders ----

func TestLibRootFolderCRUD(t *testing.T) {
	e := newAPIEnv(t)
	dir := t.TempDir()

	var rf libRoot
	e.post(t, "/api/v1/rootfolders", `{"path":"`+dir+`","kind":"movie"}`).
		expect(t, http.StatusCreated).into(t, &rf)
	if rf.Path != dir || rf.Kind != "movie" {
		t.Fatalf("created root = %+v", rf)
	}

	// The list adds the live disk facts the picker shows.
	var roots []libRoot
	e.get(t, "/api/v1/rootfolders").expect(t, http.StatusOK).into(t, &roots)
	var found bool
	for _, r := range roots {
		if r.ID == rf.ID {
			found = true
			if !r.Accessible {
				t.Error("a root that exists on disk must list as accessible")
			}
		}
	}
	if !found {
		t.Fatalf("the created root is missing from the list: %+v", roots)
	}

	// Retyping only routes what happens next (ADR 0009 §1).
	e.patch(t, "/api/v1/rootfolders/"+strconv.FormatInt(rf.ID, 10), `{"kind":"mixed"}`).
		expect(t, http.StatusOK).into(t, &rf)
	if rf.Kind != "mixed" {
		t.Errorf("kind after retype = %q", rf.Kind)
	}

	e.del(t, "/api/v1/rootfolders/"+strconv.FormatInt(rf.ID, 10)).expect(t, http.StatusNoContent)
	e.del(t, "/api/v1/rootfolders/"+strconv.FormatInt(rf.ID, 10)).expect(t, http.StatusNotFound)
	e.patch(t, "/api/v1/rootfolders/999999", `{"kind":"movie"}`).expect(t, http.StatusNotFound)
}

// Every reason a root is refused, each with the sentence that teaches the
// model. ADR 0009 §3 in particular: nested roots offer every folder under the
// overlap twice, and registering /media over /media/Movies is the specific
// mistake that makes a scan look like it is trawling the whole disk.
func TestLibRootFolderRefusals(t *testing.T) {
	e := newAPIEnv(t)

	e.post(t, "/api/v1/rootfolders", `{"path":"not-absolute"}`).expect(t, http.StatusBadRequest)
	e.post(t, "/api/v1/rootfolders", `{"path":"/does/not/exist/anywhere"}`).
		expect(t, http.StatusBadRequest)

	// A file is not a folder.
	f := filepath.Join(t.TempDir(), "notadir.txt")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.post(t, "/api/v1/rootfolders", `{"path":"`+f+`"}`).expect(t, http.StatusBadRequest)

	// Duplicate.
	e.post(t, "/api/v1/rootfolders", `{"path":"`+e.root+`"}`).expect(t, http.StatusBadRequest)

	// Inside an existing root.
	inside := libDir(t, e.root, "Movies")
	rr := e.post(t, "/api/v1/rootfolders", `{"path":"`+inside+`"}`).expect(t, http.StatusBadRequest)
	if !strings.Contains(rr.Body.String(), "inside") {
		t.Errorf("a nested root must say which root contains it: %s", rr.Body.String())
	}

	// Containing an existing root.
	rr = e.post(t, "/api/v1/rootfolders", `{"path":"`+filepath.Dir(e.root)+`"}`).
		expect(t, http.StatusBadRequest)
	if !strings.Contains(rr.Body.String(), "contains") {
		t.Errorf("a containing root must say which root it swallows: %s", rr.Body.String())
	}

	// An unknown kind is refused rather than stored — a root typed "film"
	// would accept nothing at all.
	if rr := e.post(t, "/api/v1/rootfolders", `{"path":"`+t.TempDir()+`","kind":"film"}`); rr.Code < 400 {
		t.Errorf("an unknown root kind was accepted with %d", rr.Code)
	}
}

// ---- filesystem browse (ADR 0009 §2a) ----

// The picker needs three things from this endpoint: the children, the way back
// up, and which of them are already registered. The registered flag is what
// stops the picker offering a folder whose add will fail as a duplicate.
func TestLibBrowseFilesystemListsDirectoriesAndFlagsRegistered(t *testing.T) {
	e := newAPIEnv(t)
	libDir(t, e.root, "Movies")
	libDir(t, e.root, "Music")
	libVideo(t, e.root, "loose-file.mkv")

	var res struct {
		Path   string `json:"path"`
		Parent string `json:"parent"`
		Dirs   []struct {
			Name       string `json:"name"`
			Path       string `json:"path"`
			Registered bool   `json:"registered"`
		} `json:"dirs"`
	}
	e.get(t, "/api/v1/filesystem?path="+e.root+string(filepath.Separator)).
		expect(t, http.StatusOK).into(t, &res)
	if len(res.Dirs) != 2 {
		t.Fatalf("dirs = %+v; files must never be listed", res.Dirs)
	}
	if res.Dirs[0].Name != "Movies" || res.Dirs[1].Name != "Music" {
		t.Errorf("dirs are not sorted case-insensitively: %+v", res.Dirs)
	}
	if res.Parent == "" {
		t.Error("parent must be set below the filesystem root, or the picker cannot go up")
	}

	// The harness's own root is registered, and browsing its parent must say
	// so on the row that is it.
	e.get(t, "/api/v1/filesystem?path="+filepath.Dir(e.root)+string(filepath.Separator)).
		expect(t, http.StatusOK).into(t, &res)
	var sawRegistered bool
	for _, d := range res.Dirs {
		if d.Path == e.root {
			sawRegistered = d.Registered
		}
	}
	if !sawRegistered {
		t.Error("an already-registered root must be flagged so the picker can grey it out")
	}

	// A partial final component filters by prefix — this is the typeahead.
	e.get(t, "/api/v1/filesystem?path="+filepath.Join(e.root, "Mov")).
		expect(t, http.StatusOK).into(t, &res)
	if len(res.Dirs) != 1 || res.Dirs[0].Name != "Movies" {
		t.Errorf("prefix filter = %+v, want only Movies", res.Dirs)
	}

	// Missing and forbidden are one answer on purpose: telling them apart is
	// the useful part to somebody probing the host.
	e.get(t, "/api/v1/filesystem?path="+filepath.Join(e.root, "nope")+string(filepath.Separator)).
		expect(t, http.StatusBadRequest)
}

// ---- metadata search ----

// The inLibrary flag is computed server-side against the ids the result
// carries. Getting it wrong means the Add button offers a title the user
// already owns, and the add then fails as a duplicate.
func TestLibMetadataSearchMarksWhatIsAlreadyOwned(t *testing.T) {
	e := newAPIEnv(t)

	var results []struct {
		Kind      string `json:"kind"`
		TmdbID    int64  `json:"tmdbId"`
		Title     string `json:"title"`
		InLibrary bool   `json:"inLibrary"`
	}
	e.get(t, "/api/v1/metadata/search?kind=movie&query=fight").
		expect(t, http.StatusOK).into(t, &results)
	if len(results) != 1 || results[0].TmdbID != 550 || results[0].InLibrary {
		t.Fatalf("before adding: %+v", results)
	}

	e.addMovie(t)
	e.get(t, "/api/v1/metadata/search?kind=movie&query=fight").
		expect(t, http.StatusOK).into(t, &results)
	if !results[0].InLibrary {
		t.Error("a title already in the library must come back flagged")
	}

	// Both parameters are required by the spec, so the generated router
	// refuses the request before the handler sees it.
	e.get(t, "/api/v1/metadata/search?kind=movie").expect(t, http.StatusBadRequest)
	e.get(t, "/api/v1/metadata/search?query=fight").expect(t, http.StatusBadRequest)

	// No results is an empty array, never null — the client maps over this.
	body := e.get(t, "/api/v1/metadata/search?kind=series&query=nothing").
		expect(t, http.StatusOK).Body.String()
	if strings.TrimSpace(body) != "[]" {
		t.Errorf("empty search = %s, want []", body)
	}
}

// ---- settings ----

// Settings round-trip with the secrets masked. The hint is the last four
// characters and nothing else: a settings page that can echo a key back in
// full is a key that leaks to anything that can read the page.
func TestLibSettingsRoundTripMasksSecrets(t *testing.T) {
	e := newAPIEnv(t)

	var before struct {
		TmdbConfigured  bool   `json:"tmdbApiKeyConfigured"`
		TmdbHint        string `json:"tmdbApiKeyHint"`
		OmdbConfigured  bool   `json:"omdbApiKeyConfigured"`
		TraktConfigured bool   `json:"traktClientIdConfigured"`
		AuthRequired    bool   `json:"authRequired"`
		DefaultProfiles struct {
			Movie  int64 `json:"movie"`
			Series int64 `json:"series"`
			Book   int64 `json:"book"`
		} `json:"defaultProfiles"`
	}
	e.get(t, "/api/v1/settings").expect(t, http.StatusOK).into(t, &before)
	if before.TmdbConfigured || before.OmdbConfigured || before.TraktConfigured {
		t.Errorf("a fresh install reports no keys configured, got %+v", before)
	}
	// Always the resolved answer, including the built-in fallbacks: a settings
	// screen showing nothing for "default profile" is how you end up believing
	// there isn't one.
	if before.DefaultProfiles.Movie == 0 || before.DefaultProfiles.Book == 0 {
		t.Errorf("default profiles must resolve to the built-ins: %+v", before.DefaultProfiles)
	}

	e.put(t, "/api/v1/settings", `{"tmdbApiKey":"secret-key-1234","omdbApiKey":"omdb-9876",`+
		`"traktClientId":"trakt-5555","scanSkipPatterns":"sample*\nextras"}`).
		expect(t, http.StatusNoContent)

	rr := e.get(t, "/api/v1/settings").expect(t, http.StatusOK)
	var after struct {
		TmdbConfigured   bool   `json:"tmdbApiKeyConfigured"`
		TmdbHint         string `json:"tmdbApiKeyHint"`
		OmdbHint         string `json:"omdbApiKeyHint"`
		TraktHint        string `json:"traktClientIdHint"`
		ScanSkipPatterns string `json:"scanSkipPatterns"`
	}
	rr.into(t, &after)
	if !after.TmdbConfigured || after.TmdbHint != "…1234" ||
		after.OmdbHint != "…9876" || after.TraktHint != "…5555" {
		t.Errorf("hints = %+v", after)
	}
	if after.ScanSkipPatterns != "sample*\nextras" {
		t.Errorf("skip patterns did not round-trip: %q", after.ScanSkipPatterns)
	}
	for _, secret := range []string{"secret-key", "omdb-9", "trakt-5"} {
		if strings.Contains(rr.Body.String(), secret) {
			t.Errorf("a full key reached the client: %s", rr.Body.String())
		}
	}

	// Values are trimmed on the way in, so a key pasted with a stray newline
	// does not fail every provider call with an unhelpful 401.
	e.put(t, "/api/v1/settings", `{"tmdbApiKey":"  padded-key-abcd  "}`).expect(t, http.StatusNoContent)
	e.get(t, "/api/v1/settings").expect(t, http.StatusOK).into(t, &after)
	if after.TmdbHint != "…abcd" {
		t.Errorf("hint after a padded key = %q; the value was not trimmed", after.TmdbHint)
	}

	// A key too short to show four characters of is bulleted rather than
	// printed: the hint exists to confirm which key is stored, and a
	// four-character key shown whole is just the key.
	e.put(t, "/api/v1/settings", `{"tmdbApiKey":"abc"}`).expect(t, http.StatusNoContent)
	e.get(t, "/api/v1/settings").expect(t, http.StatusOK).into(t, &after)
	if after.TmdbHint != "•••" {
		t.Errorf("hint for a short key = %q, want three bullets", after.TmdbHint)
	}

	// Monarr's own API key is deliberately *not* masked — it is the value the
	// user has to copy into Prowlarr, and a masked one would be useless.
	e.setting(t, APIKeySetting, "monarr-key-abcdef")
	var withKey struct {
		APIKey string `json:"apiKey"`
	}
	e.get(t, "/api/v1/settings").expect(t, http.StatusOK).into(t, &withKey)
	if withKey.APIKey != "monarr-key-abcdef" {
		t.Errorf("apiKey = %q; the settings page has to be able to show it", withKey.APIKey)
	}
}

// The per-kind defaults are partial by design and validated on the way in: a
// profile that does not exist, or that lives on the wrong format axis, is
// refused now rather than discovered on the next add.
func TestLibSettingsDefaultProfilesAreValidated(t *testing.T) {
	e := newAPIEnv(t)

	e.put(t, "/api/v1/settings", `{"defaultProfiles":{"movie":9999}}`).
		expect(t, http.StatusBadRequest)
	// Profile 4 is the built-in Ebook profile; a movie cannot use it.
	rr := e.put(t, "/api/v1/settings", `{"defaultProfiles":{"movie":4}}`).
		expect(t, http.StatusBadRequest)
	if !strings.Contains(rr.Body.String(), "cannot use") {
		t.Errorf("a format-axis mismatch must explain itself: %s", rr.Body.String())
	}

	// Sending only one kind leaves the others alone.
	e.put(t, "/api/v1/settings", `{"defaultProfiles":{"movie":3}}`).expect(t, http.StatusNoContent)
	var out struct {
		DefaultProfiles struct {
			Movie  int64 `json:"movie"`
			Series int64 `json:"series"`
			Book   int64 `json:"book"`
		} `json:"defaultProfiles"`
	}
	e.get(t, "/api/v1/settings").expect(t, http.StatusOK).into(t, &out)
	if out.DefaultProfiles.Movie != 3 {
		t.Errorf("movie default = %d, want 3", out.DefaultProfiles.Movie)
	}
	if out.DefaultProfiles.Series != 1 || out.DefaultProfiles.Book != 4 {
		t.Errorf("an unsent kind was changed: %+v", out.DefaultProfiles)
	}

	// And the default is what a new add lands on when nothing is chosen.
	var item libDetail
	e.post(t, "/api/v1/library", `{"kind":"movie","tmdbId":550}`).
		expect(t, http.StatusCreated).into(t, &item)
	if item.QualityProfileID != 3 {
		t.Errorf("added item used profile %d, not the configured default", item.QualityProfileID)
	}
}

// ---- quality profiles ----

// The list carries the server-rendered sentence and the in-use count, which
// are what let every client say the same thing about a profile and know
// whether DELETE will be refused before offering the button.
func TestLibProfileListCarriesSentenceAndUsage(t *testing.T) {
	e := newAPIEnv(t)
	e.addMovie(t)

	var profiles []libProfile
	e.get(t, "/api/v1/profiles").expect(t, http.StatusOK).into(t, &profiles)
	if len(profiles) < 5 {
		t.Fatalf("the built-in profiles are missing: %+v", profiles)
	}
	var any libProfile
	for _, p := range profiles {
		if p.ID == 1 {
			any = p
		}
		if p.Sentence == "" {
			t.Errorf("profile %d has no sentence; every client would have to invent one", p.ID)
		}
		if p.InUse == nil {
			t.Errorf("profile %d reports no usage count", p.ID)
		}
	}
	if any.InUse == nil || *any.InUse == 0 {
		t.Errorf("profile 1 backs the movie just added; inUse = %v", any.InUse)
	}
}

func TestLibProfileCreateUpdateDelete(t *testing.T) {
	e := newAPIEnv(t)

	var created libProfile
	e.post(t, "/api/v1/profiles",
		`{"name":"Remux only","target":{"source":"remux","resolution":2160},"upgradesAllowed":false}`).
		expect(t, http.StatusCreated).into(t, &created)
	if created.ID == 0 || created.Target.Source != "remux" || created.UpgradesAllowed {
		t.Fatalf("created = %+v", created)
	}
	if created.Sentence == "" {
		t.Error("a created profile must come back with its sentence, like a listed one")
	}

	path := "/api/v1/profiles/" + strconv.FormatInt(created.ID, 10)
	var updated libProfile
	e.put(t, path, `{"name":"Remux 1080p","target":{"source":"remux","resolution":1080},`+
		`"floor":{"source":"webdl","resolution":1080}}`).
		expect(t, http.StatusOK).into(t, &updated)
	if updated.Name != "Remux 1080p" || updated.Target.Resolution != 1080 {
		t.Errorf("updated = %+v", updated)
	}

	e.del(t, path).expect(t, http.StatusNoContent)
	e.del(t, path).expect(t, http.StatusNotFound)
	e.put(t, "/api/v1/profiles/999999", `{"name":"Ghost","target":{"source":"webdl","resolution":1080}}`).
		expect(t, http.StatusNotFound)
}

// A profile is refused rather than stored when it could never grab anything.
// Each of these is a sentence the user can act on, not a validation code.
func TestLibProfileInputRefusals(t *testing.T) {
	e := newAPIEnv(t)
	for _, tc := range []struct{ name, body, want string }{
		{"no name", `{"name":"  ","target":{"source":"webdl","resolution":1080}}`, "needs a name"},
		{"no target", `{"name":"Empty","target":{"source":""}}`, "target to hunt toward"},
		{"unknown target", `{"name":"Huh","target":{"source":"unknown"}}`, "target to hunt toward"},
		{"empty floor", `{"name":"Floored","target":{"source":"webdl","resolution":1080},` +
			`"floor":{"source":""}}`, "floor needs a quality"},
		{"floor above target", `{"name":"Impossible","target":{"source":"hdtv","resolution":720},` +
			`"floor":{"source":"remux","resolution":2160}}`, "nothing would ever be grabbed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := e.post(t, "/api/v1/profiles", tc.body).expect(t, http.StatusBadRequest)
			if !strings.Contains(rr.Body.String(), tc.want) {
				t.Errorf("message = %s, want it to mention %q", rr.Body.String(), tc.want)
			}
			// The same validation guards the update path.
			e.put(t, "/api/v1/profiles/2", tc.body).expect(t, http.StatusBadRequest)
		})
	}
}

// Deleting a profile something still points at is refused with 409. Silently
// orphaning an item onto an id that no longer exists is damage nobody sees
// until an upgrade decision goes strange months later.
func TestLibProfileInUseCannotBeDeleted(t *testing.T) {
	e := newAPIEnv(t)
	id := e.addMovie(t)

	rr := e.del(t, "/api/v1/profiles/1").expect(t, http.StatusConflict)
	if !strings.Contains(rr.Body.String(), "use it") {
		t.Errorf("the 409 must say what is holding the profile: %s", rr.Body.String())
	}

	// Move the item off it and the delete goes through, which is the other
	// half of the rule: it is a reference check, not a permanent ban.
	e.patch(t, "/api/v1/library/"+strconv.FormatInt(id, 10), `{"qualityProfileId":2}`).
		expect(t, http.StatusOK)
	e.del(t, "/api/v1/profiles/1").expect(t, http.StatusNoContent)

	// A copy counts as a reference too.
	e.post(t, "/api/v1/library/"+strconv.FormatInt(id, 10)+"/copies", `{"qualityProfileId":3}`).
		expect(t, http.StatusOK)
	e.del(t, "/api/v1/profiles/3").expect(t, http.StatusConflict)
}

// ---- bulk edit ----

// The mass editor applies one edit across a selection and reports a count the
// UI shows back to the user. Only real ids are sent here: the storage layer
// cannot currently tell a matched row from an unmatched one, so an id that no
// longer exists is counted as updated (see the note filed with this file).
func TestLibBulkEditReportsWhatItChanged(t *testing.T) {
	e := newAPIEnv(t)
	id := e.addMovie(t)
	other := libManualSeries(t, e, "Bulk Show")

	var res struct {
		Updated int `json:"updated"`
	}
	e.post(t, "/api/v1/library/bulk",
		`{"ids":[`+strconv.FormatInt(id, 10)+`,`+strconv.FormatInt(other.ID, 10)+`],`+
			`"monitored":true,"qualityProfileId":2}`).
		expect(t, http.StatusOK).into(t, &res)
	if res.Updated != 2 {
		t.Errorf("updated = %d, want the two ids that were sent", res.Updated)
	}

	var item libDetail
	e.get(t, "/api/v1/library/"+strconv.FormatInt(other.ID, 10)).
		expect(t, http.StatusOK).into(t, &item)
	if !item.Monitored || item.QualityProfileID != 2 {
		t.Errorf("bulk edit did not reach the item: %+v", item)
	}

	// An empty selection is the caller's mistake, not a no-op success.
	e.post(t, "/api/v1/library/bulk", `{"ids":[]}`).expect(t, http.StatusBadRequest)
	e.post(t, "/api/v1/library/bulk", `{"monitored":true}`).expect(t, http.StatusBadRequest)
}

// ---- scan, report, review queue ----

// The scan endpoint hands the job to the scheduler and answers 202: the
// reconcile pass walks the whole library, which is far longer than any sane
// HTTP timeout.
func TestLibScanIsAcceptedNotAwaited(t *testing.T) {
	e := newAPIEnv(t)
	e.post(t, "/api/v1/library/scan", "").expect(t, http.StatusAccepted)
}

// The report is 404 until a scan has run — an empty report and "never scanned"
// are different answers, and the settings page says different things about
// them. Once it exists, the embedded unmatched list is capped while the total
// is not, so a library with hundreds of unmatched folders does not ship the
// whole list every time the page loads.
func TestLibScanReportCapsTheEmbeddedListButNotTheTotal(t *testing.T) {
	e := newAPIEnv(t)
	e.get(t, "/api/v1/library/scan/report").expect(t, http.StatusNotFound)

	for _, name := range []string{"Arrival (2016)", "Dune (2021)", "Heat (1995)"} {
		libVideo(t, libDir(t, e.root, name), name+".1080p.WEB-DL.mkv")
	}
	libScan(t, e)

	var report struct {
		RootsScanned   int `json:"rootsScanned"`
		UnmatchedTotal int `json:"unmatchedTotal"`
		UnmatchedDirs  []struct {
			Name string `json:"name"`
			Path string `json:"path"`
		} `json:"unmatchedDirs"`
	}
	e.get(t, "/api/v1/library/scan/report").expect(t, http.StatusOK).into(t, &report)
	if report.RootsScanned != 1 || report.UnmatchedTotal != 3 {
		t.Fatalf("report = %+v", report)
	}
	if len(report.UnmatchedDirs) != 3 {
		t.Errorf("the default limit is 25, so all three belong in the window: %+v", report.UnmatchedDirs)
	}

	// limit=0 is "count them, do not list them".
	e.get(t, "/api/v1/library/scan/report?limit=0").expect(t, http.StatusOK).into(t, &report)
	if len(report.UnmatchedDirs) != 0 || report.UnmatchedTotal != 3 {
		t.Errorf("limit=0 = %+v; the total must survive the cap", report)
	}

	// A negative limit clamps to zero rather than panicking on the slice.
	e.get(t, "/api/v1/library/scan/report?limit=-5").expect(t, http.StatusOK).into(t, &report)
	if len(report.UnmatchedDirs) != 0 {
		t.Errorf("limit=-5 listed %d dirs", len(report.UnmatchedDirs))
	}

	e.get(t, "/api/v1/library/scan/report?limit=1").expect(t, http.StatusOK).into(t, &report)
	if len(report.UnmatchedDirs) != 1 || report.UnmatchedTotal != 3 {
		t.Errorf("limit=1 = %+v", report)
	}
}

// The review queue is the paginated view of the same unmatched folders, with
// counts computed over the whole queue so the tab labels stay still while the
// user narrows the list.
func TestLibReviewQueuePaginatesAndFilters(t *testing.T) {
	e := newAPIEnv(t)
	for _, name := range []string{"Arrival (2016)", "Dune (2021)", "Heat (1995)"} {
		libVideo(t, libDir(t, e.root, name), name+".1080p.WEB-DL.mkv")
	}
	libScan(t, e)

	var page struct {
		Items []struct {
			Path        string `json:"path"`
			Name        string `json:"name"`
			ParsedTitle string `json:"parsedTitle"`
			ParsedYear  int    `json:"parsedYear"`
			Confidence  string `json:"confidence"`
			Candidates  []struct {
				Title string `json:"title"`
			} `json:"candidates"`
		} `json:"items"`
		Total  int `json:"total"`
		Offset int `json:"offset"`
		Limit  int `json:"limit"`
		Counts struct {
			Total   int `json:"total"`
			None    int `json:"none"`
			Unknown int `json:"unknown"`
		} `json:"counts"`
	}
	e.get(t, "/api/v1/library/review").expect(t, http.StatusOK).into(t, &page)
	if page.Total != 3 || page.Counts.Total != 3 {
		t.Fatalf("page = %+v", page)
	}
	// Nothing has searched these folders yet, and the mixed root never
	// resolved a kind for them.
	if page.Counts.None != 3 || page.Counts.Unknown != 3 {
		t.Errorf("counts = %+v", page.Counts)
	}
	// The row says what the folder was read as, not just the raw name.
	for _, it := range page.Items {
		if it.ParsedTitle == "" || it.Candidates == nil {
			t.Errorf("item = %+v; candidates must serialize as an array", it)
		}
	}

	// The text filter narrows the list but never the counts.
	e.get(t, "/api/v1/library/review?q=dune").expect(t, http.StatusOK).into(t, &page)
	if page.Total != 1 || page.Counts.Total != 3 {
		t.Errorf("filtered page = %+v; counts must stay over the whole queue", page)
	}

	// Paging is honoured as sent, and echoed back so the client can trust it.
	e.get(t, "/api/v1/library/review?limit=2&offset=2").expect(t, http.StatusOK).into(t, &page)
	if page.Limit != 2 || page.Offset != 2 || len(page.Items) != 1 {
		t.Errorf("paged = limit %d offset %d items %d", page.Limit, page.Offset, len(page.Items))
	}

	// A kind filter that matches nothing is an empty array, never null.
	body := e.get(t, "/api/v1/library/review?kind=book").expect(t, http.StatusOK).Body.String()
	if !strings.Contains(body, `"items":[]`) {
		t.Errorf("empty review page = %s", body)
	}
}

// ---- ignored directories (ADR 0009 §4) ----

// A dismissal is keyed by exact path and survives rescans; without that, every
// non-media folder under a root is re-offered forever. It must also leave the
// review queue immediately, rather than after the next scan.
func TestLibIgnoredDirsRoundTripAndLeaveTheReviewQueue(t *testing.T) {
	e := newAPIEnv(t)
	// Not "Extras": that name is on the built-in skip list and never reaches
	// the review queue in the first place, which would make this test pass
	// without the dismissal doing anything.
	junk := libDir(t, e.root, "CameraRoll")
	libVideo(t, libDir(t, e.root, "Dune (2021)"), "Dune.2021.1080p.WEB-DL.mkv")
	libVideo(t, junk, "clip.mkv")
	libScan(t, e)

	e.post(t, "/api/v1/library/scan/ignored",
		`{"path":"`+junk+`","reason":"bonus material"}`).expect(t, http.StatusNoContent)

	var rows []struct {
		Path      string `json:"path"`
		Reason    string `json:"reason"`
		IgnoredAt string `json:"ignoredAt"`
	}
	e.get(t, "/api/v1/library/scan/ignored").expect(t, http.StatusOK).into(t, &rows)
	if len(rows) != 1 || rows[0].Path != junk || rows[0].Reason != "bonus material" {
		t.Fatalf("ignored list = %+v", rows)
	}
	if rows[0].IgnoredAt == "" {
		t.Error("a dismissal with no timestamp cannot be shown or ordered")
	}

	var page struct {
		Total int `json:"total"`
	}
	e.get(t, "/api/v1/library/review").expect(t, http.StatusOK).into(t, &page)
	if page.Total != 1 {
		t.Errorf("review total = %d; the dismissed folder is still being offered", page.Total)
	}

	// Undoing it puts the folder back in front of the user — a dismissal
	// nobody can reverse is a trap rather than a feature.
	e.del(t, "/api/v1/library/scan/ignored?path="+junk).expect(t, http.StatusNoContent)
	e.get(t, "/api/v1/library/scan/ignored").expect(t, http.StatusOK).into(t, &rows)
	if len(rows) != 0 {
		t.Errorf("un-ignore left %+v behind", rows)
	}
	// Undoing the dismissal does not resurrect the row on its own: IgnoreDir
	// also pruned it from the stored scan report, and the report is what the
	// queue is built from. The folder comes back on the next scan, which is
	// what the service documents — asserted so a change to either half is
	// noticed rather than discovered as "un-ignore does nothing".
	e.get(t, "/api/v1/library/review").expect(t, http.StatusOK).into(t, &page)
	if page.Total != 1 {
		t.Errorf("review total straight after un-ignore = %d, want 1", page.Total)
	}
	libScan(t, e)
	e.get(t, "/api/v1/library/review").expect(t, http.StatusOK).into(t, &page)
	if page.Total != 2 {
		t.Errorf("review total after the next scan = %d, want the folder back", page.Total)
	}

	// A relative path is refused: dismissals are matched by exact path, and a
	// relative one could never match anything a scan produced.
	e.post(t, "/api/v1/library/scan/ignored", `{"path":"CameraRoll"}`).expect(t, http.StatusBadRequest)
	// The query parameter is required, so a bare DELETE never reaches the
	// handler.
	e.del(t, "/api/v1/library/scan/ignored").expect(t, http.StatusBadRequest)
}

// ---- adoption (ADR 0010) ----

// Adoption with nothing to adopt still answers with three arrays rather than
// three nulls. The panel renders all three unconditionally, and a null here is
// a blank page with a console error.
func TestLibAdoptionWithNothingOutstandingIsEmptyArrays(t *testing.T) {
	e := newAPIEnv(t)
	body := e.post(t, "/api/v1/library/adopt", "").expect(t, http.StatusOK).Body.String()
	for _, field := range []string{`"adopted":[]`, `"review":[]`, `"failures":[]`} {
		if !strings.Contains(body, field) {
			t.Errorf("adopt result = %s, want %s", body, field)
		}
	}
	// The exact-only variant answers the same shape from the same empty queue.
	if got := e.post(t, "/api/v1/library/adopt/exact", "").
		expect(t, http.StatusOK).Body.String(); !strings.Contains(got, `"review":[]`) {
		t.Errorf("adopt/exact = %s", got)
	}
}

// The first pass over a new root proposes and writes nothing (ADR 0010 §5),
// so everything lands in review with the candidates a provider search found.
// Confirming the root is what releases that guard, and it is reversible.
func TestLibAdoptionFirstPassProposesAndConfirmReleasesIt(t *testing.T) {
	e := newAPIEnv(t)
	libVideo(t, libDir(t, e.root, "Fight Club (1999)"), "Fight.Club.1999.1080p.WEB-DL.mkv")
	libScan(t, e)

	var res struct {
		Adopted []struct {
			Path string `json:"path"`
		} `json:"adopted"`
		Review []struct {
			Path       string `json:"path"`
			Confidence string `json:"confidence"`
			Candidates []struct {
				Kind   string `json:"kind"`
				TmdbID int64  `json:"tmdbId"`
				Title  string `json:"title"`
			} `json:"candidates"`
		} `json:"review"`
		Failures []string `json:"failures"`
	}
	e.post(t, "/api/v1/library/adopt", "").expect(t, http.StatusOK).into(t, &res)
	if len(res.Adopted) != 0 {
		t.Errorf("a first pass must write nothing, adopted = %+v", res.Adopted)
	}
	if len(res.Review) != 1 {
		t.Fatalf("review = %+v, want the one unmatched folder", res.Review)
	}
	if len(res.Review[0].Candidates) == 0 {
		t.Error("a reviewed proposal with no candidates is a row the user cannot act on")
	}
	// Nothing was written, so the library is still empty.
	var items []libSummary
	e.get(t, "/api/v1/library").expect(t, http.StatusOK).into(t, &items)
	if len(items) != 0 {
		t.Errorf("the first pass wrote %d items", len(items))
	}

	var roots []libRoot
	e.get(t, "/api/v1/rootfolders").expect(t, http.StatusOK).into(t, &roots)
	rootID := strconv.FormatInt(roots[0].ID, 10)
	e.post(t, "/api/v1/library/adopt/confirm", `{"rootFolderId":`+rootID+`}`).
		expect(t, http.StatusNoContent)
	e.get(t, "/api/v1/rootfolders").expect(t, http.StatusOK).into(t, &roots)
	if !roots[0].AutoAdopt {
		t.Error("confirming a root must show up as autoAdopt, or the UI describes state it cannot see")
	}
	// Reversible: a trust decision you can only ever switch on is one users
	// are right to hesitate over.
	e.post(t, "/api/v1/library/adopt/confirm", `{"rootFolderId":`+rootID+`,"autoAdopt":false}`).
		expect(t, http.StatusNoContent)
	e.get(t, "/api/v1/rootfolders").expect(t, http.StatusOK).into(t, &roots)
	if roots[0].AutoAdopt {
		t.Error("autoAdopt could not be switched back off")
	}
}

// Accepting one proposed match without leaving the review window. The
// interesting part is the refusals: this endpoint takes a path from a JSON
// body, so it is the one place the library could be aimed at an arbitrary
// folder on the host.
func TestLibAdoptOne(t *testing.T) {
	e := newAPIEnv(t)
	dir := libDir(t, e.root, "Fight Club (1999)")
	libVideo(t, dir, "Fight.Club.1999.1080p.WEB-DL.mkv")
	libScan(t, e)

	// Outside every registered root: refused, and never enumerated.
	e.post(t, "/api/v1/library/adopt/one",
		`{"path":"`+t.TempDir()+`","kind":"movie","tmdbId":550}`).
		expect(t, http.StatusBadRequest)
	// A relative path never resolves to a root either.
	e.post(t, "/api/v1/library/adopt/one", `{"path":"Fight Club (1999)","kind":"movie","tmdbId":550}`).
		expect(t, http.StatusBadRequest)
	// A mixed root resolves no kind, so the caller has to say which one the
	// chosen candidate is.
	rr := e.post(t, "/api/v1/library/adopt/one", `{"path":"`+dir+`","tmdbId":550}`).
		expect(t, http.StatusBadRequest)
	if !strings.Contains(rr.Body.String(), "media kind") {
		t.Errorf("the refusal must say what is missing: %s", rr.Body.String())
	}

	e.post(t, "/api/v1/library/adopt/one",
		`{"path":"`+dir+`","kind":"movie","tmdbId":550,"title":"Fight Club","year":1999}`).
		expect(t, http.StatusNoContent)

	var items []libSummary
	e.get(t, "/api/v1/library").expect(t, http.StatusOK).into(t, &items)
	if len(items) != 1 || items[0].Path != dir {
		t.Fatalf("adopted library = %+v", items)
	}

	// A second folder claiming the same title conflicts with the first, and
	// the conflict gets its own status: the UI offers "move it here" rather
	// than reading it as a flat rejection.
	second := libDir(t, e.root, "Fight Club (1999) [remux]")
	libVideo(t, second, "Fight.Club.1999.2160p.Remux.mkv")
	e.post(t, "/api/v1/library/adopt/one",
		`{"path":"`+second+`","kind":"movie","tmdbId":550,"title":"Fight Club","year":1999}`).
		expect(t, http.StatusConflict)
	// force says "I know, move it" — the same request then goes through.
	e.post(t, "/api/v1/library/adopt/one",
		`{"path":"`+second+`","kind":"movie","tmdbId":550,"title":"Fight Club","year":1999,"force":true}`).
		expect(t, http.StatusNoContent)
}
