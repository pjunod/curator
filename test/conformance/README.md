# test/conformance — ecosystem conformance harness (Phase 4)

A docker-compose harness that boots **monarr + real Jellyseerr + real Prowlarr + real Bazarr**
and scripts their happy paths against the compat shim: connect, sync indexers, request a
movie, request a season, enumerate the library for subtitles.

If Jellyseerr can complete a request cycle against `/sonarr/api/v3` and `/radarr/api/v3`, the
shim works. No amount of unit testing substitutes for this (blueprint §8). Runs on a CI
schedule, not per-commit.
