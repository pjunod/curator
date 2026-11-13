# Household discovery — delivery status and review decisions

**Status:** implementing owner discovery · **Updated:** 2026-11-13 ·
**Baseline:** `4a9c10c` (main 0.50.1, after the 0.50.0 release).

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
- [~] Revision-fenced primary-copy presence observations and shared read projection.
- [~] Kind-aware library-seed recommendations, collections and season review.
- [~] Web/native parity, unknown/stale/error and viewport/focus behavior.
- [ ] Complete regressions, final adversarial review, required local and CI gates.
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

## Verification

Compile/typecheck checks are allowed during implementation. Per CLAUDE.md,
application unit/browser suites and required CI run after complete implementation
and an independent adversarial review. Results, PRs, merge state and receipts will
be recorded as milestones complete. No live deployment is authorized by this work.
