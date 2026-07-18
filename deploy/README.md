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

Pulls update the examples, never your copies — diff against them after big
updates. Without a `.env`, compose falls back to local dirs beside this
file and user 1000:1000.

Storage rules (what must not sit on NFS/Gluster, floating-node patterns):
[docs/deployment.md](../docs/deployment.md). Mount rules and a worked
pool layout: the README's Docker section.
