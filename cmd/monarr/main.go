// Command monarr wires everything together and runs the HTTP server and
// scheduler: config → logging → SQLite (migrate) → event bus → health
// registry → scheduler → API/UI. One process, one SQLite file, one port.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/monarr-media/monarr/internal/adapters/qbittorrent"
	"github.com/monarr-media/monarr/internal/adapters/sabnzbd"
	"github.com/monarr-media/monarr/internal/adapters/tmdb"
	"github.com/monarr-media/monarr/internal/adapters/torznab"
	"github.com/monarr-media/monarr/internal/api"
	"github.com/monarr-media/monarr/internal/app/acquisition"
	"github.com/monarr-media/monarr/internal/app/health"
	"github.com/monarr-media/monarr/internal/app/library"
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

	// Library service.
	lib := library.New(db, meta, b, log)

	// Acquisition: real adapters injected as factories.
	indexerFactory := func(cfg ports.IndexerConfig) ports.Indexer { return torznab.New(cfg) }
	clientFactory := func(cfg ports.ClientConfig) ports.DownloadClient {
		if cfg.Type == "sabnzbd" {
			return sabnzbd.New(cfg)
		}
		return qbittorrent.New(cfg)
	}
	acq := acquisition.New(db, b, log, indexerFactory, clientFactory)

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
	if err := sched.Start(ctx); err != nil {
		return err
	}

	// HTTP.
	srv := api.New(api.Deps{
		Log:            log,
		Bus:            b,
		Health:         reg,
		Scheduler:      sched,
		DB:             db,
		Library:        lib,
		Acquisition:    acq,
		Store:          db,
		IndexerFactory: indexerFactory,
		ClientFactory:  clientFactory,
		Settings:       db,
		Version:        version,
		Commit:         commit,
		DataDir:        cfg.DataDir,
		StartedAt:      startedAt,
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
