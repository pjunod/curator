# deploy/

Docker packaging lives here; the build context is the repo root.

| File | What |
|---|---|
| `Dockerfile` | multi-stage build (web UI → Go binary → distroless runtime); build with `docker build -f deploy/Dockerfile -t monarr .` from the repo root |
| `docker-compose.example.yml` | tracked compose template — **copy, don't edit** |
| `.env.example` | tracked env template documenting every variable |

```sh
cd deploy
cp docker-compose.example.yml docker-compose.yml   # yours; gitignored
cp .env.example .env                               # optional; gitignored
docker compose up -d --build
```

The copy must stay **in this directory** — the build context and path
fallbacks resolve relative to the file, not your cwd. To run from
anywhere else: `docker compose -f deploy/docker-compose.yml up -d --build`.

Pulls update the examples, never your copies — diff against them after big
updates. Built-in defaults: `/srv/monarr` (config), `/srv/pool` (media +
downloads), user `1000:1000` — the `.env` only needs the values where your
setup differs.

Storage rules (what must not sit on NFS/Gluster, floating-node patterns):
[docs/deployment.md](../docs/deployment.md). Mount rules and a worked
pool layout: the README's Docker section.
