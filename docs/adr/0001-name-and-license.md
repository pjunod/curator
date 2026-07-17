# ADR 0001 — Name "Monarr", license GPL-3.0

- **Status:** Accepted
- **Date:** 2026-07-17

## Context

The project is a unified rewrite of Sonarr and Radarr. It needs a name with no collisions in
the *arr ecosystem, and a license decision that determines how much we can lean on upstream
source — in particular their release-parser test suites, which encode a decade of real-world
edge cases and are the single most valuable asset we could inherit (blueprint §8).

Sonarr and Radarr are both GPL-3.0. A permissive license (MIT/Apache) would force a strict
clean-room approach: no reading upstream source while designing, no porting their parser test
corpora, no lifting custom-format scoring rules later.

## Decision

- **Name: Monarr.** A July 2026 web/GitHub sweep found no collisions (runners-up Singularr
  and Omniarr were also clean). Before the repo goes public: register the `monarr` GitHub org,
  the Docker Hub namespace, and a domain. The Go module path is `github.com/monarr/monarr`.
- **License: GPL-3.0**, matching upstream.

## Consequences

- The golden parser corpus port from Sonarr/Radarr test suites is legally unlocked; parser
  fidelity becomes a measurable conformance number instead of a vibe.
- We may read upstream source freely when in doubt about behavior.
- Downstream forks and distributions must remain GPL-3.0; for a homelab app this costs
  essentially nothing.
- Default HTTP port chosen alongside the name: **7676** — free in the *arr ecosystem and
  deliberately not 8989/7878, since Monarr will run next to Sonarr/Radarr during migration.
