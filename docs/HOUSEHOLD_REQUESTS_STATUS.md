# Household accounts and requests — implementation status

**Status:** implementing · **Updated:** 2026-11-13 · **Base:** owner discovery
PR [#160](https://github.com/pjunod/curator/pull/160), pending protected checks
after integration with main at `715122e`.

Read [ADR 0030](adr/0030-household-accounts-and-requests.md) for the security and
admission contracts. Work stays in the owned external-drive workspace. The
owner approved request/discovery previews and clarified that both apps must
retain independent accounts; Cinema is an optional linked sign-in method.

## Delivery

- [x] Astra reviewed the original proposal, then the revised local-account/link
  boundary after the owner's standalone-app clarification.
- [x] Previewed local Curator sign-in and optional Cinema consent on a 390px phone.
- [ ] Durable local accounts, sessions, invitation enrollment and recovery rules.
- [ ] Deny-by-default household authorization and safe web/native navigation/DTOs.
- [ ] Submit/join/withdraw and owner review/decline with private notes.
- [ ] Exact-scope approval, fresh verification, overlap claims and durable admission.
- [ ] Reviewed optional Cinema grant/link/revocation implementation on both stores.
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
requiring a local owner. Optional Cinema session validation is not implemented;
non-local session methods currently fail closed rather than inheriting authority.
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
member authority; Cinema remains unavailable until its grant validator exists.
The newly authored authority regression has not run before final review. A
supported host-operator reset for a sole owner who loses their password is still
required; an active last-owner row alone is not a usable recovery procedure.
