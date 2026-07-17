# Monarr

**A unified, modern rewrite of Sonarr + Radarr in Go.** One binary, one database, one UI, one
acquisition pipeline — for both TV and movies.

> **Status: Phase 0 — walking skeleton.** The scaffold runs, serves the embedded React shell,
> reports system status and health, persists scheduler state in SQLite, and streams live events
> over SSE. It does not manage any media yet. See the [roadmap](#roadmap).

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
docker run -d --name monarr -p 7676:7676 -v monarr-data:/data monarr
```

Open http://localhost:7676.

### From source

Requires Go ≥ 1.25 and Node ≥ 20.

```sh
make build          # builds web UI, embeds it, compiles ./bin/monarr
./bin/monarr
```

### Development

```sh
make dev-api        # terminal 1: go run (API on :7676, serves fallback page)
make dev-web        # terminal 2: vite dev server on :5173, proxies /api → :7676
make test           # go tests (unit + architecture rules)
make gen            # regenerate sqlc + oapi-codegen output after editing SQL/spec
```

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
| 3 — Automation | wanted index, RSS loop, failed-download handling, calendar, notifiers | runs unattended for a month |
| 4 — Ecosystem | Sonarr/Radarr v3 compat shim, conformance vs real tools | Jellyseerr/Prowlarr/Bazarr don't notice the swap |
| 5 — Depth | custom formats, more clients, import lists, anime numbering | parity tail |

## License

[GPL-3.0](LICENSE), matching upstream Sonarr and Radarr — which keeps the door open to porting
their release-parser test corpora as golden tests.
