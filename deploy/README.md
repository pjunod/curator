# deploy/

Docker packaging lives here; the build context is the repo root.

| File | What |
|---|---|
| `Dockerfile` | multi-stage build (web UI → Go binary → distroless runtime); build with `docker build -f deploy/Dockerfile -t monarr .` from the repo root |
| `docker-compose.example.yml` | tracked template — **copy, don't edit**: |

```sh
cd deploy
cp docker-compose.example.yml docker-compose.yml   # yours; gitignored
docker compose up -d --build
```

Pulls update the example, never your copy — diff against the example after
big updates. Paths/user come from `.env` (`MONARR_DATA`, `MONARR_POOL`,
`PUID`, `PGID`) or fall back to local dirs beside this file.

Storage rules (what must not sit on NFS/Gluster, floating-node patterns):
[docs/deployment.md](../docs/deployment.md). Mount rules and a worked
pool layout: the README's Docker section.
