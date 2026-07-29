package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	apigen "github.com/pjunod/monarr/internal/api/gen"
)

// The acquisition half of /api/v1 — indexers, clients, the queue, search and
// grab, custom formats, import lists, the blocklist, the calendar, wanted,
// and notifiers.
//
// These handlers are the ones that were unreachable from a test until the
// wired harness existed, which is why they are also the ones where a
// validation branch could rot unnoticed. Every test below either asserts what
// a response SAYS (not merely that it answered) or pins a refusal, because a
// refusal that quietly becomes an acceptance is how a bad indexer config, an
// unparseable custom format, or a grab with nowhere to send it gets written
// to the database.

// ---- helpers ----

// acqMessage pulls the reason out of an error body. Every refusal here is
// expected to explain itself; a 400 with an empty message is a dead end for
// whoever is staring at the form.
func acqMessage(t *testing.T, rr *httptestRecorder) string {
	t.Helper()
	var e apigen.Error
	rr.into(t, &e)
	if e.Message == "" {
		t.Fatalf("refusal carried no message: %s", rr.Body.String())
	}
	return e.Message
}

// acqAddIndexer saves one enabled indexer and returns its id — the
// precondition for anything that searches.
func acqAddIndexer(t *testing.T, e *apiEnv) int64 {
	t.Helper()
	rr := e.post(t, "/api/v1/indexers",
		`{"name":"nzb.invalid","url":"idx.invalid","protocol":"torrent","apiKey":"secret"}`).
		expect(t, http.StatusCreated)
	var out apigen.Indexer
	rr.into(t, &out)
	return out.Id
}

// acqAddClient saves one enabled download client of the given type and
// returns its id. The type decides the protocol a grab can be routed on
// (qbittorrent → torrent, sabnzbd → usenet), which is the whole reason a
// test picks one deliberately.
func acqAddClient(t *testing.T, e *apiEnv, typ string) int64 {
	t.Helper()
	rr := e.post(t, "/api/v1/downloadclients",
		`{"type":"`+typ+`","name":"`+typ+`","url":"box.invalid","password":"hunter2"}`).
		expect(t, http.StatusCreated)
	var out apigen.DownloadClientConfig
	rr.into(t, &out)
	return out.Id
}

// acqGrab pushes the harness's release at the given item and returns the
// queue row id, so the queue tests have a row to act on.
func acqGrab(t *testing.T, e *apiEnv, itemID int64) int64 {
	t.Helper()
	rr := e.post(t, "/api/v1/grab", `{
		"mediaItemId":`+acqItoa(itemID)+`,
		"title":"Fight.Club.1999.1080p.WEB-DL.x264-TEST",
		"downloadUrl":"http://indexer.invalid/1",
		"protocol":"torrent","indexer":"nzb.invalid","size":8589934592}`).
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

func acqItoa(v int64) string {
	digits := ""
	if v == 0 {
		return "0"
	}
	for v > 0 {
		digits = string(rune('0'+v%10)) + digits
		v /= 10
	}
	return digits
}

// acqSetReleaseDate dates an item so it lands in a calendar window. The
// metadata stub has no release date, and the calendar selects on that column
// alone.
func acqSetReleaseDate(t *testing.T, e *apiEnv, id int64, date string) {
	t.Helper()
	if _, err := e.db.W.ExecContext(context.Background(),
		`UPDATE media_items SET release_date = ? WHERE id = ?`, date, id); err != nil {
		t.Fatal(err)
	}
}

// ---- indexers ----

// An indexer survives the round trip with its URL completed and its
// defaults filled in, and disappears on delete.
//
// The URL is the interesting part: a person types a host, and the stored
// config has to be the thing Monarr will actually dial. A create that echoed
// back the raw input would leave the settings page showing something
// different from what the searches use.
func TestAcqIndexerRoundTrip(t *testing.T) {
	e := newAPIEnv(t)

	rr := e.post(t, "/api/v1/indexers",
		`{"name":"nzb.invalid","url":"idx.invalid","protocol":"torrent","apiKey":"secret","categories":[2000]}`).
		expect(t, http.StatusCreated)
	var created apigen.Indexer
	rr.into(t, &created)
	if created.Id == 0 {
		t.Fatalf("created indexer has no id: %s", rr.Body.String())
	}
	if created.Url != "http://idx.invalid" {
		t.Errorf("url = %q, want the completed http://idx.invalid", created.Url)
	}
	if created.Enabled == nil || !*created.Enabled {
		t.Error("a new indexer must default to enabled; a disabled one searches nothing")
	}
	if created.Categories == nil || len(*created.Categories) != 1 || (*created.Categories)[0] != 2000 {
		t.Errorf("categories = %v, want the submitted [2000]", created.Categories)
	}

	var list []apigen.Indexer
	e.get(t, "/api/v1/indexers").expect(t, http.StatusOK).into(t, &list)
	if len(list) != 1 || list[0].Id != created.Id {
		t.Fatalf("list = %+v, want the one indexer just created", list)
	}
	if list[0].ApiKey == nil || *list[0].ApiKey != "secret" {
		t.Errorf("apiKey = %v; the edit form re-submits what it was given", list[0].ApiKey)
	}

	e.del(t, "/api/v1/indexers/"+acqItoa(created.Id)).expect(t, http.StatusNoContent)
	list = nil
	e.get(t, "/api/v1/indexers").expect(t, http.StatusOK).into(t, &list)
	if len(list) != 0 {
		t.Errorf("indexer survived its delete: %+v", list)
	}
}

// Half-filled indexer forms are refused before anything is stored. A saved
// indexer with no URL or an unknown protocol is a search that fails on every
// run for a reason nobody can see from the settings page.
func TestAcqAddIndexerRefusesIncompleteInput(t *testing.T) {
	e := newAPIEnv(t)
	for name, body := range map[string]string{
		"no name":          `{"name":"","url":"idx.invalid","protocol":"torrent"}`,
		"no url":           `{"name":"nzb","url":"","protocol":"torrent"}`,
		"unknown protocol": `{"name":"nzb","url":"idx.invalid","protocol":"carrier-pigeon"}`,
		"not json":         `{`,
	} {
		t.Run(name, func(t *testing.T) {
			rr := e.post(t, "/api/v1/indexers", body).expect(t, http.StatusBadRequest)
			acqMessage(t, rr)
		})
	}
	var list []apigen.Indexer
	e.get(t, "/api/v1/indexers").expect(t, http.StatusOK).into(t, &list)
	if len(list) != 0 {
		t.Errorf("a refused indexer was stored anyway: %+v", list)
	}
}

// An indexer saved switched off is stored switched off, and a search with
// only that indexer is the same "no indexers" answer as having none at all.
//
// Disabled has to mean disabled all the way down. An indexer drawn greyed
// out on the settings page but still queried is one half of that confusion;
// the other is a search that appears to work because a disabled row was
// counted as a live one.
func TestAcqDisabledIndexerIsNotSearched(t *testing.T) {
	e := newAPIEnv(t)

	rr := e.post(t, "/api/v1/indexers",
		`{"name":"off","url":"idx.invalid","protocol":"torrent","enabled":false}`).
		expect(t, http.StatusCreated)
	var created apigen.Indexer
	rr.into(t, &created)
	if created.Enabled == nil || *created.Enabled {
		t.Errorf("enabled = %v, want the submitted false", created.Enabled)
	}

	item := e.addMovie(t)
	e.get(t, "/api/v1/library/"+acqItoa(item)+"/releases").
		expect(t, http.StatusServiceUnavailable)
}

// Both Test buttons reach the indexer: the one on the add form (unsaved
// config in the body) and the one on a saved row (config read back by id).
// The by-id path exists so nobody has to retype an API key to check a
// connection, and it is the one that regresses silently.
func TestAcqIndexerTestEndpoints(t *testing.T) {
	e := newAPIEnv(t)

	e.post(t, "/api/v1/indexers/test",
		`{"name":"nzb","url":"idx.invalid","protocol":"torrent"}`).expect(t, http.StatusOK)

	id := acqAddIndexer(t, e)
	e.post(t, "/api/v1/indexers/"+acqItoa(id)+"/test", "").expect(t, http.StatusOK)

	// A test against an id that is gone is a 404, not a green tick.
	e.post(t, "/api/v1/indexers/9999/test", "").expect(t, http.StatusNotFound)

	rr := e.post(t, "/api/v1/indexers/test", `{`).expect(t, http.StatusBadRequest)
	acqMessage(t, rr)
}

// ---- download clients ----

// A download client round trips with its URL completed to the type's default
// port, its category defaulted, and its password masked on the way out.
//
// The mask is not cosmetic: the settings page renders whatever the API
// returns, and a client that echoed the real password would put it in the
// DOM of every browser that opens the page.
func TestAcqDownloadClientRoundTrip(t *testing.T) {
	e := newAPIEnv(t)

	rr := e.post(t, "/api/v1/downloadclients",
		`{"type":"qbittorrent","name":"qbit","url":"box.invalid","username":"admin","password":"hunter2"}`).
		expect(t, http.StatusCreated)
	var created apigen.DownloadClientConfig
	rr.into(t, &created)
	if created.Url != "http://box.invalid:8080" {
		t.Errorf("url = %q, want qbittorrent's default port filled in", created.Url)
	}
	if created.Password == nil || *created.Password == "hunter2" {
		t.Errorf("password = %v, want it masked before it leaves the server", created.Password)
	}
	if created.Category == nil || *created.Category != "monarr" {
		t.Errorf("category = %v, want the default \"monarr\"", created.Category)
	}
	if created.Mode == nil || *created.Mode != "poll" {
		t.Errorf("mode = %v, want poll when the form has no opinion", created.Mode)
	}
	// Migration 0025's split, restated on every create: a torrent is still
	// seeding, so its payload is not Monarr's to delete on a guess.
	if created.RemoveCompleted == nil || *created.RemoveCompleted {
		t.Errorf("removeCompleted = %v for a torrent client, want off", created.RemoveCompleted)
	}

	var list []apigen.DownloadClientConfig
	e.get(t, "/api/v1/downloadclients").expect(t, http.StatusOK).into(t, &list)
	if len(list) != 1 || list[0].Id != created.Id {
		t.Fatalf("list = %+v, want the one client just created", list)
	}

	e.del(t, "/api/v1/downloadclients/"+acqItoa(created.Id)).expect(t, http.StatusNoContent)
	list = nil
	e.get(t, "/api/v1/downloadclients").expect(t, http.StatusOK).into(t, &list)
	if len(list) != 0 {
		t.Errorf("client survived its delete: %+v", list)
	}
}

// The same create against a usenet client takes the opposite default: the
// client has no obligation to the payload once the download is done, so
// leaving it behind is a second full copy nobody asked for.
func TestAcqAddUsenetClientDefaultsToRemovingCompleted(t *testing.T) {
	e := newAPIEnv(t)
	rr := e.post(t, "/api/v1/downloadclients",
		`{"type":"sabnzbd","name":"sab","url":"box.invalid"}`).expect(t, http.StatusCreated)
	var created apigen.DownloadClientConfig
	rr.into(t, &created)
	if created.RemoveCompleted == nil || !*created.RemoveCompleted {
		t.Errorf("removeCompleted = %v for a usenet client, want on", created.RemoveCompleted)
	}
}

// Editing a client with the masked placeholder in the password field keeps
// the stored secret. The UI cannot show the real password, so it sends back
// the dots — and an edit that took those literally would blank the
// credential every time somebody renamed a client.
func TestAcqUpdateDownloadClientKeepsTheStoredPassword(t *testing.T) {
	e := newAPIEnv(t)
	id := acqAddClient(t, e, "qbittorrent")

	e.put(t, "/api/v1/downloadclients/"+acqItoa(id),
		`{"type":"qbittorrent","name":"renamed","url":"box.invalid","password":"••••"}`).
		expect(t, http.StatusOK)

	cfg, err := e.db.GetDownloadClient(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Password != "hunter2" {
		t.Errorf("stored password = %q, want the original preserved", cfg.Password)
	}
	if cfg.Name != "renamed" {
		t.Errorf("name = %q, want the edit applied", cfg.Name)
	}
}

// The optional half of a client config survives the round trip, and a path
// mapping with a blank half is dropped rather than stored.
//
// A half-typed mapping is form noise — somebody clicked "add row" and moved
// on. Stored, it would rewrite every completed path against an empty prefix,
// which turns a working import into a file-not-found on a path nobody
// recognises.
func TestAcqDownloadClientCarriesOptionsAndDropsHalfTypedMappings(t *testing.T) {
	e := newAPIEnv(t)

	rr := e.post(t, "/api/v1/downloadclients", `{
		"type":"nzbd","name":"nzbd","url":"box.invalid","username":"admin",
		"category":"films","enabled":false,"manualApproval":true,"mode":"push",
		"removeCompleted":false,
		"pathMappings":[{"remote":"/downloads","local":"/mnt/dl"},{"remote":"  ","local":"/mnt/x"},{"remote":"/y","local":""}]}`).
		expect(t, http.StatusCreated)
	var created apigen.DownloadClientConfig
	rr.into(t, &created)

	if created.Category == nil || *created.Category != "films" {
		t.Errorf("category = %v, want the submitted override", created.Category)
	}
	if created.Enabled == nil || *created.Enabled {
		t.Errorf("enabled = %v, want the submitted false", created.Enabled)
	}
	if created.ManualApproval == nil || !*created.ManualApproval {
		t.Errorf("manualApproval = %v, want the submitted true", created.ManualApproval)
	}
	if created.Mode == nil || *created.Mode != "push" {
		t.Errorf("mode = %v, want the submitted push", created.Mode)
	}
	// A usenet client that was explicitly told not to remove must not be
	// overridden by the type's default.
	if created.RemoveCompleted == nil || *created.RemoveCompleted {
		t.Errorf("removeCompleted = %v, want the submitted false to win over the usenet default", created.RemoveCompleted)
	}
	if created.PathMappings == nil || len(*created.PathMappings) != 1 {
		t.Fatalf("pathMappings = %v, want only the one complete mapping kept", created.PathMappings)
	}
	if m := (*created.PathMappings)[0]; m.Remote != "/downloads" || m.Local != "/mnt/dl" {
		t.Errorf("mapping = %+v, want the submitted /downloads → /mnt/dl", m)
	}
	if created.Password == nil || *created.Password != "" {
		t.Errorf("password = %v, want an empty mask when none was set", created.Password)
	}
}

// Client forms are refused for the same reasons indexer forms are, and an
// edit to a client that no longer exists is a 404 rather than a silent
// insert of a brand new one.
func TestAcqDownloadClientRefusals(t *testing.T) {
	e := newAPIEnv(t)
	for name, body := range map[string]string{
		"no name":      `{"type":"qbittorrent","name":"","url":"box.invalid"}`,
		"no url":       `{"type":"qbittorrent","name":"qbit","url":""}`,
		"unknown type": `{"type":"emule","name":"qbit","url":"box.invalid"}`,
		"not json":     `{`,
	} {
		t.Run("create "+name, func(t *testing.T) {
			acqMessage(t, e.post(t, "/api/v1/downloadclients", body).expect(t, http.StatusBadRequest))
		})
		t.Run("update "+name, func(t *testing.T) {
			acqMessage(t, e.put(t, "/api/v1/downloadclients/1", body).expect(t, http.StatusBadRequest))
		})
	}
	e.put(t, "/api/v1/downloadclients/9999",
		`{"type":"qbittorrent","name":"qbit","url":"box.invalid"}`).expect(t, http.StatusNotFound)
}

// Both client Test buttons, for the same reason as the indexer pair: the
// by-id probe is what checks the config that actually runs.
func TestAcqDownloadClientTestEndpoints(t *testing.T) {
	e := newAPIEnv(t)

	e.post(t, "/api/v1/downloadclients/test",
		`{"type":"qbittorrent","name":"qbit","url":"box.invalid"}`).expect(t, http.StatusOK)

	id := acqAddClient(t, e, "qbittorrent")
	e.post(t, "/api/v1/downloadclients/"+acqItoa(id)+"/test", "").expect(t, http.StatusOK)
	e.post(t, "/api/v1/downloadclients/9999/test", "").expect(t, http.StatusNotFound)

	acqMessage(t, e.post(t, "/api/v1/downloadclients/test", `{`).expect(t, http.StatusBadRequest))
}

// ---- release search ----

// An interactive search returns the ranked candidate with its verdict
// attached rather than a filtered list.
//
// Rejections are carried, never dropped: "no results" and "twelve results,
// all below your floor" are different answers, and only one of them means
// the indexer is the problem.
func TestAcqSearchReleasesReturnsJudgedCandidates(t *testing.T) {
	e := newAPIEnv(t)
	acqAddIndexer(t, e)
	item := e.addMovie(t)

	var cands []apigen.ReleaseCandidate
	e.get(t, "/api/v1/library/"+acqItoa(item)+"/releases").
		expect(t, http.StatusOK).into(t, &cands)
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want the one the indexer answers with: %+v", len(cands), cands)
	}
	c := cands[0]
	if c.Title != "Fight.Club.1999.1080p.WEB-DL.x264-TEST" {
		t.Errorf("title = %q, want the release the indexer returned", c.Title)
	}
	if c.Indexer != "nzb.invalid" {
		t.Errorf("indexer = %q, want the release attributed to the indexer it came from", c.Indexer)
	}
	if c.Size != 8<<30 {
		t.Errorf("size = %d, want the advertised 8GiB", c.Size)
	}
	if c.Quality == "" {
		t.Error("quality is empty; the candidate list is unsortable without it")
	}
	if c.Rejections == nil {
		t.Error("rejections is null; the UI iterates it and null is a blank row")
	}
}

// Searching with no enabled indexer is a 503, not an empty list. An empty
// list reads as "nothing out there tonight", which sends somebody hunting
// for a release that was never looked for.
func TestAcqSearchReleasesWithoutIndexersIsUnavailable(t *testing.T) {
	e := newAPIEnv(t)
	item := e.addMovie(t)

	rr := e.get(t, "/api/v1/library/"+acqItoa(item)+"/releases").
		expect(t, http.StatusServiceUnavailable)
	if msg := acqMessage(t, rr); !strings.Contains(msg, "indexer") {
		t.Errorf("message = %q, want it to name the missing indexers", msg)
	}
}

// A search against an item that is not in the library is a 404, decided
// before any indexer is contacted.
func TestAcqSearchReleasesUnknownItem(t *testing.T) {
	e := newAPIEnv(t)
	acqAddIndexer(t, e)
	e.get(t, "/api/v1/library/9999/releases").expect(t, http.StatusNotFound)
}

// ---- grab ----

// A grab records a queue row that describes the download, and the queue
// reports it back with the state and the protocol it was sent on.
func TestAcqGrabEntersTheQueue(t *testing.T) {
	e := newAPIEnv(t)
	acqAddClient(t, e, "qbittorrent")
	item := e.addMovie(t)

	id := acqGrab(t, e, item)

	var queue []apigen.QueueItem
	e.get(t, "/api/v1/queue").expect(t, http.StatusOK).into(t, &queue)
	if len(queue) != 1 {
		t.Fatalf("queue has %d rows, want the one just grabbed: %+v", len(queue), queue)
	}
	q := queue[0]
	if q.Id != id {
		t.Errorf("queue row id = %d, want the id grab returned (%d)", q.Id, id)
	}
	if q.MediaItemId != item {
		t.Errorf("mediaItemId = %d, want the item it was grabbed for (%d)", q.MediaItemId, item)
	}
	if q.Title != "Fight.Club.1999.1080p.WEB-DL.x264-TEST" {
		t.Errorf("title = %q, want the release title", q.Title)
	}
	if q.State != "grabbed" {
		t.Errorf("state = %q, want grabbed straight after the client accepted it", q.State)
	}
	if q.Protocol != "torrent" {
		t.Errorf("protocol = %q, want the protocol it was routed on", q.Protocol)
	}
	if q.Quality == "" {
		t.Error("quality is empty; the row cannot say what it is fetching")
	}
}

// A grab with no enabled client for that protocol is refused with the
// protocol named. Accepting it would leave a queue row for a download no
// client has, which is the failure the row ordering in Grab exists to avoid.
func TestAcqGrabWithoutAClientForTheProtocol(t *testing.T) {
	e := newAPIEnv(t)
	// A usenet client only: the torrent grab below has nowhere to go.
	acqAddClient(t, e, "sabnzbd")
	item := e.addMovie(t)

	rr := e.post(t, "/api/v1/grab", `{
		"mediaItemId":`+acqItoa(item)+`,
		"title":"Fight.Club.1999.1080p.WEB-DL.x264-TEST",
		"downloadUrl":"http://indexer.invalid/1","protocol":"torrent"}`).
		expect(t, http.StatusBadRequest)
	if msg := acqMessage(t, rr); !strings.Contains(msg, "torrent") {
		t.Errorf("message = %q, want it to name the protocol with no client", msg)
	}

	var queue []apigen.QueueItem
	e.get(t, "/api/v1/queue").expect(t, http.StatusOK).into(t, &queue)
	if len(queue) != 0 {
		t.Errorf("a refused grab left a queue row behind: %+v", queue)
	}
}

// A grab needs a title and a download URL; a grab for an item that is not in
// the library is a 404. Both are refused before a client is asked to take
// anything, so a mistyped request cannot start a download nothing owns.
func TestAcqGrabRefusals(t *testing.T) {
	e := newAPIEnv(t)
	acqAddClient(t, e, "qbittorrent")
	item := e.addMovie(t)

	for name, body := range map[string]string{
		"no title": `{"mediaItemId":` + acqItoa(item) +
			`,"title":"","downloadUrl":"http://indexer.invalid/1","protocol":"torrent"}`,
		"no download url": `{"mediaItemId":` + acqItoa(item) +
			`,"title":"Some.Release","downloadUrl":"","protocol":"torrent"}`,
		"not json": `{`,
	} {
		t.Run(name, func(t *testing.T) {
			acqMessage(t, e.post(t, "/api/v1/grab", body).expect(t, http.StatusBadRequest))
		})
	}

	e.post(t, "/api/v1/grab", `{"mediaItemId":9999,"title":"Some.Release",
		"downloadUrl":"http://indexer.invalid/1","protocol":"torrent"}`).
		expect(t, http.StatusNotFound)
}

// ---- queue ----

// Importing a row that has no path yet is a 400 that says so, and the row
// stays in the queue.
//
// This is the "Import" button pressed too early. It has to be recoverable:
// a 500, or a row silently marked failed, would make the operator's next
// move a manual import of a download that was going to arrive on its own.
func TestAcqImportQueueItemBeforeAnythingLanded(t *testing.T) {
	e := newAPIEnv(t)
	acqAddClient(t, e, "qbittorrent")
	item := e.addMovie(t)
	id := acqGrab(t, e, item)

	rr := e.post(t, "/api/v1/queue/"+acqItoa(id)+"/import", "").expect(t, http.StatusBadRequest)
	if msg := acqMessage(t, rr); !strings.Contains(msg, "path") {
		t.Errorf("message = %q, want it to explain that no path was recorded yet", msg)
	}

	var queue []apigen.QueueItem
	e.get(t, "/api/v1/queue").expect(t, http.StatusOK).into(t, &queue)
	if len(queue) != 1 {
		t.Errorf("a refused import removed the row; it must stay retryable: %+v", queue)
	}
}

// Importing a row that is not there is a 404, kept distinct from the 400
// above: one means "not yet", the other means "never".
func TestAcqImportQueueItemUnknownRow(t *testing.T) {
	e := newAPIEnv(t)
	e.post(t, "/api/v1/queue/9999/import", "").expect(t, http.StatusNotFound)
}

// Blocklisting from the queue does both halves of what the button promises:
// the row leaves the queue and the release is on the blocklist by name, so a
// later search cannot pick the same bad copy again.
func TestAcqBlocklistQueueItemBansTheRelease(t *testing.T) {
	e := newAPIEnv(t)
	acqAddClient(t, e, "qbittorrent")
	item := e.addMovie(t)
	id := acqGrab(t, e, item)

	e.post(t, "/api/v1/queue/"+acqItoa(id)+"/blocklist", "").expect(t, http.StatusNoContent)

	var entries []apigen.BlocklistEntry
	e.get(t, "/api/v1/blocklist").expect(t, http.StatusOK).into(t, &entries)
	if len(entries) != 1 {
		t.Fatalf("blocklist has %d entries, want the release just banned: %+v", len(entries), entries)
	}
	if entries[0].ReleaseTitle != "Fight.Club.1999.1080p.WEB-DL.x264-TEST" {
		t.Errorf("releaseTitle = %q, want the banned release", entries[0].ReleaseTitle)
	}
	if entries[0].MediaItemId != item {
		t.Errorf("mediaItemId = %d, want the item it was grabbed for (%d)", entries[0].MediaItemId, item)
	}
	if entries[0].Reason == "" {
		t.Error("reason is empty; the blocklist page cannot say why this was banned")
	}

	// The queue row stays, failed, carrying the reason. A row that vanished
	// would leave the operator with a movie that is still missing and no
	// trace of the thing they just rejected.
	var queue []apigen.QueueItem
	e.get(t, "/api/v1/queue").expect(t, http.StatusOK).into(t, &queue)
	if len(queue) != 1 {
		t.Fatalf("queue = %+v, want the blocklisted row still visible", queue)
	}
	if queue[0].State != "failed" {
		t.Errorf("state = %q, want failed after a blocklist", queue[0].State)
	}
	if queue[0].Error == nil || *queue[0].Error == "" {
		t.Errorf("error = %v, want the row to say why it failed", queue[0].Error)
	}
	if queue[0].Handoff == nil || len(*queue[0].Handoff) == 0 {
		t.Error("handoff is empty; the trace is what explains where a download stopped")
	}

	// Un-banning it makes the release grabbable again.
	e.del(t, "/api/v1/blocklist/"+acqItoa(entries[0].Id)).expect(t, http.StatusNoContent)
	entries = nil
	e.get(t, "/api/v1/blocklist").expect(t, http.StatusOK).into(t, &entries)
	if len(entries) != 0 {
		t.Errorf("entry survived its delete: %+v", entries)
	}
}

// Blocklisting a row that is gone is a 404 rather than a no-op success: the
// button reports what it did, and "banned" for a row that does not exist is
// a lie the operator will act on.
func TestAcqBlocklistQueueItemUnknownRow(t *testing.T) {
	e := newAPIEnv(t)
	e.post(t, "/api/v1/queue/9999/blocklist", "").expect(t, http.StatusNotFound)
}

// Removing a queue row drops it, with and without the client-side delete,
// and an unknown row is a 404.
func TestAcqRemoveQueueItem(t *testing.T) {
	e := newAPIEnv(t)
	acqAddClient(t, e, "qbittorrent")
	item := e.addMovie(t)

	id := acqGrab(t, e, item)
	e.del(t, "/api/v1/queue/"+acqItoa(id)).expect(t, http.StatusNoContent)

	// fromClient=true additionally asks the client to drop it; the fake
	// accepts, and the row must still go.
	id = acqGrab(t, e, item)
	e.del(t, "/api/v1/queue/"+acqItoa(id)+"?fromClient=true").expect(t, http.StatusNoContent)

	var queue []apigen.QueueItem
	e.get(t, "/api/v1/queue").expect(t, http.StatusOK).into(t, &queue)
	if len(queue) != 0 {
		t.Errorf("queue rows survived their removal: %+v", queue)
	}

	e.del(t, "/api/v1/queue/9999").expect(t, http.StatusNotFound)
}

// ---- custom formats ----

// A custom format round trips with its score, and disappears on delete.
func TestAcqCustomFormatRoundTrip(t *testing.T) {
	e := newAPIEnv(t)

	rr := e.post(t, "/api/v1/customformats",
		`{"name":"x265","pattern":"x265|HEVC","score":-50}`).expect(t, http.StatusCreated)
	var created apigen.CustomFormat
	rr.into(t, &created)
	if created.Id == 0 || created.Name != "x265" || created.Pattern != "x265|HEVC" {
		t.Fatalf("created format = %+v, want the submitted rule", created)
	}
	if created.Score == nil || *created.Score != -50 {
		t.Errorf("score = %v, want the submitted -50; a dropped negative score silently promotes what it was meant to demote", created.Score)
	}

	var list []apigen.CustomFormat
	e.get(t, "/api/v1/customformats").expect(t, http.StatusOK).into(t, &list)
	if len(list) != 1 || list[0].Id != created.Id {
		t.Fatalf("list = %+v, want the one format just created", list)
	}

	e.del(t, "/api/v1/customformats/"+acqItoa(created.Id)).expect(t, http.StatusNoContent)
	list = nil
	e.get(t, "/api/v1/customformats").expect(t, http.StatusOK).into(t, &list)
	if len(list) != 0 {
		t.Errorf("format survived its delete: %+v", list)
	}
}

// A pattern that does not compile is refused at the door, with the compiler's
// complaint passed through.
//
// This is the one validation in the file that must happen up front: an
// uncompilable pattern stored is a scoring pass that fails on every release
// of every search from then on, and the place that discovers it is a search
// that returns nothing rather than the form that accepted it.
func TestAcqAddCustomFormatRefusesAnUncompilablePattern(t *testing.T) {
	e := newAPIEnv(t)

	rr := e.post(t, "/api/v1/customformats",
		`{"name":"broken","pattern":"(unclosed"}`).expect(t, http.StatusBadRequest)
	if msg := acqMessage(t, rr); !strings.Contains(msg, "pattern") {
		t.Errorf("message = %q, want it to name the pattern as the problem", msg)
	}

	var list []apigen.CustomFormat
	e.get(t, "/api/v1/customformats").expect(t, http.StatusOK).into(t, &list)
	if len(list) != 0 {
		t.Errorf("an uncompilable pattern was stored anyway: %+v", list)
	}
}

// Name and pattern are both required: a format with neither matches nothing
// and scores everything by accident.
func TestAcqAddCustomFormatRefusesIncompleteInput(t *testing.T) {
	e := newAPIEnv(t)
	for name, body := range map[string]string{
		"no name":    `{"name":"","pattern":"x265"}`,
		"no pattern": `{"name":"x265","pattern":""}`,
		"not json":   `{`,
	} {
		t.Run(name, func(t *testing.T) {
			acqMessage(t, e.post(t, "/api/v1/customformats", body).expect(t, http.StatusBadRequest))
		})
	}
}

// ---- import lists ----

// An import list round trips, and the fields the form leaves out come back
// filled with the defaults a manual add of that kind would have used —
// which is the rule the handler states: everything a list adds behaves like
// a manual add.
func TestAcqImportListRoundTrip(t *testing.T) {
	e := newAPIEnv(t)

	rr := e.post(t, "/api/v1/importlists",
		`{"name":"Trending","type":"tmdb-popular"}`).expect(t, http.StatusCreated)
	var created apigen.ImportList
	rr.into(t, &created)
	if created.Id == 0 || created.Name != "Trending" {
		t.Fatalf("created list = %+v, want the submitted list", created)
	}
	if created.Kind == nil || *created.Kind != "movie" {
		t.Errorf("kind = %v, want the movie default", created.Kind)
	}
	if created.Monitored == nil || !*created.Monitored {
		t.Error("monitored must default on; a list that adds unmonitored items never searches for them")
	}
	if created.Enabled == nil || !*created.Enabled {
		t.Error("enabled must default on; a list saved switched off does nothing and says nothing")
	}
	if created.QualityProfileId == nil || *created.QualityProfileId == 0 {
		t.Errorf("qualityProfileId = %v, want the kind's default profile filled in", created.QualityProfileId)
	}
	if created.Config == nil {
		t.Error("config is null; the edit form indexes into it")
	}

	var list []apigen.ImportList
	e.get(t, "/api/v1/importlists").expect(t, http.StatusOK).into(t, &list)
	if len(list) != 1 || list[0].Id != created.Id {
		t.Fatalf("list = %+v, want the one import list just created", list)
	}

	e.del(t, "/api/v1/importlists/"+acqItoa(created.Id)).expect(t, http.StatusNoContent)
	list = nil
	e.get(t, "/api/v1/importlists").expect(t, http.StatusOK).into(t, &list)
	if len(list) != 0 {
		t.Errorf("import list survived its delete: %+v", list)
	}
}

// A nameless import list is refused: the name is the only handle the
// settings page has on a list, and a blank row cannot be told from another
// blank row when it comes to deleting the right one.
func TestAcqAddImportListRefusesIncompleteInput(t *testing.T) {
	e := newAPIEnv(t)
	for name, body := range map[string]string{
		"no name":  `{"name":"","type":"tmdb-popular"}`,
		"not json": `{`,
	} {
		t.Run(name, func(t *testing.T) {
			acqMessage(t, e.post(t, "/api/v1/importlists", body).expect(t, http.StatusBadRequest))
		})
	}
}

// ---- calendar ----

// The calendar answers a window with the dated items in it, carrying the
// external ids so a consumer can resolve an entry against its own library by
// id rather than by matching titles.
func TestAcqCalendarReportsDatedItems(t *testing.T) {
	e := newAPIEnv(t)
	item := e.addMovie(t)
	acqSetReleaseDate(t, e, item, "1999-10-15")

	var entries []apigen.CalendarEntry
	e.get(t, "/api/v1/calendar?start=1999-01-01&end=1999-12-31").
		expect(t, http.StatusOK).into(t, &entries)
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want the one dated item: %+v", len(entries), entries)
	}
	got := entries[0]
	if got.MediaItemId != item || got.Title != "Fight Club" {
		t.Errorf("entry = %+v, want the movie that was dated into the window", got)
	}
	if got.Date != "1999-10-15" {
		t.Errorf("date = %q, want the release date", got.Date)
	}
	if got.Kind != "movie" {
		t.Errorf("kind = %q, want movie", got.Kind)
	}
	if got.HasFile {
		t.Error("hasFile is true for an item with no files")
	}
	if got.TmdbId == nil || *got.TmdbId != 550 {
		t.Errorf("tmdbId = %v, want 550 so a consumer can match by id", got.TmdbId)
	}
	if got.ImdbId == nil || *got.ImdbId != "tt0137523" {
		t.Errorf("imdbId = %v, want the stored IMDb id", got.ImdbId)
	}

	// Outside the window, the same item is simply not there.
	entries = nil
	e.get(t, "/api/v1/calendar?start=2020-01-01&end=2020-12-31").
		expect(t, http.StatusOK).into(t, &entries)
	if len(entries) != 0 {
		t.Errorf("window is not being applied: %+v", entries)
	}
}

// The window is required, both halves of it. Defaulting it would make an
// accidental unbounded call scan the whole library, and the caller would
// never learn its query was incomplete.
func TestAcqCalendarRequiresBothEndsOfTheWindow(t *testing.T) {
	e := newAPIEnv(t)
	for name, path := range map[string]string{
		"no params": "/api/v1/calendar",
		"no end":    "/api/v1/calendar?start=2026-01-01",
		"no start":  "/api/v1/calendar?end=2026-12-31",
	} {
		t.Run(name, func(t *testing.T) {
			e.get(t, path).expect(t, http.StatusBadRequest)
		})
	}
}

// ---- wanted ----

// A monitored movie with no file is on the wanted list, described the way
// the page renders it: title, year, and the flag that says it is missing
// rather than merely upgradable.
func TestAcqWantedListsAMonitoredMissingMovie(t *testing.T) {
	e := newAPIEnv(t)
	item := e.addMovie(t)

	var wanted []apigen.WantedItem
	e.get(t, "/api/v1/wanted").expect(t, http.StatusOK).into(t, &wanted)
	if len(wanted) != 1 {
		t.Fatalf("got %d wanted rows, want the one missing movie: %+v", len(wanted), wanted)
	}
	w := wanted[0]
	if w.MediaItemId != item {
		t.Errorf("mediaItemId = %d, want the movie just added (%d)", w.MediaItemId, item)
	}
	if w.Title != "Fight Club" {
		t.Errorf("title = %q, want the item's title", w.Title)
	}
	if !w.Missing {
		t.Error("missing = false for an item with no file at all")
	}
	if w.WantableId == "" {
		t.Error("wantableId is empty; the row cannot be acted on without it")
	}
}

// ---- notifiers ----

// A notifier round trips with its URL completed to the service's default
// port and its event flags defaulted, then updates in place and deletes.
//
// The update keeping the id is the point: the delivery log hangs off it, and
// editing used to be delete-and-recreate, which threw away the record of
// every delivery that came before the edit.
func TestAcqNotifierRoundTrip(t *testing.T) {
	e := newAPIEnv(t)

	rr := e.post(t, "/api/v1/notifiers",
		`{"type":"plurx","name":"living room","settings":{"url":"plurxd"}}`).
		expect(t, http.StatusCreated)
	var created apigen.Notifier
	rr.into(t, &created)
	if created.Id == 0 {
		t.Fatalf("created notifier has no id: %s", rr.Body.String())
	}
	if created.Settings == nil || (*created.Settings)["url"] != "http://plurxd:32400" {
		t.Errorf("settings url = %v, want the host completed to what Monarr will dial", created.Settings)
	}
	if created.OnGrab == nil || !*created.OnGrab || created.OnImport == nil || !*created.OnImport {
		t.Error("a new notifier must default to firing on grab and import; silence is indistinguishable from broken")
	}
	if created.OnHealth == nil || *created.OnHealth {
		t.Error("onHealth must default off; health chatter is opt-in")
	}

	var list []apigen.Notifier
	e.get(t, "/api/v1/notifiers").expect(t, http.StatusOK).into(t, &list)
	if len(list) != 1 || list[0].Id != created.Id {
		t.Fatalf("list = %+v, want the one notifier just created", list)
	}

	rr = e.put(t, "/api/v1/notifiers/"+acqItoa(created.Id),
		`{"type":"plurx","name":"bedroom","settings":{"url":"plurxd"},"onImport":false}`).
		expect(t, http.StatusOK)
	var updated apigen.Notifier
	rr.into(t, &updated)
	if updated.Id != created.Id {
		t.Errorf("id changed on edit: %d → %d; the delivery log hangs off it", created.Id, updated.Id)
	}
	if updated.Name != "bedroom" {
		t.Errorf("name = %q, want the edit applied", updated.Name)
	}
	if updated.OnImport == nil || *updated.OnImport {
		t.Errorf("onImport = %v, want the submitted false", updated.OnImport)
	}

	e.del(t, "/api/v1/notifiers/"+acqItoa(created.Id)).expect(t, http.StatusNoContent)
	list = nil
	e.get(t, "/api/v1/notifiers").expect(t, http.StatusOK).into(t, &list)
	if len(list) != 0 {
		t.Errorf("notifier survived its delete: %+v", list)
	}
}

// Every per-id notifier route answers 404 for an id that is not there,
// including the delete. Without the existence check a delete of nothing
// reports success, and the settings page then shows a row it just told the
// user was gone.
func TestAcqNotifierRoutesRefuseUnknownIds(t *testing.T) {
	e := newAPIEnv(t)
	e.get(t, "/api/v1/notifiers/9999/deliveries").expect(t, http.StatusNotFound)
	e.del(t, "/api/v1/notifiers/9999").expect(t, http.StatusNotFound)
	e.put(t, "/api/v1/notifiers/9999",
		`{"type":"plurx","name":"nowhere"}`).expect(t, http.StatusNotFound)
	e.post(t, "/api/v1/notifiers/9999/test", "").expect(t, http.StatusNotFound)
}

// A notifier needs a known type and a name, on create and on edit alike. An
// unknown type is a notifier that can never be constructed, so it would sit
// in the settings list failing invisibly after every import.
func TestAcqNotifierRefusesUnknownTypeOrMissingName(t *testing.T) {
	e := newAPIEnv(t)
	for name, body := range map[string]string{
		"unknown type": `{"type":"carrier-pigeon","name":"nope"}`,
		"no name":      `{"type":"plurx","name":""}`,
		"not json":     `{`,
	} {
		t.Run("create "+name, func(t *testing.T) {
			acqMessage(t, e.post(t, "/api/v1/notifiers", body).expect(t, http.StatusBadRequest))
		})
	}

	// The edit path validates the same way, but only after the row is known
	// to exist — so this needs a real notifier to aim at.
	rr := e.post(t, "/api/v1/notifiers",
		`{"type":"plurx","name":"living room"}`).expect(t, http.StatusCreated)
	var created apigen.Notifier
	rr.into(t, &created)
	for name, body := range map[string]string{
		"unknown type": `{"type":"carrier-pigeon","name":"nope"}`,
		"no name":      `{"type":"plurx","name":""}`,
		"not json":     `{`,
	} {
		t.Run("update "+name, func(t *testing.T) {
			acqMessage(t, e.put(t, "/api/v1/notifiers/"+acqItoa(created.Id), body).
				expect(t, http.StatusBadRequest))
		})
	}
}

// The unsaved-config Test button refuses a body it cannot build a notifier
// from, before it tries to reach anything. A test that fails at the network
// for a config that was never valid reports the wrong problem.
func TestAcqTestNotifierRefusesAnUnbuildableConfig(t *testing.T) {
	e := newAPIEnv(t)
	for name, body := range map[string]string{
		"unknown type": `{"type":"carrier-pigeon","name":"nope"}`,
		"not json":     `{`,
	} {
		t.Run(name, func(t *testing.T) {
			acqMessage(t, e.post(t, "/api/v1/notifiers/test", body).expect(t, http.StatusBadRequest))
		})
	}
}

// A notifier that has never delivered anything answers with an empty array
// rather than null, and the page renders "nothing yet" instead of erroring
// on a null it tried to iterate.
func TestAcqNotifierDeliveriesStartEmpty(t *testing.T) {
	e := newAPIEnv(t)
	rr := e.post(t, "/api/v1/notifiers",
		`{"type":"plurx","name":"living room","settings":{"url":"plurxd"}}`).
		expect(t, http.StatusCreated)
	var created apigen.Notifier
	rr.into(t, &created)

	got := e.get(t, "/api/v1/notifiers/"+acqItoa(created.Id)+"/deliveries").
		expect(t, http.StatusOK)
	if body := strings.TrimSpace(got.Body.String()); body != "[]" {
		t.Errorf("deliveries body = %s, want an empty array", body)
	}
	var deliveries []apigen.Delivery
	got.into(t, &deliveries)
	if len(deliveries) != 0 {
		t.Errorf("deliveries = %+v, want none for a notifier that has never fired", deliveries)
	}
}
