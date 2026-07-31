// Command monarr wires everything together and runs the HTTP server and
// scheduler: config → logging → SQLite (migrate) → event bus → health
// registry → scheduler → API/UI. One process, one SQLite file, one port.
package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/pjunod/monarr/internal/adapters/deluge"
	"github.com/pjunod/monarr/internal/adapters/notify"
	"github.com/pjunod/monarr/internal/adapters/nzbd"
	"github.com/pjunod/monarr/internal/adapters/nzbget"
	"github.com/pjunod/monarr/internal/adapters/omdb"
	"github.com/pjunod/monarr/internal/adapters/openlibrary"
	"github.com/pjunod/monarr/internal/adapters/qbittorrent"
	"github.com/pjunod/monarr/internal/adapters/sabnzbd"
	"github.com/pjunod/monarr/internal/adapters/tmdb"
	"github.com/pjunod/monarr/internal/adapters/torznab"
	"github.com/pjunod/monarr/internal/adapters/trakt"
	"github.com/pjunod/monarr/internal/adapters/transmission"
	"github.com/pjunod/monarr/internal/adapters/tvmaze"
	"github.com/pjunod/monarr/internal/api"
	"github.com/pjunod/monarr/internal/app/acquisition"
	"github.com/pjunod/monarr/internal/app/discover"
	"github.com/pjunod/monarr/internal/app/health"
	"github.com/pjunod/monarr/internal/app/importlist"
	"github.com/pjunod/monarr/internal/app/library"
	appnotify "github.com/pjunod/monarr/internal/app/notify"
	"github.com/pjunod/monarr/internal/app/transfers"
	"github.com/pjunod/monarr/internal/buildinfo"
	"github.com/pjunod/monarr/internal/compat"
	"github.com/pjunod/monarr/internal/infra/bus"
	"github.com/pjunod/monarr/internal/infra/config"
	"github.com/pjunod/monarr/internal/infra/jobs"
	"github.com/pjunod/monarr/internal/infra/logging"
	"github.com/pjunod/monarr/internal/infra/scheduler"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	log := logging.New(os.Stderr, cfg)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg, log); err != nil {
		log.Error("monarr exited with error", "err", err)
		// A misconfigured data dir is an operator problem with a one-line
		// fix, and the fix is multi-line prose that slog would escape into
		// unreadable \n literals. Repeat it plainly on stderr.
		var dde *sqlite.DataDirError
		if errors.As(err, &dde) {
			fmt.Fprintf(os.Stderr, "\n%v\n\n", dde)
		}
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	startedAt := time.Now()
	log.Info("monarr starting",
		"version", buildinfo.Version, "commit", buildinfo.Commit,
		"addr", cfg.Addr(), "dataDir", cfg.DataDir, "uiEmbedded", api.UIBuilt())

	// Storage.
	db, err := sqlite.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := db.Close(); cerr != nil {
			log.Warn("closing database", "err", cerr)
		}
	}()
	if err := db.Migrate(ctx); err != nil {
		return err
	}
	schemaVersion, err := db.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	log.Info("database ready", "path", db.Path, "schemaVersion", schemaVersion)
	if err := db.SetMeta(ctx, "last_started_at", startedAt.UTC().Format(time.RFC3339)); err != nil {
		log.Warn("could not record startup in app_meta", "err", err)
	}

	// Event bus.
	b := bus.New(log)
	defer b.Close()

	// Metadata provider: TMDB, key read live from settings so it can be
	// set in the UI without a restart. MONARR_TMDB_BASE_URL is an
	// internal override for tests.
	tmdbKey := func(ctx context.Context) (string, error) {
		v, err := db.GetMeta(ctx, api.TMDBKeySetting)
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return v, err
	}
	meta := tmdb.New(os.Getenv("MONARR_TMDB_BASE_URL"), tmdbKey)
	// Books (ADR 0006): Open Library needs no key; the env override serves
	// tests, like the TMDB one.
	books := openlibrary.New(os.Getenv("MONARR_OPENLIBRARY_BASE_URL"))
	// Extra ratings (Rotten Tomatoes / IMDb / Metacritic) via OMDb —
	// optional, activated by setting the key in the UI.
	omdbKey := func(ctx context.Context) (string, error) {
		v, err := db.GetMeta(ctx, api.OMDBKeySetting)
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return v, err
	}
	extraRatings := omdb.New(os.Getenv("MONARR_OMDB_BASE_URL"), omdbKey)
	// The series chain (ADR 0011). TVmaze needs no key and no account, so it
	// is always on; it is consulted only for series TMDB cannot place, and
	// it supplies TheTVDB ids, which is what keys them for a TVDB adapter
	// later.
	seriesChain := tvmaze.New(os.Getenv("MONARR_TVMAZE_BASE_URL"))

	// Library service.
	lib := library.New(db, meta, b, log).
		WithBooks(books).
		WithRatings(extraRatings).
		WithSeriesProviders(seriesChain)

	// Discovery (ADR 0015): browse rows, read-only, nothing stored. TMDB is
	// always on because its key is already required; Trakt joins it when a
	// client id is set, and drops out of the catalogue when it is not.
	traktDiscover := trakt.New(os.Getenv("MONARR_TRAKT_BASE_URL"), func(ctx context.Context) (string, error) {
		v, err := db.GetMeta(ctx, api.TraktClientIDSetting)
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return v, err
	})
	// meta.Summary, not meta.GetSeries: hydrating artwork for a row of shows
	// through GetSeries would fetch every season of every one of them.
	browse := discover.New(log, meta.Summary, meta, traktDiscover)

	// Acquisition: real adapters injected as factories.
	indexerFactory := func(cfg ports.IndexerConfig) ports.Indexer { return torznab.New(cfg) }
	clientFactory := func(cfg ports.ClientConfig) ports.DownloadClient {
		switch cfg.Type {
		case "sabnzbd":
			return sabnzbd.New(cfg)
		case "nzbget":
			return nzbget.New(cfg)
		case "nzbd":
			return nzbd.New(cfg)
		case "transmission":
			return transmission.New(cfg)
		case "deluge":
			return deluge.New(cfg)
		default:
			return qbittorrent.New(cfg)
		}
	}
	acq := acquisition.New(db, b, log, indexerFactory, clientFactory)
	// The data plane gets its own registry and its own workers. Cancel ctx to
	// stop them; WaitImporters drains, in the same shape the jobs queue uses.
	inflight := transfers.New()
	acq.SetRegistry(inflight)
	acq.StartImporters(ctx)
	defer acq.WaitImporters()

	// Notifications (Phase 3): bus events fan out to configured targets.
	notifierFactory := func(cfg ports.NotifierConfig) ports.Notifier { return notify.New(cfg) }
	dispatcher := appnotify.New(db, b, log, notifierFactory).WithRegistry(inflight)
	go dispatcher.Run(ctx)
	// The delivery worker runs beside it: a retry scheduled two minutes out
	// has no bus event to wake it, and a dead server must not stall the
	// event loop while it times out.
	go dispatcher.RunDeliveries(ctx)

	// API key (Phase 4 compat auth; Phase 5 hardens the native API with it).
	apiKey, err := db.GetMeta(ctx, api.APIKeySetting)
	if errors.Is(err, sql.ErrNoRows) || apiKey == "" {
		buf := make([]byte, 16)
		if _, err := rand.Read(buf); err != nil {
			return fmt.Errorf("generating api key: %w", err)
		}
		apiKey = hex.EncodeToString(buf)
		if err := db.SetMeta(ctx, api.APIKeySetting, apiKey); err != nil {
			return fmt.Errorf("storing api key: %w", err)
		}
		log.Info("generated API key (Settings shows it; consumers use X-Api-Key)")
	} else if err != nil {
		return err
	}
	keyFn := func(ctx context.Context) string {
		v, err := db.GetMeta(ctx, api.APIKeySetting)
		if err != nil {
			return ""
		}
		return v
	}

	// Import lists (Phase 5): external lists auto-add into the library.
	lists := importlist.New(db, lib, importlist.Sources{
		TMDBDiscover: meta.DiscoverMovies,
		TraktList: func(ctx context.Context, clientID, user, slug string) ([]ports.SearchResult, error) {
			// NewStatic, not New: an import list carries its own client id in
			// its config and predates the settings-level one, so a list that
			// works today must keep working with the global field empty.
			return trakt.NewStatic(os.Getenv("MONARR_TRAKT_BASE_URL"), clientID).ListItems(ctx, user, slug)
		},
	}, log)

	// Compat personalities (ADR 0003): Sonarr/Radarr v3 translation surfaces.
	compatDeps := compat.Deps{
		Log: log, Library: lib, Store: db, APIKey: keyFn,
		ResolveTVDB: meta.FindSeriesByTVDB,
	}
	sonarrShim := compat.NewSonarr(compatDeps)
	radarrShim := compat.NewRadarr(compatDeps)

	// Health checks.
	reg := health.NewRegistry(b)
	reg.Register("database", func(ctx context.Context) health.Result {
		if err := db.Ping(ctx); err != nil {
			return health.Errorf("database unreachable: %v", err)
		}
		return health.OK()
	})
	reg.Register("data-directory", func(ctx context.Context) health.Result {
		// The same probe Open ran at startup, so a mount that goes
		// read-only or gets re-chowned underneath us is reported with the
		// same ownership detail and the same one-line fix.
		if err := sqlite.CheckDataDir(cfg.DataDir); err != nil {
			return health.Errorf("%v", err)
		}
		return health.OK()
	})
	reg.Register("web-ui", func(ctx context.Context) health.Result {
		if !api.UIBuilt() {
			return health.Warn("binary built without web UI (run `make build`); serving fallback page")
		}
		return health.OK()
	})
	reg.Register("library-folders", func(ctx context.Context) health.Result {
		// Two items pointing at one folder: whichever scan runs last decides
		// which of them the files attach to, and the other is a card that
		// looks real and holds nothing. Adoption cannot create this any more;
		// this reports the libraries that already have it.
		shared, err := lib.SharedFolders(ctx)
		if err != nil {
			return health.Errorf("cannot read the library: %v", err)
		}
		if len(shared) == 0 {
			return health.OK()
		}
		// Named example, chosen in sorted order so the same library reports
		// the same folder every time rather than a different one per poll.
		paths := make([]string, 0, len(shared))
		for path := range shared {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		first := paths[0]
		return health.Warn(
			"%d folder(s) are claimed by more than one item — e.g. %s is held by %s. "+
				"Remove the duplicate from the library (files on disk are untouched)",
			len(shared), first, strings.Join(shared[first], " and "))
	})
	// The seams: the other applications in the pipeline, probed through the
	// same adapters that do the real work — so a check passing means the
	// thing that matters would work, not merely that a URL resolves.
	connections := health.NewConnectionMonitor(health.ConnectionDeps{
		Clients:     db.ListDownloadClients,
		Notifiers:   db.ListNotifiers,
		NewClient:   clientFactory,
		NewNotifier: notifierFactory,
		Contacts:    acq.Contacts,
	})
	reg.RegisterGroup(connections.Check)
	// The data plane, checked separately from the control plane. This asks
	// "is work moving", never "can we reach anyone" — the two used to be the
	// same question, and a 20 GB import answered it as a degraded download
	// client. An import that has been running for hours is a real problem
	// and a different one from a client that stopped answering; each now has
	// its own line, and neither can mask the other.
	reg.Register("imports", func(ctx context.Context) health.Result {
		age, running := acq.StalledImports()
		switch {
		case !running:
			return health.Result{Status: health.StatusOK}
		case age > 6*time.Hour:
			return health.Result{Status: health.StatusWarning, Message: fmt.Sprintf(
				"an import has been running for %s — that is long enough to be stuck rather than slow",
				age.Truncate(time.Minute))}
		default:
			return health.Result{Status: health.StatusOK, Message: fmt.Sprintf(
				"importing for %s", age.Truncate(time.Second))}
		}
	})
	reg.Register("metadata-provider", func(ctx context.Context) health.Result {
		key, err := tmdbKey(ctx)
		if err != nil {
			return health.Errorf("cannot read settings: %v", err)
		}
		if key == "" {
			return health.Warn("TMDB API key not set — add it under Settings to search and add media")
		}
		return health.OK()
	})

	// Scheduler.
	sched := scheduler.New(sqlite.NewTaskStore(db), b, log)
	if err := sched.Register(scheduler.Task{
		Name:       "health.check",
		Interval:   time.Minute,
		RunOnStart: true,
		Fn: func(ctx context.Context) error {
			reg.Run(ctx)
			return nil
		},
	}); err != nil {
		return err
	}
	if err := sched.Register(scheduler.Task{
		Name:     "db.wal-checkpoint",
		Interval: time.Hour,
		Fn:       db.Checkpoint,
	}); err != nil {
		return err
	}
	if err := sched.Register(scheduler.Task{
		Name:     api.ScanTaskName,
		Interval: 12 * time.Hour,
		Fn: func(ctx context.Context) error {
			// The scheduler stops running periodic work and starts
			// enqueuing it (ADR 0008 §1). The dedupe key means a slow
			// scan never stacks up behind itself.
			return lib.EnqueueScan(ctx)
		},
	}); err != nil {
		return err
	}
	if err := sched.Register(scheduler.Task{
		Name:     "queue.refresh",
		Interval: 30 * time.Second,
		Fn:       acq.RefreshQueue,
	}); err != nil {
		return err
	}
	// Push subscriptions run beside the poll, never instead of it: a
	// client in push mode is still swept every 30 s above, which is what
	// notices a stream that died quietly. The supervisor re-reads the
	// client list itself, so switching a client to push in the settings
	// UI takes effect without a restart.
	go acq.RunSubscribers(ctx)
	// Phase 3 automation: the RSS loop grabs wanted releases as they appear;
	// backlog search actively hunts for what RSS already scrolled past.
	if err := sched.Register(scheduler.Task{
		Name:     "rss.sync",
		Interval: 15 * time.Minute,
		Fn:       acq.SyncRSS,
	}); err != nil {
		return err
	}
	if err := sched.Register(scheduler.Task{
		Name:     "backlog.search",
		Interval: 12 * time.Hour,
		Fn:       acq.BacklogSearch,
	}); err != nil {
		return err
	}
	// Cleanup: the sweep that collects payloads the inline path did not.
	// It is what drains a backlog when somebody turns the setting on after a
	// year of grabs — enabling it means "monarr should not be leaving these
	// around", not "monarr should stop leaving NEW ones around".
	if err := sched.Register(scheduler.Task{
		Name:     acquisition.JobCleanup,
		Interval: acquisition.CleanupInterval,
		Fn:       acq.CleanupPayloads,
	}); err != nil {
		return err
	}
	// Import retry: an import that stopped because the destination was full
	// finishes itself once there is room. Without this the payload sits in
	// `failed` while the item shows as missing, and automation grabs the
	// same release again — the loop, with monarr's name on it.
	if err := sched.Register(scheduler.Task{
		Name:     acquisition.JobImportRetry,
		Interval: acquisition.ImportRetryInterval,
		Fn:       acq.RetryBlockedImports,
	}); err != nil {
		return err
	}
	if err := sched.Register(scheduler.Task{
		Name:     "importlists.sync",
		Interval: 12 * time.Hour,
		Fn:       lists.Sync,
	}); err != nil {
		return err
	}
	// Metadata refresh: continuing series learn about newly announced
	// episodes, and cached fields (status, poster, ratings) stay current.
	if err := sched.Register(scheduler.Task{
		Name:     "metadata.refresh",
		Interval: 12 * time.Hour,
		Fn: func(ctx context.Context) error {
			err := lib.RefreshAll(ctx)
			acq.InvalidateWanted()
			return err
		},
	}); err != nil {
		return err
	}
	if err := sched.Register(scheduler.Task{
		Name:     "backup.run",
		Interval: 24 * time.Hour,
		Fn: func(ctx context.Context) error {
			path, err := db.Backup(ctx)
			if err == nil {
				log.Info("backup written", "path", path)
			}
			return err
		},
	}); err != nil {
		return err
	}
	if err := sched.Start(ctx); err != nil {
		return err
	}

	// Job queue (ADR 0008 step 1). Started after the scheduler so periodic
	// work can enqueue jobs, and worth running on a single instance for the
	// retries and failure visibility the timer scheduler cannot give.
	queue := jobs.New(db, b, log, jobs.Options{})
	if err := library.RegisterJobHandlers(lib, queue.Register); err != nil {
		return err
	}
	lib.WithQueue(queue)
	queue.Start(ctx)
	defer queue.Wait()

	// Library changes invalidate the wanted index (imports do it in-service).
	go func() {
		added, cancelAdded := bus.Subscribe[library.MediaAdded](b, 16)
		defer cancelAdded()
		scans, cancelScans := bus.Subscribe[library.ScanCompleted](b, 16)
		defer cancelScans()
		// A probe can flip an item from "missing" to "already have it"
		// (ADR 0013), so a measurement landing invalidates the index just as
		// a scan does. The buffer is larger because the post-upgrade backfill
		// publishes one of these per file in the library.
		probed, cancelProbed := bus.Subscribe[library.FileProbed](b, 256)
		defer cancelProbed()
		for {
			select {
			case <-ctx.Done():
				return
			case <-added:
				acq.InvalidateWanted()
			case <-scans:
				acq.InvalidateWanted()
			case <-probed:
				acq.InvalidateWanted()
			}
		}
	}()

	// HTTP.
	srv := api.New(api.Deps{
		Log:             log,
		Bus:             b,
		Health:          reg,
		Connections:     connections,
		Callers:         api.NewCallerRegistry(),
		Scheduler:       sched,
		DB:              db,
		Library:         lib,
		Acquisition:     acq,
		Discover:        browse,
		Store:           db,
		IndexerFactory:  indexerFactory,
		ClientFactory:   clientFactory,
		NotifierFactory: notifierFactory,
		CompatSonarr:    sonarrShim.Handler(),
		CompatRadarr:    radarrShim.Handler(),
		Settings:        db,
		Version:         buildinfo.Version,
		Commit:          buildinfo.Commit,
		DataDir:         cfg.DataDir,
		StartedAt:       startedAt,
	})
	httpServer := &http.Server{
		Addr:              cfg.Addr(),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("http listening", "addr", cfg.Addr())
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-errCh:
		return err
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutCtx); err != nil {
		log.Warn("http shutdown", "err", err)
	}
	sched.Wait()
	log.Info("goodbye")
	return nil
}
