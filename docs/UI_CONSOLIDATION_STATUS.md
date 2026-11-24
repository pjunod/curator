# UI consolidation — implementation and verification

**Status:** reviewed; final gates on [PR #153](https://github.com/pjunod/curator/pull/153) · **Updated:** 2026-11-13 · **Baseline:** `6219581`

Companion to the [usage guide](usage.md), [settings reference](settings.md)
and [pipeline contract](PIPELINE_CONTRACT.md). The owner approved the rendered
consolidation in this chat. The user instruction to remove Dev supersedes the
old repository instruction about putting enablement controls in that tab.

## Scope

One Activity projection covers visible transfers and retained import history,
with In progress, Needs attention and History, complete counts and paging,
search and imports-only scope. `/imports` redirects to the unified page.
Settings Resolve/Dev are removed. Disk tools and conditional historical
receipts move under System Storage. Current import/cancellation/custody and
cleanup protections remain enforced; Runner-owned source cleanup has no
Curator retry button. Household requests/discovery remain subsequent work.

## Progress

- [x] Read current main, including the newer cleanup provenance/source-byte safeguards.
- [x] Unified local index, service classification and OpenAPI endpoint.
- [x] Activity, Settings, System and legacy-link navigation implementation.
- [x] Regression fixtures and current browser acceptance workflows.
- [x] Complete implementation; Astra adversarial review and corrections.
- [ ] Final lint, Go/web/mobile and browser gates.
- [ ] PR, required CI, merge and storage lifecycle completion.

## Decisions

Historical negative publication IDs remain read-only. Automatic failures,
Runner environmental holds and interrupted imports do not become permanent
manual recovery work. Active workers retain custody. Pending cancellation
with retained records still exposes its exact-record review after writer exit.

The optional published-file mapping is retained under the Runner connection;
the unused web automatic-refresh toggle is removed. Existing saved settings
and staged-copy APIs are preserved. Historical receipt deletion/retirement is
outside this UI change. No live deployment is part of this task.

## Review and validation

Compile/generation checks are allowed during implementation. Per repository
workflow, unit and browser suites run after completed implementation and
adversarial review. Results and PR/merge state will be recorded here.

Astra reviewed the full implementation and rechecked corrections. Findings fixed:
local failures and partial approvals remain actionable; released reservations
leave attention; retained-file acknowledgement binds to the exact polled
snapshot; stale mutations are disabled; deployment drain allows status reads;
measured progress, destination and focus behavior remain accessible. Bulk history
deletion was removed rather than claiming all receipts survive deletion. Existing
per-record removal remains guarded. A subsequent identity-reuse finding pinned open details and batch selections to
download ID, generation and epoch; changed attempts require fresh selection.

Local validation: pinned lint passed with zero issues; Go suite passed all
application/API packages and the only failure was two renamed documentation
anchors, corrected with the docs package rerun passing. Web: 169 tests passed.
Native client: 142 tests passed. Production web/binary build passed.

The first 260-case browser run found stale test navigation/selectors and nested
Escape bubbling through Activity details. The shared dialog now stops cancel
propagation, reviewed by Astra. A complete browser rerun covers that fix and the
new storage navigation. Final browser and CI results remain pending.

PR #153 is the canonical live CI and merge record. The storage remount regression
now uses More → System → Storage and matches the System link including its
health indicator. Its focused rerun passed. The nested-dialog propagation fix
and explicit Safari opener focus were independently rechecked by Astra. Local
browser rerun and required current-head CI must pass before merge; no branch
protection bypass is authorized. Task cleanup is recorded through the registered
external-drive storage lifecycle after merge.
