# Season acquisition review — decisions, corrections, and build status

**Status:** round-3 findings addressed in proposal; no implementation PR  
**Updated:** 2026-10-01 · **Current inspected baseline:** `08af106`

Companion to the [build plan](plan-season-acquisition.md). This is the status
page and disposition record. The initial three-agent review strengthened a
large execution design. Paul's supplied Opus review then challenged its scope,
policy, and operational cost. This revision accepts that central criticism:
the feature should not require a second download/import ownership system.

## 1. Product decisions Paul made

On 2026-10-01 Paul confirmed:

1. Stop adding downloads once the configured target is met. Better source rank
   and custom-format preferences do not add singles over an adequate pack.
2. Allow individually justified singles from completed searches during partial
   discovery, while deferring packs justified only by failed scopes. Retry
   automatically through scheduled work.

On the third review, Paul also explicitly approved automatic retry after seven
days of complete successful client checks without a match, with a visible
possible-duplicate warning. Deployment remains excluded.

These replace the earlier proposal's unbounded quality/format objective and
all-indexers-must-succeed default. They are confirmed decisions, not inferred
agreement with every recommendation in the pasted review.

## 2. Round-1 findings and current disposition

| Finding | Disposition |
|---|---|
| M1: ideal ignores target stopping | Accepted. §3.2 saturates classes at TARGET_MET. §4 rejects above-target or format-only additional transfers. Rank/format become final ties after health/cost; this does not preserve the old `better()` order at the expense of a much smaller equivalent release. |
| M2: execution machinery too large | Accepted with qualifications in §3 below. One plan table, scope unique index, and extended download rows replace attempts/scope/assignment tables. No mutable assignments or retained-pack fallback subsystem. Import still needs static allowlists for ordering cases outside the rank-only argument. |
| M3: discovery cost and outage behavior | Accepted. §5 defines season-first queries, targeted fallback, actual page/request budgets, cooldown, resumption, degraded indexers, scheduled retry epochs, and a real queue change for RetryAt. A pack alone cannot suppress comparison with individual releases. |
| M4: bytes make seeders irrelevant | Accepted. Equal-outcome comparison counts known-zero-seeder torrents before bytes. Round 2 moved unknown-seeder ties after bytes, with explicit missing-metadata handling. This remains a preference, not a new hard rejection rule. |
| S1: no general client idempotency | Accepted factual correction. Transfer identity must be stored before Add; current status ports do not universally echo it. Keep uncertain existing download rows reserved; title reconciliation is useful only when unique. Row-first alone does not make timeout safe. |
| S2: cross-process publication overreach | Accepted. One active service instance; existing import/storage locks remain process-local. Database uniqueness provides scope admission only. No distributed publication project. |
| S3: name legacy paths | Accepted. §2 explicitly lists pack-first item search, validateAutoSearchPack, failure replacement, add/profile-change, Wanted, RSS, and automatic decision seams. Require architecture/service checks against aggregate SeasonWantable decisions. |
| S4: types admit forbidden states | Accepted. One release Pref plus Eligible/Payload sets; baseline stays in normalization/explanation. Real quality monotonicity and target interchangeability get property checks. |
| S5: repository workflow changed | Verified from local Git object 08af106. Current CLAUDE.md requires an independent clone, batched PR, final adversarial review then one gate, and visible ActionDialog results. §9 follows it; the owner's old checkout is preserved. |
| S6: pack coverage is a claim | Accepted. Missing payload records a mismatch and returns episodes to wanted; it is not evidence of release unavailability. |
| S7: RSS searches too broad | Accepted with one-hole clarification: pack-ineligible scopes use feed singles; pack-eligible multi-episode scopes coalesce; one-hole singles may proceed, but a pack still requires individual comparison. |

Nits incorporated: the 3/15 existing files are explicitly Keep unless stated
otherwise; graburn.go versus notInFlight responsibilities are corrected;
episode-aware planned re-grab accounting is labeled new policy. The live pack's
actual cleanup outcome is not asserted from code alone. Migration/ADR numbers
are candidates to verify, not reserved identifiers.

## 3. Simplifications that needed limits

### 3.1 Target saturation does not prove import convergence for every profile

The supplied review reports scratch checks of 675,528 monotonicity cases and
6,392 pack/single combinations. Those tests were deleted by the reviewer; this
revision does not claim to have rerun or independently reproduced their counts.
The reported rank-order result does not cover these additional interactions:

- Missing episode; upgrades disabled; target 2160p; selected 1080p pack plus
  2160p single. After fixing importer compliance with upgrades-off, pack-first
  blocks the single while single-first keeps 2160p. No above-target transfer
  is involved.
- Target Bluray 2160p; WEB-DL pack resolves at 2160p with unverified source.
  `Profile.Met(current, false)` is true under the don't-churn policy, even
  though the claim-based prospective class was below target. A subsequently
  arriving better-source single can be blocked.

The [actual quality implementation](../internal/domain/quality/quality.go)
and [import path](../internal/app/acquisition/import.go) support these cases.
The revised solution is one immutable per-release import allowlist stored
with the plan decision. The pack skips slots supplied by selected better
singles. No assignment table, revision-based reassignment, or custody of
fallback pack bytes is necessary. Failure returns the hole to ordinary wanted;
that may require another download. This limitation is explicit in §7.3.

### 3.2 Measured safety must precede publication

The review correctly identifies claim-based removal, but its suggested return
of measured quality from `commitPlacement` is insufficient on its own.
[placement.go](../internal/app/acquisition/placement.go) can publish via rename
before returning. Validate measured replacement permission before that step,
then authorize cleanup from the same facts. Keep the existing journal/digest
mechanism; do not add distributed assignment fencing.

### 3.3 Row-first submission is useful but not exactly-once execution

[Grab](../internal/app/acquisition/acquisition.go) inserts a row before Add,
but deletes it on Add error. A timeout after client acceptance could erase the
only evidence and permit a duplicate. Retain ambiguous submissions on existing
download rows, use conditional pending submission, and reconcile conservatively.
No separate attempts table or new client idempotency promise is required.
Active/uncertain rows must also survive history dismissal until reconciled;
that small guard remains even without retained payload custody.

### 3.4 A target-meeting pack is not a price-comparison stopping rule

If any TARGET_MET candidate stopped individual searches, a large target pack
could again be selected for one missing episode without discovering an easy
single. §5.1 therefore stops targeted episode alternatives only when an
individual target-meeting candidate is known. Season searches that return
suitable singles can save those queries. Below-target singles still trigger
fallback for missing episodes, because acceptable is not the same as best
configured satisfaction. Round 2 defers targeted upgrade-only queries to their
seven-day cadence or surplus budget after holes have been served.

## 4. Earlier adversarial findings after simplification

| Original issue | Current treatment |
|---|---|
| Incomplete discovery can justify an unnecessary pack | Asymmetric progress approved by Paul; pack explanations cannot use failed scopes as absence. Reduced indexer sets and efficiency tradeoffs are explicit. |
| Admission race and duplicate dispatch | Transactional freshness/unique scope and conditional submission remain, using existing download records. |
| Unknown-size comparator circularity | Fixed lexicographic tuple remains; known-zero seed risk precedes bytes, unknown-seeder metadata follows bytes. |
| Measured downgrade before removal | Shared prepublication fix remains mandatory. |
| Placement assignment fencing | Removed with mutable assignments; existing journal safeguards and single-instance serialization remain. |
| Retained-pack cleanup/history custody | Retained-pack subsystem removed; unresolved execution/placement records still cannot be deleted. |
| Pagination metadata and format-policy freshness | Retained as typed discovery evidence and snapshot inputs. |

## 5. Round-2 findings and disposition

Opus's second review accepts saturation, the smaller execution model, and the
static allowlist correction to its original convergence claim. Its two new
blockers concern indefinite season claims and scheduler starvation.

| Finding | Disposition in revised build plan |
|---|---|
| N1: stalled/uncertain/review work holds entire season | §6.4 adds 48-hour observed torrent-stall retirement, 30-minute uncertain-row parking, review isolation, and a 48-hour upper bound on the broad active-plan claim. Existing rows preserve affected episode reservations; unaffected holes trigger a successor automatically. Never-submitted intents close without failure charges. |
| N2a: RSS omitted from daily budget | §5.2 explicitly allocates RSS, interactive reserve, and search from one effective budget. At unknown cap the local budget is 250, of which RSS gets 96 and search 100; at cap 100 RSS slows to 32 minutes with 45 search tokens. New cap configuration and ledger are named. |
| N2b: upgrade sweeps starve holes | Missing first and watchedFirst within tiers. Round 3 qualifies query preemption/rotation with feasible open-set admission so waiting jobs cannot expire every active comparison. Missing episodes get targeted fallback; upgrade-only episodes ordinarily use season queries and deep-search cadence/surplus. Throughput figures expose the difference. |
| N2c: irrelevant RSS invalidates cooldown | Only an eligible class improvement over known providers resets cooldown; merge fresh checkpoint evidence before making new requests. New holes/profile changes remain independent triggers. |
| S1: planned rows look like client jobs | Distinct planned download state with CHECK migration; §6.1 lists reader dispositions covering the nine ListActiveDownloads sites and UI/history queries. Explicit reservation query, not a globally broadened client-job query. |
| S2: narrow fallback/multi-episode file rules | At pack import, already terminal failed/rejected single providers can supply derived fallback eligibility under the import lock, subject to live policy and no other custody. Indivisible files require every covered episode to be permitted. No retained-payload subsystem. |
| S3: no concrete successor trigger | Scope transition enqueues a deduplicated, budgeted season job transactionally when unreserved work remains. Active sibling failures set replan_pending; settlement bounds their wait. Restart reconciliation repairs enqueue intents. |
| S4: unknown seed count beats efficiency | Unknown-seeder tie moved after known bytes; explicit incident fixtures for missing versus reported-zero seeder data and truthful explanations. |
| S5: new work mislabeled existing | Explicit new request limiter, cap configuration/usage table, lease-checked jobs.payload checkpoints, pre-Add transfer persistence, and torrent telemetry. No imaginary existing discovery store or client status fields. |

The 30-minute absence suggestion is **qualified**, not implemented as proof of
rejection. No-match polls cannot guarantee a late accepted transfer will never
appear. Title dedupe can miss both jobs before they appear. Park the affected
episodes and release the broad claim. Authoritative evidence permits confirmed
rejection; round 3 additionally permits an explicitly risky retry after seven
days, as Paul approved. Neither timeout is mislabeled proof of nonacceptance.

Torrent retirement needs confirmed client removal before failure/replacement.
Round 3 adds safe deletion of proven unshared partial payload or an explicit
cleanup-pending custody record; preserved bytes cannot be orphaned. If removal is uncertain, retain its episode custody.
A healthy or user-paused job is never failed just because its broad plan ages.

Other round-2 nits: the precise budget-yield-to-partial boundary is specified;
re-grab caps query the 12-hour window instead of LIMIT 100; deployment was raised
with Paul and explicitly excluded. No Ansible or client deployment requirement
is added to this document-only task or the build plan.

The old documents under Paul's checkout are intentionally preserved under the
current independent-clone rule. The revised documents are real files in
`/private/tmp/monarr-season-plan-revision-20261001/docs/`, not message-only text.
Use that pair together; the checkout pair is superseded and was not silently
rewritten. No claim is made that Opus's cloud checkout contained the revision.

## 6. Round-3 findings and disposition

Opus accepted all round-2 changes, then found a scheduler interaction: rotating
all waiting seasons spreads evidence until it expires and incorrectly makes
partial singles acquisition normal. The admission rule now reserves enough
capacity to finish each open season comparison, instead of starting every clock.

| Finding | Disposition |
|---|---|
| R1: interleaving defeats the 24-hour evidence window | §5.2 limits the feasible open set, estimates full page/query costs against real token replenishment and timeouts, reserves one-hour deadline slack, and prefers completion. Queued jobs have no evidence clock. All automatic origins use the same queue. Only intrinsically oversized work is partial at admission; scheduling interference requeues/refreshes instead. |
| S1: stale tracker seeds and orphan partial bytes | §6.4 uses connected seeds/availability for the 48-hour rule and a seven-observed-day no-progress backstop independent of tracker counts. Proven unshared partial payload is deleted at retirement; otherwise an explicit cleanup-pending row retains custody until the extended cleanup coordinator safely resolves it. |
| S2: indefinite uncertain episode; reserved single blocks packs | Paul approved seven-day risk retry with a warning. The plan specifies valid inventory evidence, gap/config resets, atomic successor enqueue, superseded import authority, and late-arrival reconciliation. A reserved single is excluded from successor pack targets/imports without banning the pack; holds and indivisible conflicts retain their fences. |
| S3: interactive reserve too small | Interactive requests can borrow unspent S after I/headroom, never R or beyond B. Automatic work visibly defers and its completion reservation is recalculated; that interruption does not authorize premature singles. |
| S4: RSS tokens spent early in the day | Persist per-indexer next_rss_at, wake at due times, spread calls over the calculated cadence, respect RetryAt, and avoid restart catch-up bursts. Pacing is explicitly new work. |

The worked example needed an arithmetic correction: at t=12h, `16+33+1`
finishes seasons two and three and only starts season four. The last 32 requests
finish at t=24h. Conservative two-page admission can instead start season four
at t=24h and finish its 33 requests then. Either schedule keeps every opened
season inside its own evidence window; none is artificially partial.

Other nits addressed: indexer count is N, distinct from interactive reserve I;
reuse existing RemoteError.RetryAt; no quiet-day token banking beyond rolling S;
discovery permutation fixtures assert appropriate outcome classes, while pure
Build asserts winner invariance for a fixed pool; separate provider NZB/grab
caps are explicitly outside the v1 API-call ledger.

The revised documents have been real files in the independent clone throughout
rounds 1–3. The owner's old checkout was intentionally preserved; it must not be
used as the builder's specification. The two current files linked in the handoff
are the authoritative pair. No deployment or implementation was performed.

## 7. Implementation and validation status

- Proposal revision: complete through round 3, incorporating Paul's three policy
  answers and explicit exclusion of deployment.
- Runtime changes, migrations, downloads, and deployments: none.
- Working location: independent clone, branch `codex/season-acquisition-plan`.
  Original draft in Paul's checkout remains unchanged.
- PR: none created; implementation/merge has not been requested in this turn.
- Final runtime gate: not run for documentation-only changes.
- Document verification: local links, headings/fences, whitespace, policy
  consistency, and Git scope checked before handoff.
- Cleanup: clone retained for the editable deliverable; no remote branch,
  background service, or build artifacts created by this revision.

Before implementation, verify actual provider caps/query coverage and recheck
current repository instructions. When a build is authorized, keep coherent
commits in one PR, obtain the required final adversarial review, run the gate
once, and record actual results here. Do not turn a document review into a
claim that the feature has passed runtime validation.
