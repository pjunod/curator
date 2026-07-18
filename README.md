# Monarr

**A unified, modern rewrite of Sonarr + Radarr in Go.** One binary, one database, one UI, one
acquisition pipeline — for TV, movies, and (Phase 2.5) books, filling the gap left by
Readarr's retirement.

> **Status: Phase 0 — walking skeleton — complete.** The scaffold runs, serves the embedded
> React shell, reports system status and health, persists scheduler state in SQLite, and
> streams live events over SSE. It does not manage any media yet. **Current state, phase
> ledger, and next steps live in [STATUS.md](STATUS.md)** (updated every working session);
> the long-range plan is in the [roadmap](#roadmap).

## Why

Sonarr and Radarr are, to a first approximation, the same program: an indexer layer, a
release-name parser, a quality decision engine, download client integrations, an import/rename
pipeline, notifications, and a React UI. Monarr rebuilds that shared machinery once, as a Go
modular monolith with hexagonal boundaries, and models the TV/movie difference where it actually
lives: in the domain model, not at the application boundary.

The full design is in [docs/architecture.md](docs/architecture.md); decisions are recorded as
ADRs in [docs/adr/](docs/adr/).

## Quick start

### Docker

```sh
docker build -t monarr .
docker run -d --name monarr \
  -p 7676:7676 \
  --user 1000:1000 \
  -v /srv/monarr:/data \
  -v /srv/pool:/pool \
  monarr
```

Open http://localhost:7676. Then in the UI: **Settings → add your TMDB API key**,
add root folders (e.g. `/pool/media/movies`), and configure your indexer and
download client.

**The mounts:**

| Mount | Purpose |
|---|---|
| `/data` | SQLite database, daily backups, runtime state — back this one up |
| one shared pool (e.g. `/srv/pool:/pool`) | your media library **and** your download client's completed folder, under a single mount |

A working pool layout:

```
/srv/pool/
├── media/
│   ├── movies/        ← root folder in Monarr
│   ├── tv/            ← root folder in Monarr
│   └── books/         ← root folder in Monarr
└── downloads/         ← completed folder in qBittorrent/SABnzbd/…
```

**The one rule that makes imports work:** Monarr opens completed downloads at the
literal path the download client reports. If qBittorrent says a torrent lives at
`/pool/downloads/X`, Monarr must be able to open `/pool/downloads/X` *inside its
own container* — so give the download client's container the **same** `-v
/srv/pool:/pool` mapping. There is no remote-path-mapping translation layer
(yet); identical mounts across containers is the requirement.

The single shared pool mount is also what enables **hardlink imports**: downloads
and media on one filesystem means importing a release is instant and costs no
extra space (seeding continues from the original file). With separate mounts,
Monarr silently falls back to copying — correct, just slower and double-space
during seeding.

**Permissions:** the image is distroless and runs as whatever `--user uid:gid`
you pass (root if omitted). Run as the same user that owns your pool so imported
files don't end up root-owned. Make sure `/srv/monarr` and `/srv/pool` are
writable by that uid.

Compose equivalent, side by side with a download client:

```yaml
services:
  monarr:
    image: monarr            # or ghcr.io/monarr-media/monarr once published
    user: "1000:1000"
    ports: ["7676:7676"]
    volumes:
      - /srv/monarr:/data
      - /srv/pool:/pool
    restart: unless-stopped

  qbittorrent:
    image: lscr.io/linuxserver/qbittorrent:latest
    environment: [PUID=1000, PGID=1000, WEBUI_PORT=8080]
    ports: ["8080:8080"]
    volumes:
      - /srv/qbittorrent:/config
      - /srv/pool:/pool      # same pool, same path — this is the important part
    restart: unless-stopped
```

In qBittorrent set the default save path to `/pool/downloads`; in Monarr add the
client as `http://qbittorrent:8080` (same compose network) or `http://<host>:8080`.

### From source

Requires Go ≥ 1.25 and Node ≥ 20.

```sh
make build          # builds web UI, embeds it, compiles ./bin/monarr
./bin/monarr
```

### Development

```sh
make hooks          # once per clone: installs the git pre-commit/pre-push hooks
make dev-api        # terminal 1: go run (API on :7676, serves fallback page)
make dev-web        # terminal 2: vite dev server on :5173, proxies /api → :7676
make gen            # regenerate sqlc + oapi-codegen output after editing SQL/spec
```

## Testing

The suite is a pyramid; every layer runs in CI and the fast layers run in git hooks:

```sh
make test           # Go unit tests, incl. the architecture-rules test (internal/arch_test.go)
make test-web       # web unit tests (vitest)
make lint           # golangci-lint (pinned version, same as CI)
make test-e2e       # end-to-end: builds the real binary with embedded UI, boots it
                    # against a throwaway data dir, drives it with Playwright
                    # (first run: npx playwright install chromium in test/e2e)
```

Git hooks (installed by `make hooks`, versioned in `.githooks/`): **pre-commit** runs gofmt +
`go vet` on staged Go files and typechecks staged web sources; **pre-push** runs the full Go
and web unit suites. E2E stays in CI to keep pushes fast.

CI (`.github/workflows/ci.yml`) runs four parallel jobs on every push/PR: unit tests
(Go with `-race` and a coverage summary, web with vitest, plus a check that sqlc/oapi-codegen
output is up to date), golangci-lint, the Playwright E2E suite against the compiled binary,
and a Docker image build. Least-privilege permissions, per-ref concurrency cancellation.

## Documentation

- **[Usage guide](docs/usage.md)** — first run, adding movies/series/books, adopting an
  existing library, interactive search, automation, connecting Jellyseerr/Prowlarr/Bazarr.
- **[Settings reference](docs/settings.md)** — every field in the Settings and System pages.
- **[Deployment](docs/deployment.md)** — storage rules for the SQLite database (why the live
  DB must not sit on NFS/Gluster), floating-node/HA patterns, backup/restore.
- **[Architecture](docs/architecture.md)** and **[ADRs](docs/adr/)** — the design and why.

## Configuration

Environment first, optional JSON file second (`MONARR_CONFIG=/path/to/config.json`),
built-in defaults last. Everything else (indexers, clients, profiles…) will live in the
database and be managed in the UI, per the *arr operational model.

| Env | Default | Description |
|---|---|---|
| `MONARR_HOST` | *(all interfaces)* | Bind address |
| `MONARR_PORT` | `7676` | HTTP port (native API + compat + UI share it) |
| `MONARR_DATA_DIR` | `./data` | SQLite database, backups, runtime state |
| `MONARR_LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error` |
| `MONARR_LOG_FORMAT` | `text` | `text` \| `json` |
| `MONARR_CONFIG` | *(unset)* | Optional JSON config file with the same keys |

## API

Native API is spec-first OpenAPI 3 at [internal/api/openapi.yaml](internal/api/openapi.yaml),
served under `/api/v1`. Phase 0 surface: `GET /api/v1/system/status`, `GET /api/v1/health`,
`GET /api/v1/system/tasks`, `POST /api/v1/system/tasks/{name}/run`, and `GET /api/v1/events`
(SSE). The Sonarr/Radarr v3 compat personalities (`/sonarr/api/v3`, `/radarr/api/v3`) arrive in
Phase 4.

## Repository layout

```
cmd/monarr/          wire everything, start HTTP + scheduler
internal/domain/     PURE functional core (entities, parser, decision, naming — later phases)
internal/ports/      driven-port interfaces (Indexer, DownloadClient, MetadataProvider, …)
internal/adapters/   implementations of ports (torznab, qbittorrent, tmdb, … — later phases)
internal/app/        application services (health today; library, acquisition, … later)
internal/infra/      sqlite (sqlc + goose), event bus, scheduler, config, logging
internal/api/        native /api/v1 (OpenAPI-first) + SSE + embedded UI serving
internal/compat/     Sonarr/Radarr v3 personalities (Phase 4)
web/                 React + TypeScript + Vite app, embedded via go:embed
testdata/releases/   golden parser corpus (Phase 2)
test/conformance/    docker-compose harness vs real Jellyseerr/Prowlarr/Bazarr (Phase 4)
docs/                architecture blueprint + ADRs
```

Dependency arrows are enforced by `internal/arch_test.go`: domain imports nothing but stdlib,
adapters import only ports+domain, compat never touches infra/adapters directly.

## Roadmap

| Phase | Scope | Success looks like |
|---|---|---|
| **0 — Walking skeleton** *(this)* | repo+CI, embedded UI, status API, SQLite+sqlc+goose, config, slog, bus, scheduler, health | `docker run` → UI loads, status reports, tests pass |
| 1 — Library | TMDB, add/browse movies & series, root folders, disk reconcile | real folders imported and browsable |
| 2 — Acquisition core | parser+golden corpus, matcher, decisions, Torznab, qBit+SABnzbd, import+rename | search → grab → correctly named file |
| 2.5 — Books ([ADR 0006](docs/adr/0006-books-third-media-kind.md)) | `book` kind end-to-end: ebooks + audiobooks, book metadata adapter, book parser rules, format quality ladder | grab an ebook and an audiobook, correctly named |
| 3 — Automation | wanted index, RSS loop, failed-download handling, calendar, notifiers | runs unattended for a month |
| 4 — Ecosystem | Sonarr/Radarr v3 compat shim, conformance vs real tools | Jellyseerr/Prowlarr/Bazarr don't notice the swap |
| 5 — Depth | custom formats, more clients, import lists, anime numbering | parity tail |

## License

[GPL-3.0](LICENSE), matching upstream Sonarr and Radarr — which keeps the door open to porting
their release-parser test corpora as golden tests.
