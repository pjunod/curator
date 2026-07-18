# Conformance harness

Live-fire test of the Sonarr/Radarr v3 personalities (ADR 0003) against the
real ecosystem: **Jellyseerr**, **Prowlarr**, and **Bazarr**.

```sh
docker compose up -d --build
```

| App | UI | Point it at |
|---|---|---|
| Jellyseerr | http://localhost:5055 | Sonarr → `http://monarr:7676/sonarr`, Radarr → `http://monarr:7676/radarr` |
| Prowlarr | http://localhost:9696 | Settings → Apps → Sonarr/Radarr, same URLs |
| Bazarr | http://localhost:6767 | Settings → Sonarr / Radarr, same URLs |

All three ask for an API key: Monarr generates one on first start — find it
in the Monarr UI under **Settings**, or:

```sh
curl http://localhost:7676/api/v1/settings
```

(The image is distroless — there's no shell inside the container to exec.)

## What to verify

- **Jellyseerr**: connection tests pass for both personalities; requesting a
  movie/series creates the item in Monarr's library (check the Monarr UI).
- **Prowlarr**: app sync pushes its Torznab feeds into Monarr's indexer
  list (Monarr → Settings → Indexers) with no duplicates on re-sync.
- **Bazarr**: series/episodes/files enumerate without errors.
- Anything a consumer calls that the shim doesn't know appears in Monarr's
  log as `compat: unknown v3 request` — file an issue with that line.

The same request flows are replayed hermetically (no Docker) by the
fake-consumer suite in `internal/compat/compat_test.go`, which runs in CI.
