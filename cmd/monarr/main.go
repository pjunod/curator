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
	"path/filepath"
	"syscall"
	"time"

	"github.com/monarr-media/monarr/internal/adapters/deluge"
	"github.com/monarr-media/monarr/internal/adapters/notify"
	"github.com/monarr-media/monarr/internal/adapters/nzbget"
	"github.com/monarr-media/monarr/internal/adapters/openlibrary"
	"github.com/monarr-media/monarr/internal/adapters/qbittorrent"
	"github.com/monarr-media/monarr/internal/adapters/sabnzbd"
	"github.com/monarr-media/monarr/internal/adapters/tmdb"
	"github.com/monarr-media/monarr/internal/adapters/torznab"
	"github.com/monarr-media/monarr/internal/adapters/trakt"
	"github.com/monarr-media/monarr/internal/adapters/transmission"
	"github.com/monarr-media/monarr/internal/api"
	"github.com/monarr-media/monarr/internal/app/acquisition"
	"github.com/monarr-media/monarr/internal/app/health"
	"github.com/monarr-media/monarr/internal/app/importlist"
	"github.com/monarr-media/monarr/internal/app/library"
	appnotify "github.com/monarr-media/monarr/internal/app/notify"
	"github.com/monarr-media/monarr/internal/compat"
	"github.com/monarr-media/monarr/internal/infra/bus"
	"github.com/monarr-media/monarr/internal/infra/config"
	"github.com/monarr-media/monarr/internal/infra/logging"
	"github.com/monarr-media/monarr/internal/infra/scheduler"
	"github.com/monarr-media/monarr/internal/infra/sqlite"
	"github.com/monarr-media/monarr/internal/ports"
)

// Injected via -ldflags at build time (see Makefile / Dockerfile).
var (
	version = "dev"
	commit  = "none"
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
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	startedAt := time.Now()
	log.Info("monarr starting",
		"version", version, "commit", commit,
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

	// Library service.
	lib := library.New(db, meta, b, log).WithBooks(books)

	// Acquisition: real adapters injected as factories.
	indexerFactory := func(cfg ports.IndexerConfig) ports.Indexer { return torznab.New(cfg) }
	clientFactory := func(cfg ports.ClientConfig) ports.DownloadClient {
		switch cfg.Type {
		case "sabnzbd":
			return sabnzbd.New(cfg)
		case "nzbget":
			return nzbget.New(cfg)
		case "transmission":
			return transmission.New(cfg)
		case "deluge":
			return deluge.New(cfg)
		default:
			return qbittorrent.New(cfg)
		}
	}
	acq := acquisition.New(db, b, log, indexerFactory, clientFactory)

	// Notifications (Phase 3): bus events fan out to configured targets.
	notifierFactory := func(cfg ports.NotifierConfig) ports.Notifier { return notify.New(cfg) }
	dispatcher := appnotify.New(db, b, log, notifierFactory)
	go dispatcher.Run(ctx)

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
			return trakt.New(os.Getenv("MONARR_TRAKT_BASE_URL"), clientID).ListItems(ctx, user, slug)
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
		probe := filepath.Join(cfg.DataDir, ".monarr-write-probe")
		if err := os.WriteFile(probe, []byte("ok"), 0o644); err != nil {
			return health.Errorf("data dir not writable: %v", err)
		}
		if err := os.Remove(probe); err != nil {
			return health.Warn("could not remove write probe: %v", err)
		}
		return health.OK()
	})
	reg.Register("web-ui", func(ctx context.Context) health.Result {
		if !api.UIBuilt() {
			return health.Warn("binary built without web UI (run `make build`); serving fallback page")
		}
		return health.OK()
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
			_, err := lib.Scan(ctx)
			return err
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

	// Library changes invalidate the wanted index (imports do it in-service).
	go func() {
		added, cancelAdded := bus.Subscribe[library.MediaAdded](b, 16)
		defer cancelAdded()
		scans, cancelScans := bus.Subscribe[library.ScanCompleted](b, 16)
		defer cancelScans()
		for {
			select {
			case <-ctx.Done():
				return
			case <-added:
				acq.InvalidateWanted()
			case <-scans:
				acq.InvalidateWanted()
			}
		}
	}()

	// HTTP.
	srv := api.New(api.Deps{
		Log:             log,
		Bus:             b,
		Health:          reg,
		Scheduler:       sched,
		DB:              db,
		Library:         lib,
		Acquisition:     acq,
		Store:           db,
		IndexerFactory:  indexerFactory,
		ClientFactory:   clientFactory,
		NotifierFactory: notifierFactory,
		CompatSonarr:    sonarrShim.Handler(),
		CompatRadarr:    radarrShim.Handler(),
		Settings:        db,
		Version:         version,
		Commit:          commit,
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
