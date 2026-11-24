# Household requests and discovery — daily use after v0.50.0

**Status:** proposed for owner review; owner approved implementation after Astra review; owner discovery first.
**Baseline:** Curator v0.50.0, `08d8cb43fb86a368cd581e0d542cdf5875144bca`.
**Written:** 2026-11-13. **Identity preference:** standalone Curator accounts, with optional Cinema login/linking.

This narrows sections 4–6 of the earlier improvement proposal to the household's
next daily-use workflows. Review the three rendered journeys first. Application
code, schemas, accounts and running installations have not been changed.
Setup, migration, playback and the separate OpenSSF work remain outside scope.

## 1. The everyday journey

Discover → choose the missing title/season → send or join a request → owner
approval → follow progress → verified library availability.

Requests records who wants what and whether it was approved. Activity remains
the owner’s operational view of the existing acquisition/import pipeline.
Requests does not become another transfer queue or recovery console.

### Discover knows what the household already has

Keep the current visual style and provider catalogue. Add a For your library
view beside Browse all. Start with explicit, explainable shelves:

| Shelf | Reason shown | What is new |
|---|---|---|
| More like a library title | “Based on Arrival”; the member can change the seed | Use a kind-aware TMDB provider path with existing outbound controls; preserve the advanced TV recommender. |
| Complete a collection | The owned film and exact missing collection member | New bounded collection-membership lookup and caching; identity must be verified. |
| More seasons of shows you own | Which seasons/episodes are present and which are missing | Reuse episode inventory; request only the chosen missing scope. |

Every result has one context-appropriate action: Request, Also want this,
Follow progress, View in library, or Request again for an offloaded target.
All availability is a clear selector; Still to get filters complete available
targets out without hiding partial seasons or open requests. Do not conflate a
library catalogue entry with present files. If availability cannot be checked,
show Unknown and require a fresh check before acquisition admission.

No new hosted AI or watch-history dependency. Preserve current semantic ranking,
provider budgets, lazy loading, caching and partial-coverage warnings. A failed
provider affects its shelf, not the entire page. First release has no autoplay
trailers, popularity inferred from clicks, or pretend “continue watching.”

### A member requests a choice, not a download configuration

The detail dialog shows current library availability before submission. A
series request names seasons and, when needed, missing episode scope. It does
not silently include future seasons. Movie requests use the household’s default
copy/profile; additional quality copies and editions remain owner workflows
until their separate request UI is approved. Initial new requests cover movies
and shows; existing owner book workflows remain available.

A note to the owner is optional and private. Members do not choose filesystem
paths, clients, indexers or arbitrary quality profiles. The confirmation shows
the exact scope and Waiting for approval. Submitting never means downloaded.

Matching requests share an acquisition target and retain separate requester
subscriptions. “Also want this” joins the target; it does not replace another
member’s note or broaden the requested seasons. Race-safe duplicate checking
runs on submission and approval. Different scopes/copies are not blindly merged.

### One Requests page, adapted to the member’s capability

Members see Mine and Household: the title, requested scope, people following it,
and plain-language progress. Private notes stay private. Pending requests can
be withdrawn by their requester. Stopping following an approved/shared request
does not cancel acquisition, delete files or affect another member.

The owner sees Needs review, In progress, Available and All requests. Reviewing
a request shows scope, current library state, overlaps, household quality and
library location. Approve & search rechecks the displayed target; Decline needs
a member-visible reason. A material change requires another review rather than
silently approving a broadened target. Operational problems link the owner to
the existing Activity details instead of reproducing file/recovery controls.
On a phone, selecting Review opens a focused review screen/dialog; never append
the only approval action below a long request list.

## 2. Availability is separate from the approval decision

Persist request decisions as Pending, Approved, Declined or Withdrawn, with an
audit trail and fulfillment history. Derive current fulfillment from existing
library, inventory and acquisition records. “Available” is not an irreversible
request decision: a later offload or missing file changes current availability.

| What the member sees | Required evidence |
|---|---|
| Waiting for approval | A saved request without an approval. |
| Approved · searching | Accepted target, but no current usable release or active transfer. |
| Downloading / processing / importing | Existing pipeline state, scoped to the requested target. |
| Partially available | Some requested episodes/copy requirements are met; name the remainder. |
| Waiting on the owner | A genuine operator decision; hide raw filesystem/credential details. |
| Available in library | Verified placement/presence for the exact requested scope, not 100% download progress. |
| Offloaded / availability unknown | Existing offload evidence or unavailable/stale library checks. Never treat an outage as proof files are absent. |

Playback readiness is a separate Cinema fact. The first action is View in
library; show a play/open-in-Cinema link only when a valid authorized destination
and the relevant playback/indexing availability can be established.

## 3. Cinema identity, Curator permissions

Curator and Cinema each retain independent accounts. Cinema sign-in is an optional first-class integration, not a prerequisite for using Curator. Curator owns local owner/member roles, local credentials and membership suspension. A local account may explicitly link one or more verified Cinema identities; never merge accounts by name or email. Disconnecting Cinema revokes its grants and sessions without deleting the Curator account or its local credentials. The owner prefers reusing an existing Cinema login when this integration is chosen. A read-only inspection of the local
Cinema source (`4ddbf1229`) confirms local admin/standard accounts, device tokens,
scoped machine keys and `/api/v1/me`. Its README and architecture still
mark OIDC as planned. A supported third-party sign-in/delegation contract has
not been established by this inspection; the deployed capability must be
checked before promising a Sign in with Cinema flow.

Prefer an explicit Cinema sign-in handoff: a short-lived, single-use,
audience-bound authorization that Curator exchanges server-side for a scoped
Curator session. Astra reviewed the contract: reuse Cinema instance_id and the retired-on-delete user wire UUID. Add exact-callback client registration, state/S256, consent, single-use codes and source-session-bound opaque grants with authoritative revocation checks. Both SQLite and replicated Hiqlite need the grant store; do not forward a
Cinema admin token, share browser cookies across apps, collect Cinema passwords
in Curator, or treat identity alone as Curator administrator authority. Pin the
Cinema server/principal identity; display names are not account keys. Revocation,
logout, account disablement and native-device login must have explicit behavior.
Curator keeps a local owner recovery path and never requires Cinema availability
to perform already-authorized server administration.

| Capability | Household member | Owner |
|---|---|---|
| Browse allowed catalogue/library and requestable targets | Yes, through safe member DTOs | Yes |
| Send/join requests and follow current status | Yes | Yes |
| Withdraw own pending request | Yes | Yes |
| Approve/decline and set acquisition policy | No | Yes |
| Configure servers, roots, profiles, credentials and access | No | Yes |
| Delete media, resolve retained files, cancel shared acquisition | No | Existing guarded owner operations |

Permission enforcement belongs on every applicable server route, including
native, pairing and compatibility paths. Hiding controls is presentation, not
access control. Existing administrative machine keys stay administrative;
member devices receive revocable credentials with the same bounded capability.
Anonymous admin access cannot coexist with a claimed member security boundary.
No member invitations are exposed until the complete boundary is verified.

If Cinema needs new delegation support, scope that as a small reviewed
cross-application contract before household rollout. Do not silently create a
second account system. Owner-only library-aware discovery can proceed while
that contract is resolved. Child-account restrictions and trusted auto-approval
are later decisions; the first proposal assumes adult members and owner approval.

## 4. Implementation shape and delivery order

1. **Shared availability and all owner discovery shelves.** Build one identity-keyed read
   model with independent catalogue identity, observed presence and acquisition facts, using existing
   library facts and verified observations. Reuse it in search, Discover,
   recommendation results and title details; avoid per-poster filesystem probes.
   Deliver kind-aware library-title related shelves, verified movie collections
   and missing-season review before beginning household request UI.
   The current recommendation service is TV-only; movie seeds need a separate
   kind-aware provider path with the same bounded outbound controls.
2. **Identity and one complete request journey.** Confirm the Cinema handoff;
   implement scoped authorization, request records, submit/join/withdraw,
   review/approve/decline and progress. Deliver matching web and native behavior.
   Approval calls existing library/acquisition services using durable admission
   and target identity; no new downloader or parallel import queue.
3. **Complete household parity and rollout.** Exercise scoped sign-in, review,
   approval, withdrawal and progress on native and responsive web. Curated
   trending/provider rows remain available under Browse all throughout.
4. **Only after daily use.** Consider trusted auto-approval, notifications,
   watch-history signals, trailers, more edition/copy requests and book requests.
   Setup and migration remain deferred.

Give the request application service ownership of decisions and subscriptions;
keep library presence with the library and execution with acquisition. Extend
the existing OpenAPI contract and feature-specific UI/API modules. Do not split
services/databases or introduce an event bus for this slice. New read projections
may join existing facts; they do not grant cleanup or execution authority.

## 5. Acceptance before release

- Two simultaneous requests/approvals for the same target admit one intent.
  Existing available or in-flight targets are reused. Scope changes force review.
- Members cannot read secrets/paths or call administrative native, compatibility,
  settings, pairing, file or queue mutations. Revoked principals/devices lose access.
- Partial seasons, interrupted workers, held imports and unverified Cinema
  indexing never show full requested availability. Offload and availability
  uncertainty update every surface consistently.
- An outage does not turn owned titles into missing titles or authorize a second
  acquisition. Provider failures preserve usable independent shelves.
- Existing reservation/cancellation/generation/epoch and retained-file safeguards
  remain enforced by the existing services.
- Desktop, keyboard, phone and native request journeys show action feedback in
  the viewport, preserve focus, and share the same permissions and status facts.
- Owner approved the proposed behavior and Astra review on 2026-11-13. Final implementation still receives adversarial review and the
  repository’s required gates before merge.

## 6. Renderings and evidence

All titles, availability and member activity in the mockups are illustrative.
Artwork is a layout placeholder. The mockups use the current classic palette;
the implementation must continue respecting existing themes and appearance.
Desktop renderings and 390px layouts were inspected, with no page overflow or
browser runtime errors. This is design validation, not application testing.

- [Discover desktop](renderings/household-discovery/discover-desktop.png)
- [Request dialog and member phone](renderings/household-discovery/request-and-phone.png)
- [Owner review](renderings/household-discovery/owner-requests.png)
- [Discover phone](renderings/household-discovery/discover-phone.png)
- [Member Requests phone](renderings/household-discovery/requests-phone.png)
- [Owner review phone](renderings/household-discovery/owner-phone.png)

Baseline code inspected: Curator Discover page/service, recommendation ownership
handling, API authentication and style tokens. Current Discover already marks
`inLibrary`; the proposal extends it to actual availability and request intent.
Cinema inspection covered its README, architecture, API auth routes and login
implementation; no Cinema code or deployed state was changed.

## 7. Astra review corrections

Astra completed design review on 2026-11-13. Presence, catalogue identity,
requests/subscriptions and execution remain independent. Exact scope includes
copy, materialized aired episode IDs and owner policy revision. Requests that
overlap by episode/copy share atomic claims and a durable idempotent admission
record, including restart and withdrawal/approval races. If current verified
files satisfy the request, approval records fulfillment with zero search.

Browse observations expire after 13 hours against the normal 12-hour scan
schedule. Each title/copy proof binds the root identity, successful file-ID
snapshot, title/settings revisions and last-checked time. A failed scope is
Unknown; skipped copies and unstatable directory entries retain their records.
Explicit acquisition approval later needs a targeted current recheck; a browse
observation alone never authorizes a download or cleanup.

Cinema uses GET /api/v1/me. Its integer user ID alone is insufficient as a
delegated stable subject; the reviewed handoff must bind a non-recycled subject
and issuer and support removal/revocation. Standard users are never implicitly
Curator owners. No member rollout before central deny-by-default route policy,
safe library/search/request DTOs, scoped native pairing and protected ancillary
metrics/events/compatibility surfaces. Member navigation cannot use the owner
full-library or global-search DTOs as a shortcut.

Updated renderings retain all cards on 390px, reflow the collection row, show
owner Add/View actions before household activation, and capture the actual
phone viewport. Separate phone dialog, list→review→back, private-note and
partial/unknown examples replace the former desktop component crop. They are
responsive web design previews; no native application test is claimed.
