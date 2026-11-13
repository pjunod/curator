# Curator improvements — product experience and architecture after OpenSSF

**Status:** proposed for review · **Written:** 2026-11-13 · **Baseline:**
`1db094a` (upstream main when assessed) · **Security implementation:**
[PR #118](https://github.com/pjunod/curator/pull/118)

Companion to the [usage guide](usage.md) (existing behavior),
[architecture](architecture.md) (original design), and
[security checks in PR #118](https://github.com/pjunod/curator/pull/118/files) (the separate OpenSSF implementation).
This proposal covers the HolyArrs comparison and the resulting product,
architecture, packaging and release suggestions. It authorizes no product
implementation. Build one approved milestone at a time; re-verify source
interfaces and the baseline before editing code.

## 1. Objective and retained strengths

Make Curator easier to install, move to, and use with a household while
preserving its acquisition decisions, explicit recovery workflows, books,
quality copies, native mobile app, and Runner/Cinema integration.

HolyArrs offers useful patterns: guided migration, a household request flow,
library-aware shelves, aligned feature modules, explicit SQL ownership and
shared outbound controls. Its Go/SvelteKit/SQLite stack is not a reason to
replace Curator's Go/React/SQLite stack.

Existing Curator capabilities must not be proposed as new features:

| Existing capability | Extend rather than replace |
|---|---|
| Web Classic/Plex/Theater layouts and native mobile | Improve journeys and permissions across both clients. |
| Discover and bounded TV description/seed search | Add library-aware shelves and explain their inputs. |
| TMDB and Trakt import lists | Add request-policy integration and optional Plex watchlists. |
| Sonarr/Radarr compatibility | Preserve Jellyseerr/Overseerr, Prowlarr and Bazarr consumers. |
| Disk adoption, manual import and measured media information | Add guided setup and migration around those services. |
| Quality copies and ebook/audiobook editions | Keep independent monitoring, profiles and acquisition identity. |
| Durable jobs, season planning and retained-source recovery | Reuse them; never introduce a second acquisition engine. |
| Direct metadata adapters and optional local encoder | Preserve provider choice and explicit model installation. |

## 2. Priorities and dependency order

Effort below is relative scope, not a delivery estimate. Large work needs a
separate implementation contract and adversarial review before construction.

| Order | Work | Scope | Dependency and acceptance outcome |
|---|---|---|---|
| 0 | Finish OpenSSF rollout and assess findings | Small/medium | #118 checks pass; default-branch score publishes after merge; findings triaged. |
| 1 | Guided first run and connection diagnostics | Medium | Fresh install reaches a tested configuration without guessing paths or providers. |
| 2 | Read-only migration preview, then reviewed import | Large | Existing setup imports through current services; rollback is scope-checked. |
| 3 | Household identities and request approval | Large | Members can request without acquiring admin capabilities. |
| 4 | Library-aware shelves and simpler default journeys | Medium/large | Users can discover, request and follow a title on web/mobile with honest statuses. |
| 5 | Boundary improvements alongside feature work | Incremental | New seams have ownership and regression protection without a wholesale rewrite. |
| 6 | Release trust, packaging, localization and optional integrations | Separate slices | Each slice has its own evidence and operating-cost decision. |

Start wizard work before the request system because it improves every new
installation and does not require a new identity model. Build member-aware
personalization only after identities exist. Backend boundary work belongs
inside the relevant slices, not in a prerequisite reorganization project.

## 3. Guided setup and migration

### First-run wizard

Add a resumable owner-only wizard: secure owner access, select metadata
providers, assign library/download locations, configure a downloader and
indexer, test connectivity, then preview the first title. Existing configured
servers should see a diagnostic checklist rather than a forced onboarding
flow. Never overwrite saved settings merely because a test failed.

Show concrete path and capability results: downloader-reported path,
Curator-visible path, readable/writable roots and whether hardlinks are
possible. A test must not start a download, move a library, or remove files.
The UI must distinguish an unreachable service from an authentication failure
and a readable-but-unwritable destination. Redact credentials from output.

**Acceptance:** a browser test completes first run against fake providers;
resuming preserves choices; a path mismatch yields an actionable explanation;
existing setups retain their configuration and can skip the wizard.

### Migration preview and apply

Start with Sonarr/Radarr, then Prowlarr. Seerr household requests depend on
§4; music imports depend on an approved music model. Support an API source
first; treat backup archives as a separate parser/security task.

The preview must enumerate titles, root mappings, profile mappings,
monitoring, indexers and clients, plus unsupported fields. Custom formats,
tags and history must be explicitly supported or explicitly omitted. Never
claim complete migration while silently dropping those fields. Secrets are
re-entered or imported through a separately reviewed path; previews and
receipts contain secret references, never plaintext credentials.

A proposed migration manifest records `schemaVersion`, source kind/version,
source IDs, target mappings, configuration revision, planned operations and
warnings. Apply requires the unchanged reviewed manifest and an idempotency
key. This contract is proposed, not an existing API.

Apply through current library/settings services. Record exactly which rows
were created or changed. Never write the source app or move its media files.
Do not run competing automatic acquisition while evaluating a migrated setup:
show a clear choice about which app owns automation.

Undo checks identities and post-import revisions. An imported title edited
since migration is a conflict to review, not permission to delete it. Undo
must not remove acquired media or another workflow's rows. An exportable
receipt explains applied, skipped, conflicted and unsupported operations.

**Acceptance:** API fixture migration is dry-run only until applied; a repeat
apply creates no duplicates; source apps receive no writes; undo leaves later
edits and acquired files intact; malformed input changes nothing.

## 4. Household identities and requests

The current [authentication implementation](../internal/api/auth.go) is a
server login/API-key mechanism, not a household authorization model. Do not
give members the server's administrative API key, including through mobile
pairing. Retain current installations' access during a reviewed migration.

Proposed capabilities distinguish owner/admin administration, member
requests, and optional trusted auto-approval. Enforce permissions on the
server and on native/compat routes, not only by hiding buttons. Each device
gets revocable credentials with a bounded scope. Plan owner recovery and
session revocation before exposing invitations.

Request records need stable media/provider identity, requester, selected
season/copy/edition, approval policy and timestamps. Admission must be
idempotent across web, mobile, import lists and compatibility clients.

| Proposed request state | Meaning |
|---|---|
| `pending` | Waiting for an authorized approval. |
| `approved` | Acquisition intent accepted and linked to current library targets. |
| `declined` | Rejected, with a member-visible reason. |
| `withdrawn` | Requester withdrew an eligible request; this does not delete files. |
| `available` | The requested target is proven present, not merely downloaded. |

Acquisition substatus is derived from the existing pipeline. A held import,
partial season, failed grab, or retained-source decision must not be reported
as ready. Approval calls the existing library/acquisition services; it never
submits a separate download flow. Requests remain linked to their targets so
later changes in availability are explained.

Add duplicate-request merging, per-member limits, owner-selected roots and
profiles, and an audit trail. Let trusted-member rules skip approval while
still applying quotas, profile restrictions and acquisition budgets. Define
child access separately: hiding posters is not a media-server playback
restriction. Avoid claiming parental controls without the complete boundary.

**Acceptance:** authorization tests prove members cannot edit infrastructure,
read secrets or approve requests; two simultaneous approvals produce one
intent; partial/held imports never show available; revoked devices lose
access; web and native clients expose the same permissions.

## 5. Discovery and everyday interface design

Add library-aware rows incrementally: missing collection entries, related
titles based on an explicit seed, and compatible upgrades or additions only
where their intent is clear. Reuse the existing
[Discover service](../internal/app/discover/discover.go) and
[recommendation service](../internal/app/recommendation/service.go).

Each shelf explains why an item appears, whether it is owned/requested, and
which provider or local evidence contributed. Respect the current provider
budgets, caching, cancellation and partial-coverage reporting. Existing local
semantic ranking remains optional; no hosted AI dependency is introduced.

A household-popular or continue-watching row requires actual watch signals
and permission to use them. Library ownership and page clicks are not watch
history. Begin with library-only shelves; evaluate opt-in Cinema/Plex/Jellyfin
signals separately. Subscription rows need user-selected services and region,
provider attribution and honest availability freshness.

Add trailer previews as an optional interaction: off/muted by default where
appropriate, explicit activation on touch devices, no keyboard-focus trap,
reduced-motion support, and lazy loading. Decide between embedded providers
and local trailers only after evaluating provider terms, privacy and bandwidth.
Do not proxy/download trailers implicitly or introduce autoplay as a dependency
of browsing.

The default journey should be discover → choose/request → follow progress.
Retain operator workflows and the exact rejection/recovery evidence. Hide
advanced detail progressively, but keep any decision that can affect retained
media visible at the moment it matters. Native mobile should receive the
request/discovery improvements rather than becoming a web-only afterthought.

**Acceptance:** desktop, keyboard and 390-pixel phone journeys work; shelves
handle missing metadata; owned/requested titles stay accurate after an add;
a held import has a plain-language reason and safe next action; trailer-off
mode makes no trailer-provider requests.

## 6. Architecture improvements

### SQL ownership and service boundaries

Write down ownership of acquisition, library, requests, profiles and job
records. Keep generated SQLite access within infrastructure. Where application
services import SQLite errors/types, introduce narrow consumer-owned
interfaces and shared error contracts as the relevant feature changes.

The current [dependency tests](../internal/arch_test.go) protect the pure
core and adapters; extend them to cover new request/migration boundaries.
Avoid a universal repository abstraction or a mass database rewrite. New
services must not bypass acquisition's reservation, custody and publication
checks through direct SQL writes.

### Align feature organization where it reduces change cost

Give requests and migration recognizable homes in backend services, web
components, API clients and browser fixtures. Split Activity, the web API
client and the acquisition service along coherent responsibilities when
changing them. File size is a signal to inspect, not an arbitrary rule or a
reason to move code without reducing coupling.

Keep one source for API contracts: the existing
[OpenAPI specification](../internal/api/openapi.yaml), with generated server
artifacts. Do not adopt route-generated OpenAPI alongside it. Consider
feature-specific typed client modules while preserving the same wire contract.

### Shared outbound controls

Inventory existing adapter timeouts, retries, rate limits and redaction before
adding a shared transport/admission layer. Define endpoint identity, bounded
breaker state, cancellation, retry classification and visible health. A
shared circuit breaker must never retry a grab automatically: submission
uncertainty already has domain semantics and cannot be flattened into a
transport failure. Preserve provider Retry-After and current request budgets.

### Events, storage and concurrency

Measure history size, transaction contention and backup costs before deciding
whether to split event/history storage. A split database would require an
outbox or equivalent recovery contract so an import cannot commit without
its authoritative event/receipt. This is a measurement-gated option, not an
approved migration.

Keep SQLite on local storage and reuse the leased queue. Postgres/multi-host
work is already discussed in [ADR 0008](adr/0008-distributed-execution.md);
it is not delivered by the current queue and is not a prerequisite for these
product improvements.

**Acceptance:** refactors preserve wire contracts and behavior; targeted
regressions cover import/cancellation/retry boundaries; architecture checks
reject forbidden dependencies; one failed provider cannot create retries of
uncertain downloads or starve interactive work.

## 7. Release trust and packaging

Finish the separate OpenSSF rollout first. Inspect actual Scorecard results
before choosing remediation; do not maximize a number by weakening useful
checks or adding cosmetic files.

| Follow-up | Proposed decision and evidence |
|---|---|
| Repository rules | After CI has run, select stable required contexts and a documented owner recovery/bypass policy. Decide required approvals explicitly for a solo maintainer. |
| OpenSSF Best Practices | Enroll only after auditing the criteria and supplying accurate evidence. The badge is self-certification, separate from Scorecard. |
| GitHub Releases | Turn reviewed version tags into published release notes/artifacts. The release badge should describe an actual release. |
| Signed images and provenance | Define the registry, supported architectures, reproducible build inputs, SBOM format, keyless signing and verification instructions before publishing. |
| Vulnerability scans | Add container and Rust/dependency coverage with a triage baseline, severity policy and documented exception expiry. Do not suppress findings simply to turn CI green. |
| Continuous fuzzing | Identify useful existing parsers and seed corpora, then add bounded CI runs or a suitable fuzzing service. Preserve crash reproducers outside caches. |
| Slim/full packaging | Measure compressed image size, idle RAM and workload peaks for a common corpus. Evaluate an optional lean image that omits bundled FFmpeg/encoder while keeping current full behavior available. |
| Upgrade/rollback | Preview schema changes, verify backups and restore on a fixture. A prior binary is not a safe rollback against an incompatible newer database. |
| Support diagnostics | Consider a redacted doctor/support export: versions, reachability, capabilities, mount checks and relevant job IDs; never credentials or complete private library data by default. |

No benchmark claims against HolyArrs should be published without the same
hardware, library, workload and measurement method. Its compressed download
size and Curator's runtime memory are different quantities.

**Acceptance:** verify signed artifacts from a clean environment, trace an
artifact to source, restore a pre-upgrade fixture, and publish measurement
methodology alongside results. Full-image users keep existing media-format
and ranking capabilities.

## 8. Remaining integrations and scope decisions

| Candidate | Recommendation |
|---|---|
| TRaSH/profile synchronization | Evaluate after inventorying current custom-format support. Preview diffs, preserve owner overrides and make updates reversible. |
| Trakt/Plex watchlists | Reuse current Trakt import lists; route household automation through request rules. Treat Plex authorization and per-user identity as a separate adapter contract. |
| Subtitle acquisition | Keep Bazarr compatibility. Consider built-in missing-language visibility first; provider search, matching and synchronization need a separate scope and must not be advertised before enabled. |
| Music | Defer until product demand justifies artists/albums/tracks, metadata, matching, naming and profiles. The common pipeline helps but does not supply that model for free. |
| Localization | Extract strings and establish translation/review tooling before choosing languages. Cover errors, pluralization, dates, accessibility and native clients; avoid a headline language count without maintained coverage. |
| MCP/AI connection | Consider a separately authenticated, capability-limited interface after request authorization. Start read-only; writes must use existing request approval, idempotency and audit rules. |
| Phone/TV | Continue the existing native companion. TV browsing/request clients require demand and device-specific accessibility work; playback stays with Cinema/Plex/Jellyfin. |
| Metadata convenience | Improve provider setup and failure explanations. Keep direct keys/provider choice; an operator-funded mandatory proxy is an ongoing service commitment. |
| Popularity/social badges | Optional public-launch work. Prefer release/license/security evidence over follower counts. |

## 9. Non-goals and review decisions

This proposal does not switch React to Svelte, split into microservices,
introduce a mandatory metadata proxy, replace Runner or media-server playback,
remove books/native mobile, or implement every competitor feature at once.
These changes would spend effort without addressing the identified journeys.

Before implementation, review these decisions in order:

1. Approve guided setup and a Sonarr/Radarr migration preview as the first
   product milestone; confirm whether Prowlarr belongs in that same slice.
2. Agree the household capability model and whether child accounts are in
   the initial scope. Produce a security-reviewed authorization contract.
3. Choose the first three library-aware shelves and whether any watch-history
   integration is allowed. Default to library-only inputs.
4. Agree release registry, architectures, ruleset policy and signing scope.
5. Approve architecture refactors only with their feature slice or measured
   maintenance bottleneck; review event-database splitting independently.

Each approved slice needs a concrete implementation contract, relevant unit
and integration tests, browser/native verification where applicable, and a
reviewable migration/rollback story before release. Estimates follow those
contracts; this document does not promise dates.

## 10. Comparison sources

HolyArrs sources were reviewed on 2026-11-13. They describe another project's
claims and design; they do not establish comparative reliability.

- [Architecture](https://docs.holyarrs.com/dev/architecture/) and [modules](https://docs.holyarrs.com/dev/modules/): feature boundaries and ownership rules.
- [Requests](https://docs.holyarrs.com/requests/): household approval flow.
- [Migration](https://docs.holyarrs.com/migrate/): preview and move workflow.
- [Outbound controls](https://github.com/HolyArrs/HolyArrs/blob/main/app/backend/internal/core/outbound/outbound.go): endpoint circuit breaker.
- [API limits](https://docs.holyarrs.com/reference/old-app-api-limits/): compatibility limits to retain in the comparison.
- [Metadata](https://docs.holyarrs.com/library/metadata/) and [subtitles](https://docs.holyarrs.com/library/subtitles/): provider choice and held-back features.
