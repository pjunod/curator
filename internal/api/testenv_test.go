package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/app/acquisition"
	"github.com/pjunod/monarr/internal/app/discover"
	"github.com/pjunod/monarr/internal/app/health"
	"github.com/pjunod/monarr/internal/app/library"
	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/bus"
	"github.com/pjunod/monarr/internal/infra/scheduler"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// The fully-wired test server.
//
// newLibraryServer above deliberately wires only what the library handlers
// need, and that is why this package sat at 29% while every other layer was
// above 85%: most handlers could not be reached at all from a test, because
// the Deps they read were nil. This one wires a real migrated SQLite, the
// real library and acquisition services, and fakes only at the ports —
// which is the same shape internal/compat's conformance harness uses.
//
// Fakes are at the seams and nowhere else. A handler test that stubs the
// service it is testing proves the stub works.

// apiEnv is one wired server plus the handles a test needs to arrange state.
type apiEnv struct {
	h    http.Handler
	srv  *Server
	db   *sqlite.DB
	bus  *bus.Bus
	lib  *library.Service
	acq  *acquisition.Service
	root string
}

// fakeIndexer answers a fixed candidate list, so the search and grab
// handlers have something to rank without a network. The release is sized
// like a real 1080p feature: the plausibility gate (ADR 0015's neighbour,
// Phase 8) refuses an implausibly small one before the grab, which would
// make these tests fail for a reason that is not about the handler.
type fakeIndexer struct{ cfg ports.IndexerConfig }

func (f fakeIndexer) Test(context.Context) error { return nil }

func (f fakeIndexer) Search(_ context.Context, _ domain.SearchQuery) ([]ports.Release, error) {
	return []ports.Release{{
		Title:       "Fight.Club.1999.1080p.WEB-DL.x264-TEST",
		DownloadURL: "http://indexer.invalid/1",
		Indexer:     f.cfg.Name,
		IndexerID:   f.cfg.ID,
		Protocol:    f.cfg.Protocol,
		Size:        8 << 30,
		Seeders:     40,
		PublishDate: time.Now().Add(-time.Hour),
	}}, nil
}

func (f fakeIndexer) FetchRSS(ctx context.Context) ([]ports.Release, error) {
	return f.Search(ctx, domain.SearchQuery{})
}

// fakeClient accepts anything and reports nothing in flight, so the queue
// handlers have a client to talk to without one existing.
type fakeClient struct{ cfg ports.ClientConfig }

func (f fakeClient) Test(context.Context) error { return nil }

func (f fakeClient) Add(context.Context, string, string) (ports.Handle, error) {
	return ports.Handle("fake-download-1"), nil
}

func (f fakeClient) Statuses(context.Context) ([]ports.DownloadStatus, error) { return nil, nil }

func (f fakeClient) Remove(context.Context, ports.Handle, bool) error { return nil }

// fakeNotifier accepts every delivery, so the notifier /test endpoints have
// a far side that answers.
type fakeNotifier struct{}

func (fakeNotifier) Send(context.Context, ports.Notification) error { return nil }

func (fakeNotifier) Test(context.Context) error { return nil }

// newAPIEnv wires everything the API layer can reach.
func newAPIEnv(t *testing.T) *apiEnv {
	t.Helper()

	db, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	b := bus.New(nil)
	t.Cleanup(b.Close)

	lib := library.New(db, stubProvider{configured: true}, b, nil)

	indexerFactory := func(cfg ports.IndexerConfig) ports.Indexer { return fakeIndexer{cfg} }
	clientFactory := func(cfg ports.ClientConfig) ports.DownloadClient { return fakeClient{cfg} }
	acq := acquisition.New(db, b, nil, indexerFactory, clientFactory)

	sched := scheduler.New(nil, b, nil)
	for _, name := range []string{ScanTaskName, "rss.sync", "backlog.search"} {
		if err := sched.Register(scheduler.Task{
			Name: name, Interval: time.Hour,
			Fn: func(context.Context) error { return nil },
		}); err != nil {
			t.Fatal(err)
		}
	}
	runCtx, cancel := context.WithCancel(ctx)
	if err := sched.Start(runCtx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); sched.Wait() })

	root := t.TempDir()
	if _, err := lib.AddRootFolder(ctx, root, domain.KindMixed); err != nil {
		t.Fatal(err)
	}

	srv := New(Deps{
		Bus:            b,
		Health:         health.NewRegistry(nil),
		Callers:        NewCallerRegistry(),
		Scheduler:      sched,
		DB:             fakeDB{v: 2},
		Library:        lib,
		Acquisition:    acq,
		Discover:       discover.New(nil, nil),
		Store:          db,
		Settings:       db,
		IndexerFactory: indexerFactory,
		ClientFactory:  clientFactory,
		// Without this the notifier /test endpoints panic on a nil factory
		// rather than answering, so their success paths were untestable.
		NotifierFactory: func(ports.NotifierConfig) ports.Notifier { return fakeNotifier{} },
		Version:         "test",
		Commit:          "cafebabe",
		DataDir:         t.TempDir(),
		StartedAt:       time.Now().Add(-time.Minute),
	})

	return &apiEnv{h: srv.Handler(), srv: srv, db: db, bus: b, lib: lib, acq: acq, root: root}
}

// get/post/put/patch/del are thin wrappers over `do`, so a test reads as the
// request it is making rather than as HTTP plumbing.
func (e *apiEnv) get(t *testing.T, path string) *httptestRecorder {
	t.Helper()
	return &httptestRecorder{do(t, e.h, http.MethodGet, path, "")}
}

func (e *apiEnv) post(t *testing.T, path, body string) *httptestRecorder {
	t.Helper()
	return &httptestRecorder{do(t, e.h, http.MethodPost, path, body)}
}

func (e *apiEnv) put(t *testing.T, path, body string) *httptestRecorder {
	t.Helper()
	return &httptestRecorder{do(t, e.h, http.MethodPut, path, body)}
}

func (e *apiEnv) patch(t *testing.T, path, body string) *httptestRecorder {
	t.Helper()
	return &httptestRecorder{do(t, e.h, http.MethodPatch, path, body)}
}

func (e *apiEnv) del(t *testing.T, path string) *httptestRecorder {
	t.Helper()
	return &httptestRecorder{do(t, e.h, http.MethodDelete, path, "")}
}

// httptestRecorder adds assertions to the recorder so every test does not
// re-spell "check the code, then unmarshal, then check the error".
type httptestRecorder struct {
	*httptest.ResponseRecorder
}

// expect fails unless the status matches, printing the body — which is where
// the reason always is.
func (r *httptestRecorder) expect(t *testing.T, want int) *httptestRecorder {
	t.Helper()
	if r.Code != want {
		t.Fatalf("status = %d, want %d; body: %s", r.Code, want, r.Body.String())
	}
	return r
}

// into unmarshals the body, failing the test rather than returning an error:
// a malformed body is never the thing under test.
func (r *httptestRecorder) into(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.Body.Bytes(), v); err != nil {
		t.Fatalf("decoding %s: %v", r.Body.String(), err)
	}
}

// addMovie seeds one library item and returns its id — the precondition for
// most of the per-item endpoints.
func (e *apiEnv) addMovie(t *testing.T) int64 {
	t.Helper()
	rr := e.post(t, "/api/v1/library", `{"kind":"movie","tmdbId":550}`)
	if rr.Code != http.StatusCreated && rr.Code != http.StatusOK {
		t.Fatalf("seeding a movie: status %d, body %s", rr.Code, rr.Body.String())
	}
	var item struct {
		ID int64 `json:"id"`
	}
	rr.into(t, &item)
	if item.ID == 0 {
		t.Fatalf("seeded movie has no id: %s", rr.Body.String())
	}
	return item.ID
}

// setting writes an app_meta value, for handlers gated on configuration.
func (e *apiEnv) setting(t *testing.T, k, v string) {
	t.Helper()
	if err := e.db.SetMeta(context.Background(), k, v); err != nil {
		t.Fatal(err)
	}
}
