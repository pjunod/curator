# deploy/

Docker packaging lives here; the build context is the repo root.

| File | What |
|---|---|
| `Dockerfile` | multi-stage build (web UI → Go binary → distroless runtime); build with `docker build -f deploy/Dockerfile -t monarr .` from the repo root |
| `docker-compose.example.yml` | tracked compose template — **copy, don't edit** |
| `.env.example` | tracked env template documenting every variable |
| `bootstrap.sh` | creates the host directories with the right ownership — **run this before the first `up`** |
| `k8s/` | Kubernetes manifests for the floating single-instance pattern (replicated-block and Litestream variants) |

```sh
cd deploy
cp docker-compose.example.yml docker-compose.yml   # yours; gitignored
cp .env.example .env                               # optional; gitignored
./bootstrap.sh                                     # host dirs, correct owner
docker compose up -d --build
```

## Why bootstrap.sh exists

Docker will happily start a container whose bind mount is broken. If the
host directory behind a bind mount does not exist, the daemon creates it as
`root:root`; the container runs as `${PUID}:${PGID}`, cannot create
`monarr.db` inside it, and Monarr exits at startup. There is no way to
declare a bind mount's ownership in a compose file, so the host has to be
right first — that is all this script does.

It is idempotent and conservative. It creates missing directories with the
right owner, and for directories that already exist with the wrong owner it
**reports** them and exits non-zero rather than recursively chowning
anything: on a media pool that is a multi-hour operation, and on the wrong
path it is destructive. The report prints the `chown` for you to run
yourself. `--dry-run` shows the plan without touching the disk.

If you skip it and hit the failure anyway, Monarr's startup error names the
directory, both ownerships, and the exact fix — this is the one class of
misconfiguration that explains itself.

The copy must stay **in this directory** — the build context and path
fallbacks resolve relative to the file, not your cwd. To run from
anywhere else: `docker compose -f deploy/docker-compose.yml up -d --build`.

Pulls update the examples, never your copies — diff against them after big
updates. Built-in defaults: `/srv/monarr` (config), `/srv/pool` (media +
downloads), user `1000:1000` — the `.env` only needs the values where your
setup differs.

## Why discovery has a second container

A Docker bridge is a multicast boundary: Monarr can serve port `7676` through
the published TCP mapping while its `_monarr._tcp` announcement remains trapped
inside the bridge. The Compose template therefore runs `monarr-discovery` from
the same image with host networking. That process only advertises the host's
existing port; it does not open the database, proxy requests, or read keys and
media.

If you copied `docker-compose.example.yml` before v0.18.5, copy the
`monarr-discovery` service into your gitignored `docker-compose.yml`, then run:

```bash
docker compose up -d --build   # rebuild both processes and expose DNS-SD on the LAN
```

Set `MONARR_DISCOVERY_NAME` in `.env` when `monarr` is not a useful name in the
native server picker. Host networking is the requirement: publishing UDP 5353
through a bridge does not reproduce multicast DNS.

Storage rules (what must not sit on NFS/Gluster, floating-node patterns):
[docs/deployment.md](../docs/deployment.md). Mount rules and a worked
pool layout: the README's Docker section.
