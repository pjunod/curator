# Monarr

[![Tests](https://github.com/pjunod/monarr/actions/workflows/tests.yml/badge.svg)](https://github.com/pjunod/monarr/actions/workflows/tests.yml)
[![Lint](https://github.com/pjunod/monarr/actions/workflows/lint.yml/badge.svg)](https://github.com/pjunod/monarr/actions/workflows/lint.yml)
[![Compat](https://github.com/pjunod/monarr/actions/workflows/compat.yml/badge.svg)](https://github.com/pjunod/monarr/actions/workflows/compat.yml)
[![Docker](https://github.com/pjunod/monarr/actions/workflows/docker.yml/badge.svg)](https://github.com/pjunod/monarr/actions/workflows/docker.yml)
[![Coverage](coverage.svg)](https://github.com/pjunod/monarr/actions/workflows/tests.yml)

**A unified, modern rewrite of Sonarr + Radarr in Go.** One binary, one database, one UI, one
acquisition pipeline — for TV, movies, ebooks, and audiobooks, filling the gap left by
Readarr's retirement. Audiobooks get their own defaults, indexer routing, library views,
and multipart-track import while remaining books in the shared metadata model.

> **Status: all planned phases complete** — library, acquisition, books, automation
> (RSS/backlog/blocklist), Sonarr/Radarr compat personalities, and the depth tail (custom
> formats, client zoo, import lists, anime numbering, auth, metrics). Now in real-world
> validation, which is where **Discover** came from (browse what's trending and add it
> without leaving the app — [ADR 0015](docs/adr/0015-discovery.md)). **The per-item ledger
> lives in [STATUS.md](STATUS.md)**; the original plan is in the [roadmap](#roadmap).

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

Deployment files live in [`deploy/`](deploy/) — the Dockerfile and a
compose *template* you copy once and customize (your copy is gitignored,
so pulls never clobber it):

```sh
cd deploy
cp docker-compose.example.yml docker-compose.yml   # yours to edit
cp .env.example .env                                # optional: edit paths/user
docker compose up -d --build
```

Rebuild-and-swap after pulling changes is the same `up` command. Plain
`docker run` equivalent (from the repo root — the build context is the
repo, the Dockerfile just lives in deploy/):

```sh
docker build -f deploy/Dockerfile -t monarr .
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
    image: monarr            # or ghcr.io/pjunod/monarr once published
    user: "1000:1000"
    ports: ["7676:7676"]
    volumes:
      - /srv/monarr:/data
      - /srv/pool:/pool
    restart: unless-stopped

  monarr-discovery:
    image: monarr
    network_mode: host       # DNS-SD multicast must reach the physical LAN
    command: ["advertise", "--name", "monarr", "--port", "7676"]
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
make dev-mobile     # Expo dev server for the native iOS/Android app
make gen            # regenerate sqlc + oapi-codegen output after editing SQL/spec
```

The native [iOS and Android app](mobile/README.md) connects to an existing
Monarr server. It owns the daily phone workflows — Library, Discover, Wanted,
Activity, Calendar, and health — while full administration stays in the web UI.

## Testing

The suite is a pyramid; every layer runs in CI and the fast layers run in git hooks:

```sh
make test           # Go unit tests, incl. the architecture-rules test (internal/arch_test.go)
make test-web       # web unit tests (vitest)
make test-mobile    # mobile strict TypeScript + Vitest
make mobile-export  # Expo Doctor + production bundles for iOS and Android
make lint           # golangci-lint (pinned version, same as CI)
make coverage       # Go coverage: prints the percentage, writes coverage.svg
make coverage-html  # the same run, as a browsable line-by-line report
make test-e2e       # end-to-end: builds the real binary with embedded UI, boots it
                    # against a throwaway data dir, drives it with Playwright
                    # (first run: npx playwright install chromium in test/e2e)
```

Git hooks (installed by `make hooks`, versioned in `.githooks/`): **pre-commit** runs gofmt +
`go vet` on staged Go files and typechecks staged web sources; **pre-push** runs the full Go
and web unit suites. E2E stays in CI to keep pushes fast.

CI runs four workflows on every push/PR — `tests.yml` (Go unit tests with `-race` and
coverage, web vitest, mobile tests plus both native bundles, a check that
sqlc/oapi-codegen output is current, and the Playwright E2E suite against the
compiled binary), `lint.yml`, `compat.yml` (Sonarr/Radarr conformance), and
`docker.yml`. Least-privilege permissions, per-ref concurrency cancellation.

### Coverage

**The target is 85%, and the floor is enforced.** [`COVERAGE_MIN`](COVERAGE_MIN) holds the
number the build refuses to go below; [`scripts/coverage-gate.sh`](scripts/coverage-gate.sh)
checks it in `make coverage` and in CI, and nags when the floor has drifted far enough below
the real number to be worth raising. The floor is a **ratchet, not the goal**: setting it
straight to 85% would paint the build red for a gap no single commit created, and a build
that is always red is a build everybody learns to ignore. This way a change that *lowers*
coverage fails immediately — while the person who lowered it is still holding it, which is
the only moment the fix is cheap — and the target stays a real number rather than an
aspiration nothing enforces. Raise `COVERAGE_MIN` in the commit that earns it.

The number and the badge are produced by this repository, not by a coverage service.
`make coverage` writes [`coverage.svg`](coverage.svg), and **whoever changes coverage commits
it in the same commit**. CI does not push it: it did for exactly one run, and the bot commit
put every clone one behind `main`, so the next `git push` failed with a non-fast-forward over
a picture. This repository is synced between machines by git bundle, and a bundle is built
against a known base — a tip that moves on its own breaks every handoff, not just the next
push. CI now only warns when the committed badge disagrees with what it measured. The floor
above is what protects the number on every push; the badge is a readout.

Two rules decide what the number counts, both in
[`scripts/coverage-badge.sh`](scripts/coverage-badge.sh) so nothing can report a different
figure:

- **`-coverpkg=./...`.** Without it a package appears in the profile only if it owns a
  `_test.go` file, so what gets measured depends on where the tests happen to live rather
  than on what they exercise. Ten of forty packages were invisible: `cmd/monarr` and
  `internal/infra/logging` at 0%, but also `internal/domain/decision` at 94% and
  `internal/domain/quality` at 93%, both covered thoroughly through their callers. The
  subset can flatter or understate; the point is that it is arbitrary.
- **Generated code excluded.** sqlc and oapi-codegen contribute tens of thousands of
  statements that no one will write a test for; counting them measures how much machine
  output the repo holds, which is not a fact about the tests. On 2026-07-29 it was the
  difference between 63.8% and 68.4%, and only the second number moves when someone writes
  a test.

The CI step summary lists every package least-covered first, which is where the number
becomes useful — on 2026-07-29 that read `cmd/monarr` at zero (the composition root, wiring
only) and `internal/app/acquisition` at 72%, with every domain package above 85%.

**Why the badge is a tracked file rather than a URL.** It used to live on an orphan `badges`
branch and be linked as `raw.githubusercontent.com/…`, which keeps the badge out of main's
history — but GitHub fetches an absolute image URL through its proxy, anonymously, and this
repository is private, so that image 404s for everyone. The four workflow badges above are
served by `github.com` itself and honour the viewer's session, which is why they render and
that one did not. A relative path is served the same way, so it works private *and* public;
the cost is one `coverage: NN.N%` commit from CI whenever the number actually changes.

## Documentation

- **[Usage guide](docs/usage.md)** — first run, adding movies/series/books, browsing
  Discover, adopting an existing library, interactive search, automation, connecting
  Jellyseerr/Prowlarr/Bazarr.
- **[Mobile app](mobile/README.md)** — run, verify, build, and submit the native iOS and
  Android companion.
- **[Settings reference](docs/settings.md)** — every field in the Settings and System pages.
- **[Integration](docs/integration.md)** — every seam with nzbd and plurx: what each does, the
  wire format, where you watch it in the UI, and the command that proves it works.
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

The shape is hexagonal (blueprint §3) and the arrows are *enforced* — an
architecture test (`internal/arch_test.go`) fails the build if a package
imports against the grain:

```
domain ← ports ← app ← { api, compat }        adapters → ports
                  ↑                           infra    → domain
            (infra wires under app)           cmd      → everything
```

```
cmd/monarr/           composition root: wiring, scheduler tasks, HTTP startup
internal/
  domain/             PURE core, stdlib-only — media model, Wantable
    quality/            sources×resolutions + book formats, profiles, ranking
    parser/             release-name parser (golden corpus + fuzz)
    matcher/            release ↔ wantable matching (incl. books, anime absolute)
    decision/           accept/reject/upgrade with machine-readable reasons
    format/             custom-format regex scoring
    naming/  filename/  on-disk name rendering · scan-side name reading
  ports/              interfaces the core needs from the world (+ shared URL normalizer)
  adapters/           one package per external service, ports implementations only:
                        tmdb, openlibrary, torznab, qbittorrent, transmission,
                        deluge, sabnzbd, nzbget, trakt, notify (webhook/discord/plex/jellyfin/plurx)
  app/                use cases over domain+ports:
                        library (add/scan/reconcile), acquisition (search/grab/
                        queue/import + RSS/backlog/wanted), discover (browse rows),
                        importlist, notify, health
  infra/              technical substrate: sqlite (sqlc+goose), bus, scheduler,
                        config, logging
  api/                native /api/v1 (OpenAPI-first), SSE, auth, /metrics, SPA serving
  compat/             Sonarr/Radarr v3 personalities (translation-only over app)
web/                  React + TypeScript + Vite UI, embedded via go:embed
mobile/               Expo + React Native app for iOS and Android
deploy/               Dockerfile + compose template (copy to docker-compose.yml, yours to edit)
test/e2e/             Playwright suite booting the real binary against fake services
test/conformance/     docker-compose harness vs real Jellyseerr/Prowlarr/Bazarr
testdata/releases/    golden parser corpus (shared spec, 70 cases)
docs/                 architecture blueprint, ADRs, usage/settings/deployment guides
scripts/              version derivation, shared by the Makefile and Docker build
.githooks/ .github/   versioned git hooks · CI (unit+lint+e2e+docker)
STATUS.md             the per-item work ledger
```

Root files stay at the root because tooling discovers them there:
`go.mod` (module root), `Makefile`, `VERSION` (the version string the build
stamps in, verbatim — bump it to cut a release), `.golangci.yml`, and
`.dockerignore` (read at the build *context* root — the Dockerfile itself
lives in deploy/ and builds with the repo as context).

Dependency arrows are enforced by `internal/arch_test.go`: domain imports nothing but stdlib,
adapters import only ports+domain, compat never touches infra/adapters directly.

## Roadmap

| Phase | Scope | Success looks like |
|---|---|---|
| **0 — Walking skeleton** *(this)* | repo+CI, embedded UI, status API, SQLite+sqlc+goose, config, slog, bus, scheduler, health | `docker run` → UI loads, status reports, tests pass |
| 1 — Library | TMDB, add/browse movies & series, root folders, disk reconcile | real folders imported and browsable |
| 2 — Acquisition core | parser+golden corpus, matcher, decisions, Torznab, qBit+SABnzbd, import+rename | search → grab → correctly named file |
| 2.5 — Books ([ADR 0006](docs/adr/0006-books-third-media-kind.md)) | `book` kind end-to-end: ebooks + audiobooks, book metadata adapter, book parser rules, format quality ladder | grab an ebook and an audiobook, correctly named |
| 3 — Automation | wanted index, RSS loop, failed-download handling, calendar (month grid + rolling agenda with TVmaze air times), notifiers | runs unattended for a month |
| 4 — Ecosystem | Sonarr/Radarr v3 compat shim, conformance vs real tools | Jellyseerr/Prowlarr/Bazarr don't notice the swap |
| 5 — Depth | custom formats, more clients, import lists, anime numbering | parity tail |

## License

[GPL-3.0](LICENSE), matching upstream Sonarr and Radarr — which keeps the door open to porting
their release-parser test corpora as golden tests.
