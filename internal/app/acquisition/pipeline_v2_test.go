package acquisition

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/app/pipeline"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// Pipeline contract v2, M2 acceptance tests (plan M2 (b)-(p)). The Runner
// here is a fake implementing the reducer's feeds; the database, the reducer,
// the importer and the grab worker are the real ones.

// fakeRunner is a Runner client: the queue, an ordered history with an
// incarnation, the deliveries feed and request receipts.
type fakeRunner struct {
	fakeClient
	rmu         sync.Mutex
	history     []ports.RunnerFact // HistorySeq set, ascending
	incarnation string
	retainedMin int64
	deliveries  []ports.RunnerFact
	admissions  map[string]ports.RequestAdmission
	admitErr    error
	nextJob     int
	adds        []ports.AddOptions
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{admissions: map[string]ports.RequestAdmission{}, nextJob: 100}
}

func (f *fakeRunner) AddWithOptions(_ context.Context, url, _ string, o ports.AddOptions) (ports.Handle, error) {
	f.rmu.Lock()
	defer f.rmu.Unlock()
	f.mu.Lock()
	f.added = append(f.added, url)
	err := f.addErr
	f.mu.Unlock()
	if err != nil {
		return "", err
	}
	f.nextJob++
	f.adds = append(f.adds, o)
	return ports.Handle(strconv.Itoa(f.nextJob)), nil
}

func (f *fakeRunner) ListDeliveries(context.Context, uint64, int) (ports.RunnerDeliveryPage, error) {
	return ports.RunnerDeliveryPage{ProtocolVersion: 1}, nil
}
func (f *fakeRunner) AcceptDelivery(context.Context, string, ports.RunnerDeliveryAcceptance) error {
	return nil
}
func (f *fakeRunner) SupportsDeliveryV1(context.Context) (bool, error)              { return true, nil }
func (f *fakeRunner) SupportsRequestTokenIdempotency(context.Context) (bool, error) { return true, nil }

func (f *fakeRunner) HistorySince(_ context.Context, c ports.HistoryCursor, limit int) ([]ports.RunnerFact, ports.HistoryCursor, bool, error) {
	f.rmu.Lock()
	defer f.rmu.Unlock()
	head := int64(0)
	for _, h := range f.history {
		head = max(head, h.HistorySeq)
	}
	next := ports.HistoryCursor{Incarnation: f.incarnation, SinceSeq: c.SinceSeq, HeadSeq: head, RetainedMinSeq: f.retainedMin}
	if (c.Incarnation != "" && c.Incarnation != f.incarnation) || (f.retainedMin > 0 && c.SinceSeq+1 < f.retainedMin) {
		next.SinceSeq = head
		return nil, next, true, nil
	}
	var out []ports.RunnerFact
	for _, h := range f.history {
		if h.HistorySeq > c.SinceSeq && len(out) < limit {
			out = append(out, h)
			next.SinceSeq = h.HistorySeq
		}
	}
	return out, next, false, nil
}

func (f *fakeRunner) DeliveriesSince(_ context.Context, after uint64, limit int) ([]ports.RunnerFact, uint64, error) {
	f.rmu.Lock()
	defer f.rmu.Unlock()
	if after >= uint64(len(f.deliveries)) {
		return nil, after, nil
	}
	end := min(uint64(len(f.deliveries)), after+uint64(limit))
	return append([]ports.RunnerFact(nil), f.deliveries[after:end]...), end, nil
}

func (f *fakeRunner) GetRequestAdmission(_ context.Context, token string) (ports.RequestAdmission, bool, error) {
	f.rmu.Lock()
	defer f.rmu.Unlock()
	if f.admitErr != nil {
		return ports.RequestAdmission{}, false, f.admitErr
	}
	a, ok := f.admissions[token]
	return a, ok, nil
}

func (f *fakeRunner) addHistory(fact ports.RunnerFact) {
	f.rmu.Lock()
	defer f.rmu.Unlock()
	f.history = append(f.history, fact)
	sort.Slice(f.history, func(i, j int) bool { return f.history[i].HistorySeq < f.history[j].HistorySeq })
}

func (f *fakeRunner) addCount() int {
	f.rmu.Lock()
	defer f.rmu.Unlock()
	return len(f.adds)
}

// v2Fixture is a service on the reducer with a completed root to publish in.
type v2Fixture struct {
	t      *testing.T
	svc    *Service
	db     *sqlite.DB
	runner *fakeRunner
	cfg    ports.ClientConfig
	item   int64
	root   string
	ctx    context.Context
}

func setupV2(t *testing.T, releases []ports.Release) *v2Fixture {
	t.Helper()
	runner := newFakeRunner()
	svc, db, item := setup(t, releases, &runner.fakeClient)
	return setupV2On(t, runner, svc, db, item)
}

// setupV2On finishes the v2 fixture on a setup scenario.
func setupV2On(t *testing.T, runner *fakeRunner, svc *Service, db *sqlite.DB, item int64) *v2Fixture {
	t.Helper()
	svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return runner }
	ctx := context.Background()
	root := t.TempDir()
	if err := db.SetMeta(ctx, CompletedRootsSetting, root); err != nil {
		t.Fatal(err)
	}
	// The storage identity of the completed root is recorded by an inventory
	// scan; a publication import waits for one, as the v1 import did.
	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	cfg, err := db.GetDownloadClient(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	return &v2Fixture{t: t, svc: svc, db: db, runner: runner, cfg: cfg, item: item, root: root, ctx: ctx}
}

// grab grabs an episode (episode 0: the season pack) through the real grab path.
func (f *v2Fixture) grab(episode int, title string) sqlite.Download {
	f.t.Helper()
	id, err := f.svc.Grab(f.ctx, GrabRequest{MediaItemID: f.item, Season: 1, Episode: episode, Title: title,
		DownloadURL: "http://dl/" + title, Indexer: "idx", Protocol: "torrent", Size: 1000})
	if err != nil {
		f.t.Fatal(err)
	}
	return f.row(id)
}

func (f *v2Fixture) row(id int64) sqlite.Download {
	f.t.Helper()
	dl, err := f.db.GetDownload(f.ctx, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return dl
}

// publish writes a publication directory with its manifest and returns the
// publication summary a completed terminal carries.
func (f *v2Fixture) publish(name, generation string, files map[string]string) ports.RunnerPublication {
	f.t.Helper()
	dir := filepath.Join(f.root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	var mf []ports.ManifestFile
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			f.t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(body))
		mf = append(mf, ports.ManifestFile{Path: rel, Bytes: int64(len(body)), SHA256: hex.EncodeToString(sum[:])})
	}
	digest, err := ports.ManifestDigest(mf)
	if err != nil {
		f.t.Fatal(err)
	}
	raw, _ := json.Marshal(ports.Manifest{Generation: generation, Files: mf, Digest: digest})
	if err := os.WriteFile(filepath.Join(dir, ports.ManifestFileName), raw, 0o644); err != nil {
		f.t.Fatal(err)
	}
	return ports.RunnerPublication{Path: dir, Generation: generation, ManifestDigest: digest, Files: len(mf), Kind: "completed"}
}

func terminalFact(dl sqlite.Download, t ports.RunnerTerminal, source string) ports.RunnerFact {
	return ports.RunnerFact{Kind: ports.FactTerminal, Transfer: dl.Transfer, Handle: ports.Handle(dl.Handle), Terminal: &t, Source: source}
}

func failed(reason string, blames bool) ports.RunnerTerminal {
	return ports.RunnerTerminal{Outcome: ports.OutcomeFailed, Reason: reason, Message: reason + " failure", BlamesRelease: blames, Salvage: "none"}
}

func (f *v2Fixture) observe(dl sqlite.Download, fact ports.RunnerFact) {
	f.t.Helper()
	if err := f.svc.observe(f.ctx, dl, f.cfg, fact); err != nil {
		f.t.Fatal(err)
	}
}

func (f *v2Fixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.db.R.QueryRowContext(f.ctx, query, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *v2Fixture) deferGrabs() { f.svc.grabWorker.Store(true) }

const (
	epOne      = "Test.Show.S01E01.1080p.WEB-DL.x264-GRP"
	epTwo      = "Test.Show.S01E02.1080p.WEB-DL.x264-GRP"
	seasonPack = "Test.Show.S01.1080p.WEB-DL.x264-PACK"
)

// (b) A failed terminal blaming the release writes the blocklist and the
// replacement grab in its own transaction; one worker tick grabs once for the
// same wantable, and the failed row is terminal.
func TestTerminalFailedWritesReplacementGrabInSameTx(t *testing.T) {
	f := setupV2(t, []ports.Release{rel("Test.Show.S01E01.1080p.WEB-DL.x264-OTHER", 10)})
	dl := f.grab(1, epOne)
	if dl.State != "downloading" || dl.Handle == "" {
		t.Fatalf("admitted row = %s handle %q, want downloading with its job", dl.State, dl.Handle)
	}
	f.deferGrabs()
	f.observe(dl, terminalFact(dl, failed(ports.ReasonUnpack, true), "event 7"))
	got := f.row(dl.ID)
	if got.RunnerOutcome != "failed" || got.State != "failed" || !got.RunnerBlamesRelease || got.Provisional {
		t.Fatalf("row = %s/%s blames=%v provisional=%v, want a final blamed failure", got.State, got.RunnerOutcome, got.RunnerBlamesRelease, got.Provisional)
	}
	if n := f.count(`SELECT COUNT(*) FROM replacement_grabs WHERE download_id=? AND result=''`, dl.ID); n != 1 {
		t.Fatalf("replacement_grabs = %d, want one unsettled row written with the terminal", n)
	}
	if bl, _ := f.db.ListBlocklist(f.ctx); len(bl) != 1 || bl[0].ReleaseTitle != epOne {
		t.Fatalf("blocklist = %+v, want the failed release", bl)
	}
	if f.runner.addCount() != 1 {
		t.Fatal("the grab ran inside the terminal, not in the worker")
	}
	f.svc.RunReplacementGrabs(f.ctx) // one worker tick
	if f.runner.addCount() != 2 {
		t.Fatalf("adds = %d, want exactly one replacement grab", f.runner.addCount())
	}
	g, ok, _ := f.db.GetReplacementGrab(f.ctx, dl.ID)
	if !ok || !strings.HasPrefix(g.Result, "grabbed") {
		t.Fatalf("grab row = %+v, want settled grabbed", g)
	}
	rows, _ := f.db.ListActiveDownloads(f.ctx)
	for _, r := range rows {
		if r.ID == dl.ID {
			t.Fatal("the failed row is still active")
		}
		if len(r.WantableIDs) != 1 || r.WantableIDs[0] != "episode:1:1:1" {
			t.Fatalf("replacement wantables = %v, want the same episode", r.WantableIDs)
		}
	}
	// A second tick does nothing: the row is settled.
	f.svc.RunReplacementGrabs(f.ctx)
	if f.runner.addCount() != 2 {
		t.Fatal("a settled replacement grab ran again")
	}
}

// (c) A failed season pack is replaced per episode, never by a pack; the
// planner is invoked only for the episode with no single release.
func TestEpisodeFailureGrabsEpisodeNotPack(t *testing.T) {
	f := setupV2(t, []ports.Release{
		rel("Test.Show.S01E01.1080p.WEB-DL.x264-SINGLE", 10),
		rel("Test.Show.S01.1080p.WEB-DL.x264-OTHERPACK", 50),
	})
	dl := f.grab(0, seasonPack)
	f.deferGrabs()
	f.observe(dl, terminalFact(dl, failed(ports.ReasonHealth, true), "event 1"))
	f.svc.RunReplacementGrabs(f.ctx)
	f.runner.rmu.Lock()
	adds := append([]ports.AddOptions(nil), f.runner.adds...)
	f.runner.rmu.Unlock()
	if len(adds) != 2 {
		t.Fatalf("adds = %+v, want the pack and one single-episode replacement", adds)
	}
	if strings.Contains(adds[1].Name, "PACK") || !strings.Contains(adds[1].Name, "S01E01") {
		t.Fatalf("replacement = %q, want the single E01 release, never a pack", adds[1].Name)
	}
	if n := f.count(`SELECT COUNT(*) FROM jobs WHERE kind=?`, SeasonSearchJobKind); n != 1 {
		t.Fatalf("season planner jobs = %d, want one, for the episode with no single release", n)
	}
}

// (d) A held row whose stored control is older than anything the terminal
// says still completes: a terminal ends a hold unconditionally.
func TestTerminalCompletesHeldRowWithOlderRevision(t *testing.T) {
	f := setupV2(t, nil)
	dl := f.grab(1, epOne)
	deadline := int64(3600)
	control := &ports.DownloadControl{Version: 1, Revision: "10", Lifecycle: "held", Cause: "capacity", Message: "waiting for disk", Instance: "n1", DeadlineInS: &deadline}
	st := ports.DownloadStatus{Handle: ports.Handle(dl.Handle), Transfer: dl.Transfer, State: ports.StateQueued, Control: control}
	f.observe(dl, ports.RunnerFact{Kind: ports.FactStatus, Transfer: dl.Transfer, Status: &st, Source: "event 2"})
	held := f.row(dl.ID)
	if held.State != "held" || held.Error != "waiting for disk" || held.DeadlineAt.Before(time.Now().Add(time.Hour)) {
		t.Fatalf("held row = %s %q deadline %s, want held with Runner's deadline + grace", held.State, held.Error, held.DeadlineAt)
	}
	pub := f.publish("Test.Show.S01E01.job-101.runner-aaa", "aaa", map[string]string{epOne + ".mkv": "video"})
	f.observe(held, terminalFact(held, ports.RunnerTerminal{Outcome: ports.OutcomeCompleted, Publication: &pub, Salvage: "none"}, "event 3"))
	done := f.row(dl.ID)
	if done.RunnerOutcome != "completed" || done.State != "imported" {
		t.Fatalf("row = %s/%s (%s), want completed and imported", done.State, done.RunnerOutcome, done.Error)
	}
	pubs, _ := f.db.ListPublicationsForDownload(f.ctx, dl.ID)
	// Imported, then removed by the per-file cleanup (M4) as every file had
	// evidence.
	if len(pubs) != 1 || pubs[0].State != "removed" || pubs[0].Cohort != "live" {
		t.Fatalf("publications = %+v, want one live publication, imported and cleaned up", pubs)
	}
}

// (e) A terminal whose transfer token is not the row's is dropped and
// counted; the row is unchanged.
func TestTransferMismatchIsDropped(t *testing.T) {
	f := setupV2(t, nil)
	dl := f.grab(1, epOne)
	before := f.svc.FactMismatches()
	tm := failed(ports.ReasonHealth, true)
	f.svc.handleRunnerEvent(f.ctx, f.cfg, ports.ClientEvent{Fact: &ports.RunnerFact{Kind: ports.FactTerminal, Transfer: "t-9-someone-else",
		Handle: ports.Handle(dl.Handle), Terminal: &tm, Source: "event 5"}}, map[int64]float64{}, new(time.Time))
	if f.svc.FactMismatches() != before+1 {
		t.Fatalf("mismatches = %d, want +1", f.svc.FactMismatches())
	}
	if got := f.row(dl.ID); got.State != "downloading" || got.RunnerOutcome != "" {
		t.Fatalf("row = %s/%s, want unchanged", got.State, got.RunnerOutcome)
	}
	// Routed to the row by handle (a legacy path), the same fact is dropped too.
	f.observe(dl, terminalFact(sqlite.Download{Transfer: "t-9-someone-else", Handle: dl.Handle}, tm, "poll"))
	if got := f.row(dl.ID); got.RunnerOutcome != "" {
		t.Fatal("a foreign-token terminal was applied")
	}
}

// (f) SSE and poll delivering the same terminal at once record it once and
// make one publication; a replayed completed after import makes no second.
func TestSSEAndPollSameTerminalOnce(t *testing.T) {
	f := setupV2(t, nil)
	dl := f.grab(1, epOne)
	pub := f.publish("Test.Show.S01E01.job-101.runner-bbb", "bbb", map[string]string{epOne + ".mkv": "video"})
	tm := ports.RunnerTerminal{Outcome: ports.OutcomeCompleted, Publication: &pub, Salvage: "none"}
	var wg sync.WaitGroup
	for _, source := range []string{"event 9", "history -:9"} {
		wg.Add(1)
		go func(source string) {
			defer wg.Done()
			_ = f.svc.observe(f.ctx, dl, f.cfg, terminalFact(dl, tm, source))
		}(source)
	}
	wg.Wait()
	if n := f.count(`SELECT COUNT(*) FROM publications WHERE download_id=?`, dl.ID); n != 1 {
		t.Fatalf("publications = %d, want one", n)
	}
	got := f.row(dl.ID)
	if got.State != "imported" {
		t.Fatalf("state = %s (%s), want imported", got.State, got.Error)
	}
	imports := f.count(`SELECT COUNT(*) FROM history_events WHERE type='imported'`)
	f.observe(got, terminalFact(got, tm, "deliveries 3"))
	if n := f.count(`SELECT COUNT(*) FROM publications WHERE download_id=?`, dl.ID); n != 1 {
		t.Fatalf("a replayed completed made a second publication (%d)", n)
	}
	if f.count(`SELECT COUNT(*) FROM history_events WHERE type='imported'`) != imports || f.row(dl.ID).State != "imported" {
		t.Fatal("a replayed completed imported again")
	}
}

// (g) A crash between the terminal's commit and the grab: on restart the
// replacement grab runs exactly once.
func TestCrashBetweenTerminalAndGrabRunsGrabOnce(t *testing.T) {
	f := setupV2(t, []ports.Release{rel("Test.Show.S01E01.1080p.WEB-DL.x264-OTHER", 10)})
	dl := f.grab(1, epOne)
	f.deferGrabs() // the process dies before its worker runs
	f.observe(dl, terminalFact(dl, failed(ports.ReasonPar, true), "event 4"))
	restarted := New(f.db, nil, nil, f.svc.newIndexer, f.svc.newClient)
	restarted.RunReplacementGrabs(f.ctx)
	restarted.RunReplacementGrabs(f.ctx)
	f.svc.RunReplacementGrabs(f.ctx) // the old process's late tick, too
	if f.runner.addCount() != 2 {
		t.Fatalf("adds = %d, want the original and exactly one replacement", f.runner.addCount())
	}
}

// (g) A crash between the terminal's commit and the history cursor's
// advance: the replayed terminal is a no-op, and the cursor then moves.
func TestCrashBetweenCommitAndCursorIsNoop(t *testing.T) {
	f := setupV2(t, nil)
	dl := f.grab(1, epOne)
	f.deferGrabs()
	if err := f.db.UpsertHistoryCursor(f.ctx, sqlite.RunnerHistoryCursor{ClientID: f.cfg.ID, Incarnation: "inc", SinceSeq: 40}); err != nil {
		t.Fatal(err)
	}
	f.runner.incarnation = "inc"
	fact := terminalFact(dl, failed(ports.ReasonHealth, true), "history inc:41")
	fact.HistorySeq = 41
	f.runner.addHistory(fact)
	f.observe(dl, fact) // committed; the process dies before UpsertHistoryCursor
	blocked := len(mustBlocklist(t, f))
	f.svc.pollHistory(f.ctx, f.cfg, f.runner)
	if n := f.count(`SELECT COUNT(*) FROM replacement_grabs WHERE download_id=?`, dl.ID); n != 1 {
		t.Fatalf("replacement_grabs = %d after replay, want 1", n)
	}
	if len(mustBlocklist(t, f)) != blocked {
		t.Fatal("the replay blocklisted again")
	}
	if c, _ := f.db.GetHistoryCursor(f.ctx, f.cfg.ID); c.SinceSeq != 41 {
		t.Fatalf("cursor = %d, want 41", c.SinceSeq)
	}
}

func mustBlocklist(t *testing.T, f *v2Fixture) []sqlite.BlocklistEntry {
	t.Helper()
	bl, err := f.db.ListBlocklist(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	return bl
}

// (h) cancelled and lost never re-grab and never blocklist.
func TestCancelledAndLostNeverRegrab(t *testing.T) {
	f := setupV2(t, []ports.Release{rel("Test.Show.S01E01.1080p.WEB-DL.x264-OTHER", 10)})
	first := f.grab(1, epOne)
	f.observe(first, terminalFact(first, ports.RunnerTerminal{Outcome: ports.OutcomeCancelled, Salvage: "none"}, "event 1"))
	second := f.grab(2, epTwo)
	f.observe(second, ports.RunnerFact{Kind: ports.FactGone, Transfer: second.Transfer, Source: "poll"})
	f.svc.RunReplacementGrabs(f.ctx)
	if n := f.count(`SELECT COUNT(*) FROM replacement_grabs`); n != 0 {
		t.Fatalf("replacement_grabs = %d, want none", n)
	}
	if bl := mustBlocklist(t, f); len(bl) != 0 {
		t.Fatalf("blocklist = %+v, want empty", bl)
	}
	if f.runner.addCount() != 2 {
		t.Fatalf("adds = %d, want only the two originals", f.runner.addCount())
	}
	lost := f.row(second.ID)
	if lost.RunnerOutcomeReason != ports.ReasonLost || !lost.Provisional {
		t.Fatalf("lost row = %+v, want a provisional failed{lost}", lost.RunnerOutcomeReason)
	}
	if c := f.row(first.ID); c.RunnerOutcome != "cancelled" || c.Provisional {
		t.Fatalf("cancelled row = %s provisional=%v", c.RunnerOutcome, c.Provisional)
	}
}

// (h) An io failure is not the release's fault: no blocklist, but a grab.
func TestIoFailureDoesNotBlocklistButGrabs(t *testing.T) {
	f := setupV2(t, []ports.Release{rel("Test.Show.S01E01.1080p.WEB-DL.x264-OTHER", 10)})
	dl := f.grab(1, epOne)
	f.observe(dl, terminalFact(dl, failed(ports.ReasonIO, false), "event 1"))
	if bl := mustBlocklist(t, f); len(bl) != 0 {
		t.Fatalf("blocklist = %+v, want empty for an io failure", bl)
	}
	if f.runner.addCount() != 2 {
		t.Fatalf("adds = %d, want a replacement grab", f.runner.addCount())
	}
}

// (p) An exhausted fetch through 503s is the indexer's outage: no blocklist,
// no replacement grab, the want left to the next search. A 404 is the item's.
func TestIndexerUnavailableRequeuesWithoutGrab(t *testing.T) {
	f := setupV2(t, []ports.Release{rel("Test.Show.S01E01.1080p.WEB-DL.x264-OTHER", 10)})
	dl := f.grab(1, epOne)
	if f.count(`SELECT COUNT(*) FROM indexers`) == 0 {
		t.Fatal("fixture has no indexer")
	}
	if _, err := f.db.W.ExecContext(f.ctx, `UPDATE downloads SET source_indexer_id=(SELECT MIN(id) FROM indexers) WHERE id=?`, dl.ID); err != nil {
		t.Fatal(err)
	}
	dl = f.row(dl.ID)
	tm := failed(ports.ReasonFetch, false)
	tm.FetchCause = ports.FetchCauseIndexerUnavailable
	f.observe(dl, terminalFact(dl, tm, "event 1"))
	// The outage defers the indexer (the request-budget gate the wanted pass
	// honours) and the row carries the same provider_retry_after.
	soon := time.Now().Add(10 * time.Minute).UnixMilli()
	if n := f.count(`SELECT COUNT(*) FROM downloads d JOIN indexers i ON i.id=d.source_indexer_id
		WHERE d.id=? AND d.provider_retry_after>? AND i.retry_at=d.provider_retry_after`, dl.ID, soon); n != 1 {
		t.Fatal("fetch{indexer_unavailable} wrote no provider_retry_after / indexer retry_at")
	}
	if n := f.count(`SELECT COUNT(*) FROM replacement_grabs`); n != 0 || len(mustBlocklist(t, f)) != 0 {
		t.Fatalf("grabs=%d blocklist=%v, want neither", n, mustBlocklist(t, f))
	}
	if n := f.count(`SELECT COUNT(*) FROM history_events WHERE type=?`, HistoryNoReplacement); n != 1 {
		t.Fatalf("no_replacement history = %d, want the reason recorded", n)
	}
	// The want is wanted again: nothing holds it in flight.
	if w, err := f.svc.wantableFromID(f.ctx, "episode:1:1:1"); err != nil || len(f.svc.notInFlight(f.ctx, nil)) != 0 || !f.svc.stillWanted(f.ctx, w) {
		t.Fatalf("the want is not re-queued: %v", err)
	}

	gone := f.grab(2, epTwo)
	f.runner.mu.Lock()
	f.runner.addErr = nil
	f.runner.mu.Unlock()
	tm = failed(ports.ReasonFetch, true)
	tm.FetchCause = ports.FetchCauseGone
	f.deferGrabs()
	f.observe(gone, terminalFact(gone, tm, "event 2"))
	if n := f.count(`SELECT COUNT(*) FROM replacement_grabs WHERE download_id=?`, gone.ID); n != 1 {
		t.Fatalf("a 404 fetch wrote %d grabs, want 1", n)
	}
	if bl := mustBlocklist(t, f); len(bl) != 1 || bl[0].ReleaseTitle != epTwo {
		t.Fatalf("blocklist = %+v, want the gone release", bl)
	}
}

// (i) A provisional failed{submit} re-opens on a /deliveries row and imports.
func TestProvisionalSubmitReopensOnDeliveryRow(t *testing.T) {
	f := setupV2(t, nil)
	f.runner.addErr = fmt.Errorf("connection reset")
	id, _ := f.svc.Grab(f.ctx, GrabRequest{MediaItemID: f.item, Season: 1, Episode: 1, Title: epOne,
		DownloadURL: "http://dl/" + epOne, Indexer: "idx", Protocol: "torrent", Size: 1000})
	dl := f.row(id)
	if dl.State != "submitting" {
		t.Fatalf("state = %s, want submitting after a lost Add response", dl.State)
	}
	f.runner.addErr = nil
	// Runner positively answers "no such token" for ten minutes.
	f.svc.notFoundSince.Store(dl.ID, time.Now().Add(-11*time.Minute))
	if err := f.svc.RefreshQueue(f.ctx); err != nil {
		t.Fatal(err)
	}
	sub := f.row(dl.ID)
	if sub.State != "failed" || sub.RunnerOutcomeReason != ports.ReasonSubmit || !sub.Provisional {
		t.Fatalf("row = %s/%s provisional=%v, want provisional failed{submit}", sub.State, sub.RunnerOutcomeReason, sub.Provisional)
	}
	// The POST had landed after all: Runner published it.
	pub := f.publish("Test.Show.S01E01.job-777.runner-ccc", "ccc", map[string]string{epOne + ".mkv": "video"})
	m := ports.Manifest{Generation: "ccc", Digest: pub.ManifestDigest}
	pub.Manifest = &m
	f.runner.deliveries = append(f.runner.deliveries, ports.RunnerFact{Kind: ports.FactTerminal, Transfer: dl.Transfer, Handle: "777",
		Terminal: &ports.RunnerTerminal{Outcome: ports.OutcomeCompleted, Publication: &pub, Salvage: "none"}, Source: "deliveries 1"})
	if err := f.svc.RefreshQueue(f.ctx); err != nil {
		t.Fatal(err)
	}
	got := f.row(dl.ID)
	if got.State != "imported" || got.RunnerOutcome != "completed" || got.Provisional {
		t.Fatalf("row = %s/%s provisional=%v (%s), want re-opened and imported", got.State, got.RunnerOutcome, got.Provisional, got.Error)
	}
}

// (i) A held row whose local deadline passes is held (stale), never failed;
// a late completed still imports.
func TestHeldLocalExpiryIsStaleNotFailed(t *testing.T) {
	f := setupV2(t, nil)
	dl := f.grab(1, epOne)
	control := &ports.DownloadControl{Version: 1, Revision: "3", Lifecycle: "held", Cause: "storage_unavailable", Message: "mount gone", Instance: "n"}
	st := ports.DownloadStatus{Handle: ports.Handle(dl.Handle), Transfer: dl.Transfer, State: ports.StateQueued, Control: control}
	f.observe(dl, ports.RunnerFact{Kind: ports.FactStatus, Transfer: dl.Transfer, Status: &st, Source: "event 1"})
	if _, err := f.db.W.ExecContext(f.ctx, `UPDATE downloads SET deadline_at=? WHERE id=?`, time.Now().Add(-time.Minute).UnixMilli(), dl.ID); err != nil {
		t.Fatal(err)
	}
	// Runner still lists it held at 0% every poll: the deadline, not
	// updated_at, is what makes it overdue.
	f.runner.statuses = []ports.DownloadStatus{st}
	if err := f.svc.RefreshQueue(f.ctx); err != nil {
		t.Fatal(err)
	}
	stale := f.row(dl.ID)
	if stale.State != "held" || !strings.HasPrefix(stale.Error, "held (stale)") || stale.RunnerOutcome != "" {
		t.Fatalf("row = %s %q outcome %q, want held (stale) and no terminal", stale.State, stale.Error, stale.RunnerOutcome)
	}
	if n, _ := f.db.CountOverdueDownloads(f.ctx, time.Now()); n != 1 {
		t.Fatalf("overdue = %d, want the stale hold counted", n)
	}
	pub := f.publish("Test.Show.S01E01.job-101.runner-ddd", "ddd", map[string]string{epOne + ".mkv": "video"})
	f.observe(stale, terminalFact(stale, ports.RunnerTerminal{Outcome: ports.OutcomeCompleted, Publication: &pub, Salvage: "none"}, "event 2"))
	if got := f.row(dl.ID); got.State != "imported" {
		t.Fatalf("late completed: %s (%s), want imported", got.State, got.Error)
	}
}

// (j) A publication with only a v1 delivery record imports through the
// adapter, in a real nuc3-shaped directory.
func TestV1RecordManifestImports(t *testing.T) {
	f := setupV2(t, nil)
	dl := f.grab(1, epOne)
	name := epOne + ".job-2333-url-fabba0b486c9.runner-29c89841a6c050b662dc794671e52708"
	pub := f.publish(name, "29c89841a6c050b662dc794671e52708", map[string]string{epOne + ".mkv": "video", "sample/" + epOne + "-sample.mkv": "s"})
	raw, _ := os.ReadFile(filepath.Join(pub.Path, ports.ManifestFileName))
	_ = os.Remove(filepath.Join(pub.Path, ports.ManifestFileName)) // a pre-M3 Runner writes no manifest file
	var m ports.Manifest
	_ = json.Unmarshal(raw, &m)
	m.Source = "v1_record"
	pub.Manifest = &m
	f.observe(dl, terminalFact(dl, ports.RunnerTerminal{Outcome: ports.OutcomeCompleted, Publication: &pub, Salvage: "none"}, "deliveries 12"))
	got := f.row(dl.ID)
	if got.State != "imported" {
		t.Fatalf("state = %s (%s), want imported through the v1 record", got.State, got.Error)
	}
	p, _, _ := f.db.PublicationByPath(f.ctx, pub.Path)
	files, _ := f.db.ListPublicationFiles(f.ctx, p.ID)
	outcomes := map[string]string{}
	for _, file := range files {
		outcomes[file.Path] = file.Outcome + "/" + file.DiscardReason
	}
	if outcomes[epOne+".mkv"] != "placed/" || outcomes["sample/"+epOne+"-sample.mkv"] != "discarded/rejected" || p.ManifestSource != "v1_record" {
		t.Fatalf("outcomes = %v source %s", outcomes, p.ManifestSource)
	}
}

// (j) A directory with neither a manifest file nor a v1 record is not a
// publication yet: skipped, retried on the next poll.
func TestManifestlessDirectoryIsSkipped(t *testing.T) {
	f := setupV2(t, nil)
	dl := f.grab(1, epOne)
	dir := filepath.Join(f.root, "Test.Show.S01E01.job-101.runner-eee")
	writeCompleted(t, filepath.Join(dir, epOne+".mkv"), 5)
	pub := ports.RunnerPublication{Path: dir, Generation: "eee", Kind: "completed"}
	f.observe(dl, terminalFact(dl, ports.RunnerTerminal{Outcome: ports.OutcomeCompleted, Publication: &pub, Salvage: "none"}, "event 1"))
	got := f.row(dl.ID)
	p, ok, _ := f.db.PublicationByPath(f.ctx, dir)
	if !ok || p.State != "discovered" || got.State != "downloaded" {
		t.Fatalf("row %s, publication %+v: want downloaded and the publication still discovered", got.State, p)
	}
	if n := f.count(`SELECT COUNT(*) FROM media_files`); n != 0 {
		t.Fatal("a manifest-less directory was imported")
	}
	// The manifest lands (Runner finished publishing): the next poll imports.
	f.publish("Test.Show.S01E01.job-101.runner-eee", "eee", map[string]string{epOne + ".mkv": "video"})
	if err := f.svc.RefreshQueue(f.ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.row(dl.ID); got.State != "imported" {
		t.Fatalf("after the manifest: %s (%s)", got.State, got.Error)
	}
}

// (l) Destination conflicts are decided by the current attempt, never by
// generation ids: both arrival orders end with one library file.
func TestSupersededFileDiscardedByCurrentAttempt(t *testing.T) {
	for _, order := range []string{"salvage-then-replacement", "replacement-then-salvage"} {
		t.Run(order, func(t *testing.T) {
			f := setupV2(t, nil)
			pack := f.grab(0, seasonPack)
			f.deferGrabs()
			tm := failed(ports.ReasonHealth, true)
			tm.Salvage = "pending"
			f.observe(pack, terminalFact(pack, tm, "event 1"))
			// The replacement for E01 is the current attempt for episode:1:1:1.
			repl := f.grab(1, epOne)
			// Generation ids that sort opposite to acquisition order.
			salvage := f.publish("Test.Show.S01.job-101.runner-zzzz", "zzzz", map[string]string{
				"Test.Show.S01E01.mkv": "salvaged e1", "Test.Show.S01E02.mkv": "salvaged e2"})
			salvage.Kind = "salvage"
			complete := f.publish("Test.Show.S01E01.job-102.runner-aaaa", "aaaa", map[string]string{epOne + ".mkv": "replacement e1"})
			applySalvage := func() {
				f.observe(f.row(pack.ID), ports.RunnerFact{Kind: ports.FactPublicationReady, Transfer: pack.Transfer, Publication: &salvage, Source: "event 5"})
			}
			applyReplacement := func() {
				r := f.row(repl.ID)
				f.observe(r, terminalFact(r, ports.RunnerTerminal{Outcome: ports.OutcomeCompleted, Publication: &complete, Salvage: "none"}, "event 6"))
			}
			if order == "salvage-then-replacement" {
				applySalvage()
				applyReplacement()
			} else {
				applyReplacement()
				applySalvage()
			}
			e1, _ := f.db.GetEpisodeID(f.ctx, f.item, 1, 1)
			if n := f.count(`SELECT COUNT(*) FROM media_file_episodes WHERE episode_id=?`, e1); n != 1 {
				t.Fatalf("E01 library files = %d, want exactly one", n)
			}
			sp, _, _ := f.db.PublicationByPath(f.ctx, salvage.Path)
			files, _ := f.db.ListPublicationFiles(f.ctx, sp.ID)
			byPath := map[string]sqlite.PublicationFile{}
			for _, file := range files {
				byPath[file.Path] = file
			}
			if e2 := byPath["Test.Show.S01E02.mkv"]; e2.Outcome != "placed" {
				t.Fatalf("salvaged E02 = %+v, want placed (the pack is current for it)", e2)
			}
			e1f := byPath["Test.Show.S01E01.mkv"]
			if e1f.Outcome == "placed" {
				t.Fatalf("salvaged E01 was placed over the current attempt: %+v", e1f)
			}
			if order == "replacement-then-salvage" && (e1f.Outcome != "discarded" || e1f.DiscardReason != "superseded") {
				t.Fatalf("salvaged E01 = %+v, want discarded{superseded}", e1f)
			}
			if order == "salvage-then-replacement" && (e1f.Outcome != "discarded" || e1f.DiscardReason != "superseded") {
				t.Fatalf("salvaged E01 = %+v, want superseded once the replacement placed", e1f)
			}
		})
	}
}

// (l, amended 2026-11-11) A file whose wantable has no acquisition record (a
// legacy row) is not left for a person: the import decides it by quality,
// as it does any file of the attempt Curator chose.
func TestNoAcquisitionRecordImportsByQuality(t *testing.T) {
	f := setupV2(t, nil)
	dl := f.grab(1, epOne)
	if _, err := f.db.W.ExecContext(f.ctx, `DELETE FROM wantable_attempts`); err != nil {
		t.Fatal(err)
	}
	pub := f.publish("Test.Show.S01E01.job-101.runner-fff", "fff", map[string]string{epOne + ".mkv": "video"})
	f.observe(dl, terminalFact(dl, ports.RunnerTerminal{Outcome: ports.OutcomeCompleted, Publication: &pub, Salvage: "none"}, "event 1"))
	p, _, _ := f.db.PublicationByPath(f.ctx, pub.Path)
	files, _ := f.db.ListPublicationFiles(f.ctx, p.ID)
	if len(files) != 1 || files[0].Outcome != "placed" || f.row(dl.ID).State != "imported" {
		t.Fatalf("publication %s files %+v row %s, want the file placed", p.State, files, f.row(dl.ID).State)
	}
}

// (m) The cursor belongs to the ordered history reader: SSE applies 42 at
// once but never moves it; the poll applies 41, no-ops 42, cursor -> 42.
func TestHistoryCursorOwnedByPoll(t *testing.T) {
	f := setupV2(t, []ports.Release{rel("Test.Show.S01E01.1080p.WEB-DL.x264-OTHER", 10)})
	one := f.grab(1, epOne)
	two := f.grab(2, epTwo)
	f.deferGrabs()
	f.runner.incarnation = "inc"
	if err := f.db.UpsertHistoryCursor(f.ctx, sqlite.RunnerHistoryCursor{ClientID: f.cfg.ID, Incarnation: "inc", SinceSeq: 40}); err != nil {
		t.Fatal(err)
	}
	f41 := terminalFact(one, failed(ports.ReasonHealth, true), "history inc:41")
	f41.HistorySeq = 41
	f42 := terminalFact(two, failed(ports.ReasonHealth, true), "event 42")
	f42.HistorySeq = 42
	f.runner.addHistory(f41)
	f.runner.addHistory(f42)
	f.svc.handleRunnerEvent(f.ctx, f.cfg, ports.ClientEvent{Fact: &f42}, map[int64]float64{}, new(time.Time))
	if f.row(two.ID).RunnerOutcome != "failed" {
		t.Fatal("SSE did not apply 42")
	}
	if c, _ := f.db.GetHistoryCursor(f.ctx, f.cfg.ID); c.SinceSeq != 40 {
		t.Fatalf("SSE moved the cursor to %d", c.SinceSeq)
	}
	f.svc.pollHistory(f.ctx, f.cfg, f.runner)
	if f.row(one.ID).RunnerOutcome != "failed" {
		t.Fatal("the poll did not apply the missed 41")
	}
	if n := f.count(`SELECT COUNT(*) FROM replacement_grabs`); n != 2 {
		t.Fatalf("replacement_grabs = %d, want one per failure (42 a no-op on replay)", n)
	}
	if c, _ := f.db.GetHistoryCursor(f.ctx, f.cfg.ID); c.SinceSeq != 42 {
		t.Fatalf("cursor = %d, want 42", c.SinceSeq)
	}
}

// A changed incarnation looks every outstanding transfer up by its token and
// resets the cursor to the new head.
func TestContinuityLossReconcilesByToken(t *testing.T) {
	f := setupV2(t, nil)
	dl := f.grab(1, epOne)
	f.deferGrabs()
	if err := f.db.UpsertHistoryCursor(f.ctx, sqlite.RunnerHistoryCursor{ClientID: f.cfg.ID, Incarnation: "old", SinceSeq: 900}); err != nil {
		t.Fatal(err)
	}
	f.runner.incarnation = "rebuilt"
	blames := true
	f.runner.admissions[dl.Transfer] = ports.RequestAdmission{ProtocolVersion: 1, RequestToken: dl.Transfer, JobID: dl.Handle,
		Outcome: "failed", TerminalOutcome: "UNPACK_FAILURE", FailureReason: ports.ReasonUnpack, BlamesRelease: &blames, Salvage: "none"}
	f.runner.addHistory(ports.RunnerFact{Kind: ports.FactTerminal, Transfer: "t-unrelated", HistorySeq: 5,
		Terminal: &ports.RunnerTerminal{Outcome: ports.OutcomeCompleted}})
	f.svc.pollHistory(f.ctx, f.cfg, f.runner)
	got := f.row(dl.ID)
	if got.RunnerOutcome != "failed" || got.RunnerOutcomeReason != ports.ReasonUnpack || !got.RunnerBlamesRelease {
		t.Fatalf("row = %s/%s blames=%v, want the receipt's typed failure", got.RunnerOutcome, got.RunnerOutcomeReason, got.RunnerBlamesRelease)
	}
	c, _ := f.db.GetHistoryCursor(f.ctx, f.cfg.ID)
	if c.Incarnation != "rebuilt" || c.SinceSeq != 5 {
		t.Fatalf("cursor = %+v, want the new incarnation at its head", c)
	}
}

// (n) Unfinished v1 work: a historical publication migrated from an accepted,
// partially placed receipt imports the rest on the first poll, and keeps the
// placed file placed.
func TestHistoricalPartiallyPlacedPublicationImportsTheRest(t *testing.T) {
	f := setupV2(t, nil)
	dl := f.grab(0, seasonPack)
	// What migration 0072 leaves: the row completed, a historical publication
	// from its v1 receipt with E01 placed and E02 pending.
	if _, err := f.db.W.ExecContext(f.ctx, `UPDATE downloads SET state='importing', runner_outcome='completed', runner_outcome_reason='legacy' WHERE id=?`, dl.ID); err != nil {
		t.Fatal(err)
	}
	pub := f.publish("Test.Show.S01.job-55.runner-v1gen", "v1gen", map[string]string{"Test.Show.S01E01.mkv": "e1", "Test.Show.S01E02.mkv": "e2"})
	raw, _ := os.ReadFile(filepath.Join(pub.Path, ports.ManifestFileName))
	_ = os.Remove(filepath.Join(pub.Path, ports.ManifestFileName))
	id, err := f.db.InsertPublication(f.ctx, sqlite.PublicationWrite{DownloadID: dl.ID, Generation: "v1gen", Kind: "completed", Path: pub.Path,
		ManifestJSON: string(raw), Source: "v1_record", Cohort: "historical"})
	if err != nil {
		t.Fatal(err)
	}
	var m ports.Manifest
	_ = json.Unmarshal(raw, &m)
	var seed []sqlite.PublicationFile
	for _, file := range m.Files {
		seed = append(seed, sqlite.PublicationFile{Path: file.Path, Bytes: file.Bytes, SHA256: file.SHA256})
	}
	_ = f.db.SeedPublicationFiles(f.ctx, id, seed)
	_ = f.db.SetPublicationFileOutcome(f.ctx, sqlite.PublicationFile{PublicationID: id, Path: "Test.Show.S01E01.mkv", Outcome: "placed", LibraryPath: "/library/e1.mkv"})
	if err := f.svc.RefreshQueue(f.ctx); err != nil {
		t.Fatal(err)
	}
	files, _ := f.db.ListPublicationFiles(f.ctx, id)
	got := map[string]sqlite.PublicationFile{}
	for _, file := range files {
		got[file.Path] = file
	}
	if got["Test.Show.S01E01.mkv"].Outcome != "placed" || got["Test.Show.S01E01.mkv"].LibraryPath != "/library/e1.mkv" {
		t.Fatalf("the placed file was re-imported: %+v", got["Test.Show.S01E01.mkv"])
	}
	if got["Test.Show.S01E02.mkv"].Outcome != "placed" {
		t.Fatalf("E02 = %+v (%s), want imported on the first poll", got["Test.Show.S01E02.mkv"], f.row(dl.ID).Error)
	}
}

// (o) A historical publication, fully imported, is never deleted by the
// inline cleanup or the sweep.
func TestHistoricalPublicationImportsButIsNeverSwept(t *testing.T) {
	f := setupV2(t, nil)
	dl := f.grab(1, epOne)
	pub := f.publish("Test.Show.S01E01.job-9.runner-hist", "hist", map[string]string{epOne + ".mkv": "video"})
	cfg := f.cfg
	cfg.RemoveCompleted = true
	if err := f.db.UpdateDownloadClient(f.ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.InsertPublication(f.ctx, sqlite.PublicationWrite{DownloadID: dl.ID, Generation: "hist", Kind: "completed", Path: pub.Path, Cohort: "historical"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.W.ExecContext(f.ctx, `UPDATE downloads SET runner_outcome='completed', state='downloaded', import_path=?, save_path=? WHERE id=?`, pub.Path, pub.Path, dl.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.RefreshQueue(f.ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.row(dl.ID); got.State != "imported" {
		t.Fatalf("historical publication did not import: %s (%s)", got.State, got.Error)
	}
	ref := downloadRef{ID: dl.ID, MediaItemID: dl.MediaItemID, ClientID: dl.ClientID, Handle: dl.Handle, GenerationKey: dl.GenerationKey, ImportPath: pub.Path}
	f.svc.cleanupAfterImport(f.ctx, ref)
	if err := f.svc.CleanupPayloads(f.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(pub.Path, epOne+".mkv")); err != nil {
		t.Fatalf("a historical publication was deleted: %v", err)
	}
	f.runner.mu.Lock()
	removed := len(f.runner.removed)
	f.runner.mu.Unlock()
	if removed != 0 {
		t.Fatal("cleanup asked Runner to delete a publication")
	}
}

// The historical cohort is marked once: a later start marks nothing.
func TestHistoricalCohortIsMarkedOnce(t *testing.T) {
	f := setupV2(t, nil)
	old := filepath.Join(f.root, "Old.Payload")
	writeCompleted(t, filepath.Join(old, "a.mkv"), 3)
	past := time.Now().Add(-time.Hour)
	_ = os.Chtimes(old, past, past)
	fresh := filepath.Join(f.root, "Being.Published")
	writeCompleted(t, filepath.Join(fresh, "b.mkv"), 3)
	n, err := f.svc.MarkHistoricalCohort(f.ctx)
	if err != nil || n != 1 {
		t.Fatalf("marked %d (%v), want only the settled directory", n, err)
	}
	p, ok, _ := f.db.PublicationByPath(f.ctx, old)
	if !ok || p.Cohort != "historical" || p.DownloadID != 0 || p.ManifestSource != "none" {
		t.Fatalf("publication = %+v", p)
	}
	_ = os.Chtimes(fresh, past, past)
	if n, _ := f.svc.MarkHistoricalCohort(f.ctx); n != 0 {
		t.Fatalf("second start marked %d", n)
	}
}

// (review #1) A persistence outage after files were placed leaves the
// publication offerable again, not stranded in importing: the next poll
// completes it from the committed placements, with one library file.
func TestPersistenceOutageMidImportRetriesFromDiscovered(t *testing.T) {
	f := setupV2(t, nil)
	dl := f.grab(1, epOne)
	pub := f.publish("Test.Show.S01E01.job-101.runner-out", "out", map[string]string{epOne + ".mkv": "video"})
	if _, err := f.db.W.ExecContext(f.ctx, `CREATE TRIGGER outage BEFORE UPDATE OF state ON publications WHEN NEW.state='imported'
		BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`); err != nil {
		t.Fatal(err)
	}
	f.observe(dl, terminalFact(dl, ports.RunnerTerminal{Outcome: ports.OutcomeCompleted, Publication: &pub, Salvage: "none"}, "event 1"))
	p, _, _ := f.db.PublicationByPath(f.ctx, pub.Path)
	if p.State != "discovered" || !strings.Contains(p.Error, "disk I/O error") {
		t.Fatalf("publication after the outage = %s %q, want discovered with the error", p.State, p.Error)
	}
	if _, err := f.db.W.ExecContext(f.ctx, `DROP TRIGGER outage`); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.RefreshQueue(f.ctx); err != nil {
		t.Fatal(err)
	}
	p, _, _ = f.db.PublicationByPath(f.ctx, pub.Path)
	// Imported, then removed by the per-file cleanup (M4) as every file had
	// evidence.
	if got := f.row(dl.ID); got.State != "imported" || p.State != "removed" {
		t.Fatalf("after recovery row %s (%s), publication %s, want the row imported and the publication imported and cleaned up", got.State, got.Error, p.State)
	}
	if n := f.count(`SELECT COUNT(*) FROM media_files WHERE media_item_id=?`, f.item); n != 1 {
		t.Fatalf("library files = %d, want one (the retry reused the placement)", n)
	}
}

// (review #2) A job reporting the same post-processing stage poll after poll
// is not moving: its downloading deadline stays where it was, so a stalled
// unpack goes overdue. A new stage is movement.
func TestRepeatedStageDoesNotMoveTheDeadline(t *testing.T) {
	f := setupV2(t, nil)
	dl := f.grab(1, epOne)
	status := func(stage string) {
		st := ports.DownloadStatus{Handle: ports.Handle(dl.Handle), Transfer: dl.Transfer, State: ports.StateDownloading, Progress: 1, Stage: stage}
		f.observe(f.row(dl.ID), ports.RunnerFact{Kind: ports.FactStatus, Transfer: dl.Transfer, Status: &st, Source: "poll"})
	}
	status("unpack")
	if got := f.row(dl.ID); got.RunnerStage != "unpack" || got.DeadlineAt.Before(time.Now()) {
		t.Fatalf("first unpack: stage %q deadline %s, want stored and in the future", got.RunnerStage, got.DeadlineAt)
	}
	past := time.Now().Add(-time.Minute).UnixMilli()
	if _, err := f.db.W.ExecContext(f.ctx, `UPDATE downloads SET deadline_at=? WHERE id=?`, past, dl.ID); err != nil {
		t.Fatal(err)
	}
	status("unpack")
	status("") // a tick carries no stage and keeps the stored one
	status("unpack")
	if got := f.row(dl.ID); got.DeadlineAt.UnixMilli() != past {
		t.Fatalf("a repeated stage moved the deadline to %s", got.DeadlineAt)
	}
	if n, err := f.db.CountOverdueDownloads(f.ctx, time.Now()); err != nil || n != 1 {
		t.Fatalf("overdue = %d (%v), want the stalled unpack", n, err)
	}
	status("repair")
	if got := f.row(dl.ID); !got.DeadlineAt.After(time.Now()) || got.RunnerStage != "repair" {
		t.Fatalf("a new stage: stage %q deadline %s, want moved", got.RunnerStage, got.DeadlineAt)
	}
}

// (review #3) A fact carrying a job handle but no transfer token (a hold
// from job_control_changed) reaches a tokened row only when the queue names
// that handle as the row's token. After a Runner state wipe the same job id
// belongs to a new transfer: the hold is dropped and counted.
func TestHandleOnlyFactNeedsTheQueueToken(t *testing.T) {
	f := setupV2(t, nil)
	dl := f.grab(1, epOne)
	hold := func() {
		st := ports.DownloadStatus{Handle: ports.Handle(dl.Handle), State: ports.StateQueued, OperationalState: "held", Message: "waiting for disk"}
		f.svc.handleRunnerEvent(f.ctx, f.cfg, ports.ClientEvent{Kind: ports.EventStage, Fact: &ports.RunnerFact{Kind: ports.FactStatus,
			Handle: ports.Handle(dl.Handle), Status: &st, Source: "event 9"}}, map[int64]float64{}, new(time.Time))
	}
	// No queue poll yet: nothing vouches for the handle.
	hold()
	if got := f.row(dl.ID); got.State != "downloading" {
		t.Fatalf("an unvouched handle-only fact moved the row to %s", got.State)
	}
	// The queue says job 101 is another transfer now (a reused job id).
	f.svc.queueTokens.Store(f.cfg.ID, map[string]string{dl.Handle: "t-new-after-wipe"})
	before := f.svc.FactMismatches()
	hold()
	if got := f.row(dl.ID); got.State != "downloading" {
		t.Fatalf("a reused job id's hold landed on the old row: %s", got.State)
	}
	if f.svc.FactMismatches() != before+1 {
		t.Fatalf("mismatches = %d, want +1", f.svc.FactMismatches())
	}
	// The queue names the row's own token for the handle: the hold applies.
	f.svc.queueTokens.Store(f.cfg.ID, map[string]string{dl.Handle: dl.Transfer})
	hold()
	if got := f.row(dl.ID); got.State != "held" || got.Error != "waiting for disk" {
		t.Fatalf("a vouched handle-only hold = %s %q, want held", got.State, got.Error)
	}
}

// (review #5) Both timings start when Runner's terminal is recorded: a
// completion whose manifest has not landed is already being timed, and a
// failure's clock runs before the grab worker's first search.
func TestTimingsStartAtTerminalReceipt(t *testing.T) {
	f := setupV2(t, []ports.Release{rel("Test.Show.S01E02.1080p.WEB-DL.x264-OTHER", 10)})
	f.svc.SetTimings(pipeline.New(f.db))
	open := func(kind string, dl sqlite.Download) int {
		return f.count(`SELECT COUNT(*) FROM pipeline_timings WHERE kind=? AND transfer=? AND ended_at=0`, kind, dl.Transfer)
	}
	done := f.grab(1, epOne)
	dir := filepath.Join(f.root, "Test.Show.S01E01.job-101.runner-tim")
	writeCompleted(t, filepath.Join(dir, epOne+".mkv"), 5) // no manifest yet
	pub := ports.RunnerPublication{Path: dir, Generation: "tim", Kind: "completed"}
	f.observe(done, terminalFact(done, ports.RunnerTerminal{Outcome: ports.OutcomeCompleted, Publication: &pub, Salvage: "none"}, "event 1"))
	if n := open("runner_done_to_import", done); n != 1 {
		t.Fatalf("runner_done_to_import open = %d, want started at the terminal while the manifest is missing", n)
	}
	failedRow := f.grab(2, epTwo)
	f.deferGrabs()
	f.observe(failedRow, terminalFact(failedRow, failed(ports.ReasonHealth, true), "event 2"))
	if n := open("failure_to_next_grab", failedRow); n != 1 {
		t.Fatalf("failure_to_next_grab open = %d, want started with the failure", n)
	}
	f.svc.RunReplacementGrabs(f.ctx)
	if n := f.count(`SELECT COUNT(*) FROM pipeline_timings WHERE kind='failure_to_next_grab' AND transfer=? AND ended_at>0`, failedRow.Transfer); n != 1 {
		g, _, _ := f.db.GetReplacementGrab(f.ctx, failedRow.ID)
		t.Fatalf("the grab worker did not end failure_to_next_grab on submit (grab %+v)", g)
	}
	// An indexer outage writes no grab, so no clock.
	other := f.grab(1, "Test.Show.S01E01.720p.WEB-DL.x264-ALT")
	tm := failed(ports.ReasonFetch, false)
	tm.FetchCause = ports.FetchCauseIndexerUnavailable
	f.observe(other, terminalFact(other, tm, "event 3"))
	if n := f.count(`SELECT COUNT(*) FROM pipeline_timings WHERE kind='failure_to_next_grab' AND transfer=?`, other.Transfer); n != 0 {
		t.Fatal("an indexer outage started failure_to_next_grab")
	}
}

// (review #6) The held (stale) sweep reads its own query: seventy other
// overdue rows (another state, ahead of it by id) cannot crowd a stale hold
// out of the sweep.
func TestStaleHoldIsSweptPastManyOtherOverdueRows(t *testing.T) {
	f := setupV2(t, nil)
	past := time.Now().Add(-time.Hour).UnixMilli()
	for i := 0; i < 70; i++ {
		if _, err := f.db.W.ExecContext(f.ctx, `INSERT INTO downloads(media_item_id,protocol,release_title,state,client_id,transfer,deadline_at,added_at,updated_at)
			VALUES(?,'torrent',?,'downloading',1,?,?,1,1)`, f.item, fmt.Sprintf("other-%d", i), fmt.Sprintf("t-other-%d", i), past); err != nil {
			t.Fatal(err)
		}
	}
	dl := f.grab(1, epOne)
	control := &ports.DownloadControl{Version: 1, Revision: "3", Lifecycle: "held", Message: "mount gone"}
	st := ports.DownloadStatus{Handle: ports.Handle(dl.Handle), Transfer: dl.Transfer, State: ports.StateQueued, Control: control}
	f.observe(dl, ports.RunnerFact{Kind: ports.FactStatus, Transfer: dl.Transfer, Status: &st, Source: "event 1"})
	if _, err := f.db.W.ExecContext(f.ctx, `UPDATE downloads SET deadline_at=? WHERE id=?`, past, dl.ID); err != nil {
		t.Fatal(err)
	}
	f.svc.sweepDeadlines(f.ctx, f.cfg, f.runner)
	if got := f.row(dl.ID); !strings.HasPrefix(got.Error, "held (stale)") {
		t.Fatalf("stale hold behind 70 overdue rows = %q, want swept", got.Error)
	}
}

// (review #8) A provisional row is in the poll's rows: a migrated parked
// failure whose job Runner still lists re-opens from the queue poll alone,
// with no event.
func TestProvisionalRowReopensFromThePoll(t *testing.T) {
	f := setupV2(t, nil)
	dl := f.grab(1, epOne)
	if _, err := f.db.W.ExecContext(f.ctx, `UPDATE downloads SET state='failed', runner_outcome='failed', runner_outcome_reason='parked',
		provisional=1, error='parked: Runner retains source files' WHERE id=?`, dl.ID); err != nil {
		t.Fatal(err)
	}
	f.runner.statuses = []ports.DownloadStatus{{Handle: ports.Handle(dl.Handle), Transfer: dl.Transfer, State: ports.StateDownloading, Progress: 0.4}}
	if err := f.svc.RefreshQueue(f.ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.row(dl.ID); got.State != "downloading" || got.Provisional || got.RunnerOutcome != "" || got.Progress != 0.4 {
		t.Fatalf("row = %s/%s provisional=%v progress %.2f, want re-opened downloading", got.State, got.RunnerOutcome, got.Provisional, got.Progress)
	}
}

// (review #10) A completed terminal whose directory is already another
// download's publication fails that row's import visibly, instead of leaving
// it downloaded with no publication and no deadline. A historical directory
// with no row is adopted by the download that produced it.
func TestPublicationPathCollisionFailsTheImport(t *testing.T) {
	f := setupV2(t, nil)
	first := f.grab(1, epOne)
	pub := f.publish("Test.Show.S01E01.job-101.runner-col", "col", map[string]string{epOne + ".mkv": "video"})
	f.observe(first, terminalFact(first, ports.RunnerTerminal{Outcome: ports.OutcomeCompleted, Publication: &pub, Salvage: "none"}, "event 1"))
	second := f.grab(2, epTwo)
	f.observe(second, terminalFact(second, ports.RunnerTerminal{Outcome: ports.OutcomeCompleted, Publication: &pub, Salvage: "none"}, "event 2"))
	got := f.row(second.ID)
	if got.State != "failed" || got.RunnerOutcome != "completed" || !strings.Contains(got.Error, fmt.Sprintf("publication of download %d", first.ID)) {
		t.Fatalf("colliding row = %s/%s %q, want its import failed naming the owner", got.State, got.RunnerOutcome, got.Error)
	}
	if p, _, _ := f.db.PublicationByPath(f.ctx, pub.Path); p.DownloadID != first.ID {
		t.Fatalf("publication owner = %d, want unchanged %d", p.DownloadID, first.ID)
	}

	third := f.grab(3, "Test.Show.S01E03.1080p.WEB-DL.x264-GRP")
	hist := f.publish("Test.Show.S01E03.job-103.runner-hist3", "hist3", map[string]string{"Test.Show.S01E03.1080p.WEB-DL.x264-GRP.mkv": "video"})
	if _, err := f.db.InsertPublication(f.ctx, sqlite.PublicationWrite{Kind: "completed", Path: hist.Path, Cohort: "historical", Source: "none"}); err != nil {
		t.Fatal(err)
	}
	f.observe(third, terminalFact(third, ports.RunnerTerminal{Outcome: ports.OutcomeCompleted, Publication: &hist, Salvage: "none"}, "event 3"))
	p, _, _ := f.db.PublicationByPath(f.ctx, hist.Path)
	if p.DownloadID != third.ID || p.Cohort != "historical" {
		t.Fatalf("historical directory = %+v, want adopted by download %d and still historical", p, third.ID)
	}
}

// (review #11) An operator removing a Runner row records a cancelled
// terminal: never a park, never a re-grab or a blocklist; Runner is asked to
// cancel the job; the row stays (dismissed) for its token.
func TestRemovingARunnerRowCancelsItNeverParks(t *testing.T) {
	f := setupV2(t, []ports.Release{rel("Test.Show.S01E01.1080p.WEB-DL.x264-OTHER", 10)})
	dl := f.grab(1, epOne)
	if err := f.svc.RemoveDownload(f.ctx, dl.ID, true); err != nil {
		t.Fatal(err)
	}
	got := f.row(dl.ID)
	if got.State != "failed" || got.RunnerOutcome != ports.OutcomeCancelled || got.Provisional || got.ParkedReason != "" ||
		f.count(`SELECT parked_at FROM downloads WHERE id=?`, dl.ID) != 0 {
		t.Fatalf("removed row = %s/%s provisional=%v parked %q, want a final cancelled terminal and no park", got.State, got.RunnerOutcome, got.Provisional, got.ParkedReason)
	}
	f.runner.mu.Lock()
	removed := len(f.runner.removed)
	f.runner.mu.Unlock()
	if removed != 1 {
		t.Fatalf("Runner remove calls = %d, want the job cancelled", removed)
	}
	if n := f.count(`SELECT COUNT(*) FROM replacement_grabs`); n != 0 || len(mustBlocklist(t, f)) != 0 {
		t.Fatal("an operator cancel re-grabbed or blocklisted")
	}
	if n := f.count(`SELECT COUNT(*) FROM downloads WHERE id=? AND activity_dismissed=1`, dl.ID); n != 1 {
		t.Fatal("the cancelled row is not dismissed from Activity")
	}
}

// (review #13: half-placed restart) A crash mid-import leaves the
// publication importing with one file already placed. After restart the
// poll re-offers it (no import is running for it) and only the rest is
// imported.
func TestCrashMidImportResumesOnlyTheRest(t *testing.T) {
	f := setupV2(t, nil)
	dl := f.grab(0, seasonPack)
	pub := f.publish("Test.Show.S01.job-101.runner-half", "half", map[string]string{"Test.Show.S01E01.mkv": "e1", "Test.Show.S01E02.mkv": "e2"})
	// What the crash leaves: the terminal recorded, the publication
	// importing, E01's outcome written, E02 not reached.
	if _, err := f.db.W.ExecContext(f.ctx, `UPDATE downloads SET state='importing', runner_outcome='completed', import_path=?, save_path=? WHERE id=?`, pub.Path, pub.Path, dl.ID); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(pub.Path, ports.ManifestFileName))
	id, err := f.db.InsertPublication(f.ctx, sqlite.PublicationWrite{DownloadID: dl.ID, Generation: "half", Kind: "completed", Path: pub.Path,
		ManifestJSON: string(raw), Source: "file", Cohort: "live"})
	if err != nil {
		t.Fatal(err)
	}
	var m ports.Manifest
	_ = json.Unmarshal(raw, &m)
	var seed []sqlite.PublicationFile
	for _, file := range m.Files {
		seed = append(seed, sqlite.PublicationFile{Path: file.Path, Bytes: file.Bytes, SHA256: file.SHA256})
	}
	_ = f.db.SeedPublicationFiles(f.ctx, id, seed)
	_ = f.db.SetPublicationFileOutcome(f.ctx, sqlite.PublicationFile{PublicationID: id, Path: "Test.Show.S01E01.mkv", Outcome: "placed", LibraryPath: "/library/e1.mkv"})
	_ = f.db.SetPublicationState(f.ctx, id, "importing", "")
	if err := f.svc.RefreshQueue(f.ctx); err != nil {
		t.Fatal(err)
	}
	p, _ := f.db.GetPublication(f.ctx, id)
	files, _ := f.db.ListPublicationFiles(f.ctx, id)
	got := map[string]sqlite.PublicationFile{}
	for _, file := range files {
		got[file.Path] = file
	}
	if (p.State != "imported" && p.State != "removed") || got["Test.Show.S01E02.mkv"].Outcome != "placed" {
		t.Fatalf("after restart publication %s, E02 %+v (%s): want the orphaned import resumed", p.State, got["Test.Show.S01E02.mkv"], f.row(dl.ID).Error)
	}
	if got["Test.Show.S01E01.mkv"].LibraryPath != "/library/e1.mkv" {
		t.Fatalf("E01 was re-imported: %+v", got["Test.Show.S01E01.mkv"])
	}
}

// (review #13: manifest boundary) What a manifest may say before anything
// is imported from it.
func TestManifestShapeBoundary(t *testing.T) {
	good := []ports.ManifestFile{{Path: "Season 01/a.mkv", Bytes: 3, SHA256: strings.Repeat("a", 64)}}
	digest, _ := ports.ManifestDigest(good)
	if err := verifyManifestShape(ports.Manifest{Generation: "g", Files: good, Digest: digest}, "g"); err != nil {
		t.Fatalf("a good manifest was refused: %v", err)
	}
	cases := map[string]ports.Manifest{
		"no files":            {Generation: "g"},
		"generation mismatch": {Generation: "other", Files: good},
		"parent escape":       {Files: []ports.ManifestFile{{Path: "../a.mkv"}}},
		"absolute":            {Files: []ports.ManifestFile{{Path: "/etc/a.mkv"}}},
		"backslash":           {Files: []ports.ManifestFile{{Path: `a\b.mkv`}}},
		"empty segment":       {Files: []ports.ManifestFile{{Path: "a//b.mkv"}}},
		"dot segment":         {Files: []ports.ManifestFile{{Path: "./a.mkv"}}},
		"empty path":          {Files: []ports.ManifestFile{{Path: ""}}},
		"negative size":       {Files: []ports.ManifestFile{{Path: "a.mkv", Bytes: -1}}},
		"digest mismatch":     {Files: good, Digest: strings.Repeat("0", 64)},
	}
	for name, m := range cases {
		if err := verifyManifestShape(m, "g"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// End to end: a manifest naming a path outside its publication fails
	// the publication and places nothing.
	f := setupV2(t, nil)
	dl := f.grab(1, epOne)
	dir := filepath.Join(f.root, "Test.Show.S01E01.job-101.runner-esc")
	writeCompleted(t, filepath.Join(dir, epOne+".mkv"), 5)
	bad := ports.Manifest{Generation: "esc", Files: []ports.ManifestFile{{Path: "../" + epOne + ".mkv", Bytes: 5}}}
	rawBad, _ := json.Marshal(bad)
	if err := os.WriteFile(filepath.Join(dir, ports.ManifestFileName), rawBad, 0o644); err != nil {
		t.Fatal(err)
	}
	pub := ports.RunnerPublication{Path: dir, Generation: "esc", Kind: "completed"}
	f.observe(dl, terminalFact(dl, ports.RunnerTerminal{Outcome: ports.OutcomeCompleted, Publication: &pub, Salvage: "none"}, "event 1"))
	p, _, _ := f.db.PublicationByPath(f.ctx, dir)
	if p.State != "import_failed" || !strings.Contains(p.Error, "manifest refused") || f.count(`SELECT COUNT(*) FROM media_files`) != 0 {
		t.Fatalf("escaping manifest: publication %s %q, library files %d", p.State, p.Error, f.count(`SELECT COUNT(*) FROM media_files`))
	}
}

// (review #13: sidecar conflict, superseded 2026-11-11) A subtitle is placed
// with its video by the import; it is never media, so Resolve still cannot
// place it as one.
func TestSubtitleIsPlacedWithItsVideoNeverAsMedia(t *testing.T) {
	f := setupV2(t, nil)
	dl := f.grab(1, epOne)
	pub := f.publish("Test.Show.S01E01.job-101.runner-srt", "srt", map[string]string{epOne + ".mkv": "video", epOne + ".en.srt": "subs"})
	f.observe(dl, terminalFact(dl, ports.RunnerTerminal{Outcome: ports.OutcomeCompleted, Publication: &pub, Salvage: "none"}, "event 1"))
	p, _, _ := f.db.PublicationByPath(f.ctx, pub.Path)
	files, _ := f.db.ListPublicationFiles(f.ctx, p.ID)
	got := map[string]sqlite.PublicationFile{}
	for _, file := range files {
		got[file.Path] = file
	}
	video := got[epOne+".mkv"]
	if f.row(dl.ID).State != "imported" || video.Outcome != "placed" {
		t.Fatalf("media did not import: row %s publication %s files %+v", f.row(dl.ID).State, p.State, got)
	}
	want := strings.TrimSuffix(video.LibraryPath, filepath.Ext(video.LibraryPath)) + ".en.srt"
	if srt := got[epOne+".en.srt"]; srt.Outcome != "placed" || srt.LibraryPath != want {
		t.Fatalf("subtitle = %+v, want placed at %s", srt, want)
	}
	if body, err := os.ReadFile(want); err != nil || string(body) != "subs" {
		t.Fatalf("library subtitle = %q (%v)", body, err)
	}
	if f.count(`SELECT COUNT(*) FROM media_files`) != 1 {
		t.Fatal("a subtitle became a media file")
	}
}

// Review r2 (2): a file whose want's current attempt is dead (failed, gone)
// is not superseded by it: the import decides it by quality.
func TestDeadCurrentAttemptDoesNotSupersede(t *testing.T) {
	f := setupV2(t, nil)
	first := f.grab(1, epOne)
	second := f.grab(1, "Test.Show.S01E01.1080p.WEB-DL.x264-OTHER")
	if _, err := f.db.W.ExecContext(f.ctx, `UPDATE downloads SET state='failed' WHERE id=?`, second.ID); err != nil {
		t.Fatal(err)
	}
	pub := f.publish("Test.Show.S01E01.job-610.runner-dead", "dead", map[string]string{epOne + ".mkv": "video"})
	f.observe(first, terminalFact(first, ports.RunnerTerminal{Outcome: ports.OutcomeCompleted, Publication: &pub, Salvage: "none"}, "event 1"))
	p, _, _ := f.db.PublicationByPath(f.ctx, pub.Path)
	files, _ := f.db.ListPublicationFiles(f.ctx, p.ID)
	if len(files) != 1 || files[0].Outcome != "placed" || f.count(`SELECT COUNT(*) FROM media_files`) != 1 {
		t.Fatalf("files %+v, want placed: the current attempt is dead", files)
	}
}
