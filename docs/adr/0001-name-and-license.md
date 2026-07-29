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
  and Omniarr were also clean). Before the repo goes public: register the GitHub org, the
  Docker Hub namespace, and a domain. The Go module path is
  `github.com/pjunod/monarr` (see the two Amendments below).
- **License: GPL-3.0**, matching upstream.

## Consequences

- The golden parser corpus port from Sonarr/Radarr test suites is legally unlocked; parser
  fidelity becomes a measurable conformance number instead of a vibe.
- We may read upstream source freely when in doubt about behavior.
- Downstream forks and distributions must remain GPL-3.0; for a homelab app this costs
  essentially nothing.
- Default HTTP port chosen alongside the name: **7676** — free in the *arr ecosystem and
  deliberately not 8989/7878, since Monarr will run next to Sonarr/Radarr during migration.

## Amendment (2026-07-17): GitHub org is `monarr-media`

The bare `monarr` GitHub handle turned out to be occupied by a **dormant user account**
(created July 2011, zero repositories, no profile) — invisible to a project-collision sweep,
but GitHub user and org names share one namespace, so the org could not be created. The
product name is unaffected.

Decision: the GitHub org is **`monarr-media`** (GitHub orgs are free) and the module path is
**`github.com/monarr-media/monarr`**. A dormant-username release request to GitHub Support
remains worth filing; if granted, transfer the repo (GitHub redirects old URLs) and update
the module path in one commit.

Container images: **publish to GitHub Container Registry** —
`ghcr.io/monarr-media/monarr` — not Docker Hub. Docker Hub organizations require a paid
subscription, while GHCR is free for public images, lives next to the repo, and pushes from
GitHub Actions with the built-in `GITHUB_TOKEN`. (If a Docker Hub presence is ever wanted,
a free *personal* account named `monarr-media` can hold the namespace at no cost.)

## Amendment 2 (2026-07-29): the repo and module path are `pjunod/monarr`

The repository moved out of the `monarr-media` org and onto the owner's personal account. The
org bought nothing a personal account does not — there is one maintainer — and it was one more
namespace to keep alive.

Amendment 1 ended with "transfer the repo (GitHub redirects old URLs) and update the module
path in one commit", and treated that redirect as cover. It is not, and that is the part worth
recording. GitHub does forward requests from a renamed or transferred repository, but the
forward stops the moment anything is created at the old path — and a vacated org name is
claimable by anyone. So the redirect is a courtesy that a third party can end, on a schedule
nobody here controls.

That is survivable for a browser URL and not survivable for a Go module path. A module path is
resolved literally: `go get github.com/monarr-media/monarr` asks that host for that repo, and
if the answer ever changes, it changes for every downstream consumer at once, with no fix
available except each of them editing their own imports. A path that only works while a
redirect holds is a promise the project cannot keep.

Decision: module path, repository, and container image are all **`github.com/pjunod/monarr`**
/ **`ghcr.io/pjunod/monarr`**. Nothing outside this repo imports the module yet — no tagged
release has been published and the repo is still private — so the change costs one mechanical
242-file commit and breaks nothing. That is the whole reason to do it now. After the first
public release the same edit would strand every importer, and the module proxy would keep
serving the old path's published versions forever, so the wrong path would never fully go away.

The dormant-username request for the bare `monarr` handle stays worth filing, but the same
argument now applies to it: take it before the first public release or not at all. A prettier
path is not worth an ecosystem-visible break, and moving again later would cost exactly what
this commit cost — which is cheap only while the answer is "nobody depends on it".
