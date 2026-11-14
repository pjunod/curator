# Household discovery — delivery status and review decisions

**Status:** owner implementation reviewed; protected CI pending · **Updated:** 2026-11-13 ·
**Baseline:** `d2e1594` (main, including the separately completed security fixes).
**PR:** [#160](https://github.com/pjunod/curator/pull/160).

Companion to the [approved plan](HOUSEHOLD_DISCOVERY_PLAN.md),
[shared availability decision](adr/0029-shared-availability.md),
[usage](usage.md) and [settings](settings.md). User authorized the full sequence:
Astra review and corrections, owner availability/discovery, then household
requests using Cinema login. Existing source and runtime storage stay external.

## Delivery sequence

- [x] Astra reviewed all proposal and rendering material.
- [x] Correct independent catalogue/presence/acquisition/request facts, exact
  identities/scope, movie provider path, real phone layouts and Cinema /me route.
- [x] Owner discovery precedes all household Request controls. Preserve Add/View.
- [x] Revision-fenced primary-copy presence observations and shared read projection.
- [x] Kind-aware library-seed recommendations, collections and season review.
- [x] Web/native parity, unknown/stale/error and viewport/focus behavior.
- [x] Complete regressions, final adversarial review and required local gates.
- [ ] Required hosted CI gates and protected merge.
- [ ] Owner discovery PR, merge and task storage receipt.
- [ ] Reviewed Cinema issuer/subject/revocation and single-use sign-in handoff.
- [ ] Household capability boundary and safe web/native DTO/navigation.
- [ ] Durable scoped request/approval/admission workflow and final gates.

## Astra findings addressed in the owner implementation

The previous renderer hid a card and overflowed its phone collection shelf;
those are fixed. Actual 390px request dialog, member Requests and owner
list→review→back captures include private note, partial and expired checks.
These are responsive-web previews, not a claim of native app execution.

The TV-only recommendation adapter is not reused with movie IDs. Movie and TV
provider paths and result types stay kind-aware. Untracked external metadata
says Not in Curator, never missing on disk. Catalogue lookup errors are Unknown.

Files skipped by an incomplete scan are preserved. Presence proof is a separate
read-only check after successful reconciliation, fenced by the title and shared
configuration revision captured before filesystem reads. A root must be readable,
match its recorded volume identity; an empty uninitialized root cannot supply
first-pin evidence. Post-upgrade and changed facts
remain unknown until checked. Proof expires at 13h against the normal 12h scan
schedule; it shows its timestamp and never authorizes acquisition or deletion.

Atomic reconciliation now fences pre-walk snapshots and file generations. It
refuses foreign path ownership and preserves imported episode mappings. Known
volume changes stop insertion and pruning. Uninitialized roots cannot prune or
clear offload intent; only migration-time file evidence can first-pin them
without explicit owner review. The reviewed volume dialog uses current custody,
revision and identity fingerprints; confirmation invalidates proofs and rejects
old in-flight scans. See [volume rendering](renderings/household-discovery/volume-review-phone.png).

## Independent accounts and optional Cinema integration

User clarified on 2026-11-13 that both apps are standalone, with integrations
as first-class citizens. Curator needs its own individual accounts and local
owner/member authority. Cinema sign-in is optional, explicitly linked to a local
account. No display-name/email auto-merging. Disconnecting Cinema revokes that
grant and its derived sessions without deleting local accounts or credentials.
Local account sign-in and administration do not depend on Cinema availability.
This supersedes Cinema-only account assumptions in earlier review material.

## Household contract work remains separate

Cinema GET /api/v1/me identifies a token caller; it is not delegated sign-in.
A stable non-recycled issuer/subject and current revocation behavior are required.
Members must never inherit owner keys, full library/path DTOs, pairing scopes,
compatibility, metrics or event access. Before request admission, recheck exact
scopes and claim overlapping episode/copy targets atomically with durable
idempotent admission, including crash and withdrawal/approval races. Browsing
proof alone is not admission proof. Setup, migration and watch history stay out.

## Review and verification

Astra's final review and static finding rechecks cleared the implementation.
Corrected findings cover atomic scan races, scope/symlink completeness, retained
custody, root-ID reuse, native stale callbacks, truthful inventory labels,
post-provider seed conflicts, credential-scoped provider caching and presence
validity through descriptive metadata/quality updates. Browser checks also fixed
grid overflow and user-triggered alternate-name reveal without polling scroll.

Local results on 2026-11-13:

- `make test`: passed after scanner and API-fixture corrections.
- `make lint`: passed, zero issues.
- `make test-web`: 172 tests plus seven dependency-patch regressions passed.
- `make test-mobile`: typecheck and 143 tests passed.
- `make test-e2e`: all 263 desktop, phone and lifecycle-phone tests passed.
- `make gen`: API and SQL bindings regenerated; schema 0083 migration/bootstrap,
  changed-volume, foreign-path and import-race regressions passed.

Final browser captures include real 1280px and 390px discovery, in-viewport Add
and volume dialogs, acknowledgment gating and return focus. Hosted CI and merge
are tracked by PR #160; this document does not claim deployment or native device
execution. Durable logs and captures remain in the owned external-drive receipt.

## Verification policy

Compile/typecheck checks are allowed during implementation. Per CLAUDE.md,
application unit/browser suites and required CI run after complete implementation
and an independent adversarial review. Results, PRs, merge state and receipts will
be recorded as milestones complete. No live deployment is authorized by this work.

## Merge receipt — 2026-11-13

PR [#160](https://github.com/pjunod/curator/pull/160) merged as
`97149893fc6d28c5edb4655ebce5bbfd6beaabec` after Astra review, local regression
suites and all 19 required hosted checks on candidate `31d47751a65c2d5e2988b3557010be670c9a19ff`.
The owned milestone lease is finished and external-drive evidence is preserved.
Household delivery continues in [HOUSEHOLD_REQUESTS_STATUS.md](HOUSEHOLD_REQUESTS_STATUS.md).
No live deployment.
