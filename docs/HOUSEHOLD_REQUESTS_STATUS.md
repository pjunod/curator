# Household accounts and requests — implementation status

**Status:** reviewed and locally qualified; protected PR checks pending · **Updated:**
2026-11-13 · **Base:** owner discovery PR
[#160](https://github.com/pjunod/curator/pull/160), merged as
`97149893fc6d28c5edb4655ebce5bbfd6beaabec`.

Read [ADR 0030](adr/0030-household-accounts-and-requests.md) for the security and
admission contracts. Work stays in the owned external-drive workspace. The
owner approved request/discovery previews and clarified that both apps must
retain independent accounts; Cinema is an optional linked sign-in method.

## Delivery

- [x] Astra reviewed the original proposal, then the revised local-account/link
  boundary after the owner's standalone-app clarification.
- [x] Previewed local Curator sign-in and optional Cinema consent on a 390px phone.
- [x] Durable local accounts, sessions, invitation enrollment and recovery rules.
- [x] Deny-by-default household authorization and safe web/native navigation/DTOs.
- [x] Submit/join/withdraw and owner review/decline with private notes.
- [x] Exact-scope approval, fresh verification, overlap claims and durable admission.
- [x] Optional Cinema grant/link/revocation implementation on SQLite and Hiqlite;
  final cross-repository review and runtime regression evidence pending.
- [ ] Final adversarial review, regression suites and protected PR checks.
- [ ] Merge and storage lifecycle receipts; no live deployment requested.

## Acceptance boundaries

Local sign-in, requests and owner approval work without Cinema. Unlinking
Cinema preserves local access/history and revokes only integration-derived
sessions. Test same-name identities, deleted/recreated Cinema accounts, concurrent
linking, last-owner changes, account suspension, restart and Cinema outages.
Members must fail to reach every owner/admin/compatibility/event/metrics route.
Test file arrival during approval, season/episode overlap, withdrawal races,
restart between approval and admission, and exact offloaded episode scopes.

## Runtime evidence

The household backend compiled and both web and native TypeScript checks passed
on 2026-11-13. These are compile checks only. No household implementation tests
or end-to-end Cinema deployment checks have run yet. The separate owner discovery milestone has its own status and evidence.
Cinema's pinned 1.97.1 core/replicated-store baseline compiled locally before
any Cinema source changes. Production endpoint/callback readiness is unverified.

## Account independence checkpoint — 2026-11-13

The owner reiterated that standalone accounts must remain available in both
applications. The implementation keeps Curator credentials, roles, account IDs
and request ownership local. Local account login does not call Cinema. Account
management and invitations require a local session, with owner operations
requiring a local owner. At that checkpoint, Cinema validation was not implemented and non-local
methods failed closed. The final candidate below now implements scoped grants.
The reviewed linking design requires explicit consent and forbids name/email
merges. Cinema implementation and outage/unlink regression evidence remain open.

The admission checkpoint fixes now preserve immutable request scope through
import, verify existing transfer generation and exact coverage before following
it, recheck presence inside acquisition's own lock, and narrow offload
notifications cumulatively with errors propagated. Follow-up hardening rejects
cancelled/completed import attempts when attaching existing work and validates
current title/episode identity before fulfillment. Regression fixtures and the
final adversarial review are still required before running the final test gate.

Astra's standalone-account checkpoint found no Cinema dependency or implicit
identity merge. Its UI-copy finding is fixed. API routing, owner-private request
reads, event authorization and web/native navigation now use explicit session
`accessRole`, separate from the permanent account role. Unknown methods fail to
member authority. The final candidate now has the grant validator.
The newly authored authority regression has not run before final review. A
host-operator reset was still required at that checkpoint; it is now implemented
and covered by the authored recovery regressions.

## Final candidate boundaries

Local owner recovery is implemented as `monarr accounts reset-password`, with
secret input on stdin, preserved history and credential/session revocation.
Cinema links now hold only issuer/subject identity. Each derived session has its
own source-bound opaque grant, checked authoritatively on every protected
request. The session's `accessRole` is separate from the account's role. Local
unlink/disconnect commits before asynchronous remote revocation. Native handoffs
use a second S256 proof and are one-use, expiry- and credential-generation-fenced.

The Cinema counterpart is in its own owned `codex/curator-delegation` branch,
integrated with current main `483d2bcf3` before qualification. It adds independently
recognized extension schema 132 and uses shared transaction code across stores.
No native Cinema credentials or administrator claims leave Cinema. Old Cinema
versions are reported unavailable rather than weakening the login contract.

Authored regressions cover local restart, invitation/last-owner races, permission
and private-note boundaries, SSE revocation, stale approval, exact leaf claims,
offload scope, import allowlists, immutable admission/origin, explicit linking,
outage/unlink, native proof and stale-server results. Isolated browser fixtures
cover desktop/390px member-to-owner flows without enabling auth on the shared
legacy acceptance server. Compile checks are not execution evidence. No household
regression suite has run before the final review. No live deployment is requested.

## Final implementation review and fixes

Astra requested changes on Curator `123d8c70` / Cinema `6f12ce9d`. The
implementation now has per-leaf durable due order, post-approval subscription
semantics and claim release on present-scope re-review; primary-only verification;
retained offload epochs and durable primary-only notification exclusions; no
ordinary replacement dispatch for household origins; immutable full-ID/profile/
leaf/epoch checks at publication; existing-owner transfer attachment before
indexer search; fair expiring remote revocations; separate initiation throttles;
shared mobile attempt generations; same-device completion proof; private query
partitioning with stale principal action refusal; and pre/post-provider full-ID
seed checks. Cinema fixes include exact callbacks, cold-page layout mounting,
atomic schema preflight/verification, strict historical empty-tail rewinds,
deterministic cleanup, delayed-writer expiry and an atomic active-client cap.

New store, API, mobile and real-browser fixtures cover these boundaries. Only
compilation, typechecking and generation have run on the fixes. Astra's targeted
static recheck must clear before regression execution, PR checks and merging.

The first static fix recheck held the test gate for five residual issues. The
follow-up corrections use the real transfer columns and Unix epoch sentinel,
share held-publication checks across lookup/bind/poll, settle exact offload
notifications before following existing owner transfers, commit exclusions and
notification rewrites atomically, lift native attempt fencing to every connect
method, and bind all personal web mutations/link caches to the initiating actor.
New fixtures include real worker attachment, failed rewrite rollback, mismatched
publication/download generations, delayed owner connection and a stale member
form after another tab signs in. Cinema's populated rewind fixtures now seed
valid foreign-key parents. A second targeted recheck is pending; no runtime
regression evidence is claimed yet.

Astra cleared production fixes on Curator `99eb022` / Cinema `e0f60d0c3`.
Two worker-fixture-only corrections (valid automatic intent and actual download
generation in the origin assertion) were explicitly cleared without another
review. Final suites now run before PR qualification and merge. No deployment.

## Final local qualification

Astra's production and current-main composition reviews cleared all findings
before execution. Full Go regressions and required lint pass. Web has 174 passing
tests; mobile has 155; the real-binary desktop/phone suite has 271. OpenAPI and
SQL generation reproduces the committed output with no changes. New runtime
cases include request/approval at 1280px and 390px, cross-tab owner note isolation,
stale member action refusal, same-device native completion, offload rollback and
attachment without indexers. Fixture-only corrections preserved the reviewed
production safeguards (metadata preview port, journaled local offload state,
protected metrics and fail-closed durable authority during storage failure).

Current main `19d865c` is integrated; feature VERSION remains `0.52.0` and native
version `0.29.0`, build 42. PR qualification, exact-head checks and merge remain.
Cinema's companion uses its required merge train. No live deployment or real
Cinema account linking has been performed.

## Pull requests

Curator [PR #162](https://github.com/pjunod/curator/pull/162) is submitted with
all local gates passing. Cinema companion PR #1586 is submitted as WIP to its
enabled merge train; both applications remain independently usable. Protected
current-candidate checks and both merge receipts remain pending.
