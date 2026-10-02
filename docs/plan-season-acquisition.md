# Season acquisition planner — satisfy the configured target with efficient releases

**Status:** revised after Opus review round 3; not implemented  
**Updated:** 2026-10-01 · **Source baseline:** `08af106` (VERSION 0.35.3)  
**Product decisions:** Paul approved target saturation and limited singles
progress during incomplete searches on 2026-10-01. He also approved automatic
retry of unmatched uncertain submissions after seven days of successful checks,
with a visible possible-duplicate warning.

Companion to [usage.md](usage.md), [architecture.md](architecture.md), and
[ADR 0014](adr/0014-target-profiles.md). This is the implementation contract.
Read §§1–5 for selection, §§6–8 for execution, and §§9–11 for delivery and
validation. The [review record](plan-season-acquisition-review.md) distinguishes
accepted findings from qualifications to the suggested simplifications.

This revision replaces the earlier ownership-heavy proposal. Keep the small
pack-plus-singles optimizer. Use one execution-plan table and extend existing download
records; account separately for the new scheduler request ledger; do not introduce separate attempt, scope, or assignment tables.
Retain only the execution protections the simpler design actually needs.

## 1. Objective and settled policy

> Fill every obtainable wanted episode at the best available outcome up to
> its configured target, then prefer reliable, efficient acquisition.

An episode is the unit of need. A season/copy is the planning scope whenever
packs compete with singles. A release is a delivery mechanism, not a quality
policy. Complete the configured discovery procedure before dispatching a
normal plan. Explicit partial-search behavior is defined in §5.3.

### 1.1 Target satisfaction stops additional downloads

Paul approved these defaults after Opus review:

- Once a prospective episode provider meets the configured target, a higher
  source rank or custom-format score cannot justify another transfer for it.
- Below target, obtain the best acceptable available quality. A below-target
  pack and better singles remain a valid combination.
- Incomplete discovery may supply individually justified singles while packs
  whose justification depends on failed searches are deferred. Retry later
  through the scheduler; do not require a human after every exhausted job.

Acceptance is still governed by the existing floor, resolution cap, language,
monitoring, upgrade permission, verification, identity, failure, and hold rules.
Saturation is a selection policy; it does not change `Profile.Acceptable` to
reject a source above target. Existing satisfactory files remain protected.

| Facts | Required choice |
|---|---|
| 5/6 on disk at WEB-DL 2160p; same-quality pack; suitable smaller E05 single | E05 single; existing five earn no fictitious upgrade credit. |
| 3/15 protected existing files; singles for 9 of the 12 missing; qualifying pack supplies all 12 | Pack; do not submit nine redundant singles first. |
| Three missing episodes have 2160p singles; pack is 1080p; target is 2160p | Pack plus those singles, when 1080p is allowed. |
| Target WEB-DL 1080p; 15 GB WEB-DL pack versus ten 25 GB Remux singles | 15 GB pack: both attain the target for every episode. |
| Pack and singles attain identical target classes | Apply the common health/cost ordering in §4.3. |
| Required English; higher-resolution candidate is German-only | Exclude the candidate; quality does not compensate for wrong language. |
| Only supply for E12 violates the floor | Other obtainable episodes may proceed; E12 remains wanted. |

The 3/15 example assumes the existing three files are Keep. If they are
upgrade-wanted, a qualifying pack may also improve them; count actual benefits.

### 1.2 What the Heated Rivalry incident establishes

Read-only inspection on 2026-10-01 found item 121 with five WEB-DL 2160p files
and missing E05. Best targeted Bluray 2160p. A WEB-DL 2160p pack was recorded
at `2026-10-01T20:39:45Z`; Runner showed 34.6 GiB. The E05 release about seventy
seconds later was manually selected by Paul. Its Runner failure is separate.

The source explains a consistent mechanism: all six episodes can be wanted
under the higher target; the item auto-search loop tries a pack first;
`SeasonWantable.CurrentQuality` becomes unknown when any episode is missing;
the aggregate decision then accepts the pack. Same-quality existing episodes
can be skipped at import. That does not reclaim the pack's downloaded bytes.

Historical candidate comparisons and the pack's complete import/cleanup trace
were not retained or re-inspected here. Do not claim to know which alternative
was returned to the original automatic search, or that its payload was actually
deleted, solely from the code's normal cleanup behavior.

## 2. Scope and integration boundaries

Plan key: `(mediaItemID, copyID, seasonNumber)`; copy zero is primary. Never
cross copy boundaries in candidates, reservations, imports, or explanations.
Automatic v1 packs require a regular, fully aired, fully monitored known season
with no conflicting custody. A parked single reservation excludes its episode
from successor targets/imports but does not by itself prohibit a pack for other
episodes; holds and unknown/indivisible coverage keep conservative fences (§6.4).
Existing protected files do not disqualify packs:
this deliberately relaxes today's requirement that every child be wanted.

| Entry point | Required integration |
|---|---|
| `AutoSearchItem` | Replace the pack-first loop and `validateAutoSearchPack` aggregate eligibility with season discovery and per-episode normalization. |
| Search on add and profile change | Route through that same item/season service. |
| Backlog | Admit bounded season/copy work, with request budgets and cooldowns in §5. |
| `handleFailure` | Reconcile plan downloads, then enqueue fresh season planning for unmet work. Never re-search a failed season wantable through the old aggregate decision. |
| Exact Wanted selection | Preserve selected episodes and episode-only behavior with `AllowPacks=false`; no sibling upgrades. |
| RSS | Pack-ineligible scopes use eligible feed singles without a season search. For pack-eligible scopes with at least two acquirable episodes, coalesce into planning. With one acquirable episode, an eligible single can proceed directly; a pack feed row must trigger comparison before automatic acquisition. |
| Interactive/manual grab | Preserve explicit operator choice and label it manual. Respect active holds and reconcile overlapping automatic work. |
| Movies/books | Selection unchanged; shared measured-replacement safety fixes must preserve their contracts. |

Audit callers of `decision.Decide`, `searchAndGrabBest`,
`searchAndGrabBestReserved`, `notInFlight`, and `autoGrab`. Add an architecture
check plus service fixtures proving automatic TV decisions never evaluate a
`SeasonWantable` aggregate. Interactive rejection display may still do so.

Non-goals: general set cover, arbitrary multi-episode bundles, mixed-quality
packs, cross-season packs, selective client downloads, Runner extraction fixes,
new codec/HDR policy, above-target extra transfers, and multi-process filesystem
publication. V1 supports one running acquisition/import service instance. The
SQLite scope claim protects database admission; it does not make existing
process-local import/storage locks a distributed system.

## 3. Normalize policy into a small pure input

### 3.1 Existing seams to reuse

| Source | Responsibility |
|---|---|
| [quality.go](../internal/domain/quality/quality.go), [decision.go](../internal/domain/decision/decision.go) | Acceptance, floor, language, target stopping, and authorized episode replacements. |
| [wantable.go](../internal/domain/wantable.go), [wanted.go](../internal/app/acquisition/wanted.go) | Copy/episode facts and in-flight scope. |
| [automation.go](../internal/app/acquisition/automation.go), [indexersearch.go](../internal/app/acquisition/indexersearch.go), [searchplan.go](../internal/app/acquisition/searchplan.go) | Search collection, query alternatives, and automatic callers. |
| [graburn.go](../internal/app/acquisition/graburn.go) | Title duplicate lookup and re-grab caps; same-season suppression is in `notInFlight`. |
| [acquisition.go](../internal/app/acquisition/acquisition.go), [download_control.go](../internal/app/acquisition/download_control.go) | Grab, client submission, reconciliation, failure, and holds. |
| [import.go](../internal/app/acquisition/import.go), [placement.go](../internal/app/acquisition/placement.go) | Measured placement, live library protection, and crash recovery. |
| [indexer port](../internal/ports/indexer.go), [client port](../internal/ports/downloadclient.go) | Discovery metadata and actual client capabilities. |

Reverify signatures at implementation time. The client API is currently
`Add/Statuses/Remove/Test`. `AddOptions` carries name, transfer identity, and
priority, not file selection. Cost the entire pack transfer.

### 3.2 Episode eligibility and outcome class

Snapshot every known episode, copy profile, measured files, monitoring/dates,
custom-format rules, blocklists/caps, holds, identity, destination, and relevant
indexer/client configuration. Keep current, unverified, unselected, future,
and reserved episodes protected. A reservation is not an on-disk file.

For each candidate, evaluate each relevant episode through the existing policy
adapter, without using aggregate season quality. A normalized eligible episode
means an authorized missing fill or replacement. A wrong-language baseline
can justify a lower-resolution correction; do not compare that baseline's raw
rank against an already authorized correction.

Define candidate class after hard gates:

```text
class(q) = TARGET_MET             if profile.Met(q, true)
           quality.Rank(q)        otherwise

TARGET_MET sorts above every below-target rank.
```

Here `true` means the predicted release-level source, not that downloaded bytes
have been verified. Measured import remains authoritative. Hard language
eligibility precedes this class. Existing baseline verification and audio still
use their actual facts; this definition must not erase don't-churn protections.

Custom-format score and raw source rank do not create extra outcome classes.
They are final preference tie-breakers within equivalent acquisition shapes,
after health/cost. This deliberately prioritizes efficient target satisfaction
rather than blindly preserving today's single-release `better()` ordering.

Re-grab accounting currently keys on the actual wantable ID, so renaming an
episode attempt to a season is not already covered. Proposed new behavior:
count a failed planned download once for each episode it was expected to
supply, never for protected incidental payload. Replace the current
`ListRecentDownloads` (LIMIT 100) cap lookup with an indexed query over the
actual 12-hour failure window; inserting planned rows must not hide failures. Deduplicate by download ID
when migrating/reading legacy history; test that a season label cannot reset
an episode's failure budget. Keep legacy nonplanned accounting unchanged.

### 3.3 Proposed contracts

The pure package `internal/domain/acquisitionplan` imports no clients, database,
clock, HTTP, or logger. Baseline details belong to normalization/explanations.

```go
// Proposed types, not existing source.
type EpisodeKey int64
type CandidateKey string

type Preference struct {
    Class       int // TARGET_MET sentinel or below-target quality rank
    QualityRank int // tie preference only within equal class
    FormatScore int // tie preference only, not replacement permission
}

type Candidate struct {
    Key       CandidateKey
    Pack      bool
    Pref      Preference        // one release-level preference
    Eligible  []EpisodeKey      // authorized fills/improvements
    Payload   []EpisodeKey      // claimed physical coverage, including extras
    SizeBytes int64
    SizeKnown bool
    Protocol  string
    Seeders   int
    SeedersKnown bool
}

type Input struct {
    Episodes   []EpisodeKey     // acquirable targets only
    Candidates []Candidate
    AllowPacks bool
}

type Plan struct {
    Releases  []CandidateKey
    Providers map[EpisodeKey]CandidateKey // predicted best provider, for records
    Unserved  []EpisodeKey                // no admissible supply in this pool
    // Typed outcome/cost comparisons and rejected-alternative summaries.
}

func Build(Input) (Plan, error)
```

Uniform preference is encoded once per candidate. Validate singles have one
episode, packs claim the same known full-season payload, and no candidate has
out-of-scope eligible IDs. Pack eligibility must nest with raw release quality
under current profile rules; equally ranked, equally admissible packs must have
compatible eligibility. TARGET_MET candidates must be interchangeable over
wanted baselines. Keep these as assertions and test the underlying monotonicity
in `internal/domain/quality`; a future policy change must break the test rather
than silently invalidate the optimizer.

## 4. Selection — one pack base plus independently useful singles

### 4.1 Construct the attainable target vector

For each acquirable episode, take the maximum candidate class. No supply leaves
it unserved; neither cost nor coverage licenses a floor/language violation.
Protected baseline files are outside this vector and stay untouched.

```text
normalize and validate all discovered candidates
compute the attainable per-episode class vector

for base in [no pack] + each eligible full-season pack:
    start with base's eligible outcomes
    for every target episode:
        add a single only if its class improves the prospective provider
        choose equivalent singles using the fixed cost/tie ordering
    record predicted providers
    remove releases with no useful contribution
    retain alternative only if it reaches the attainable vector

choose the least-cost retained alternative
```

A pack meeting target cannot acquire better-source/format singles on top.
A below-target pack can acquire higher-class singles. A same-class single
never adds a transfer beside an adequate pack just for a preference point.
The no-pack alternative still allows cheaper equivalent singles to win.

### 4.2 Why this restricted algorithm is sufficient

All supported packs cover the same season and carry uniform preference.
For one profile, a stronger eligible pack can supply every episode a weaker
one supplies; at target, outcome improvement saturates. Two packs therefore
cannot improve the attainable class vector beyond one suitable pack plus
independent singles. Partial/mixed bundles invalidate that argument and are
excluded from automatic v1 optimization.

Expected work is roughly `O(R log R + P × E)` for retained singles R, packs P,
and episodes E. Test small inputs against a brute-force subset oracle. Do not
use brute force in production or replace the per-episode vector with a summed
quality score that trades one episode's quality against another's.

Retain equal-class candidates until comparing health/cost. Lower-class singles
can be excluded from final construction while a higher eligible provider
remains, but retain diagnostics. Failed or invalidated winners trigger fresh
normalization; stale pruning decisions are not fallback authorization.

### 4.3 One fixed, transitive cost comparator

Outcome classes come first. Among plans reaching that vector, minimize:

```text
(known-zero-seeder torrent count,
 unknown-size transfer count,
 total known advertised bytes,
 unknown-seeder torrent count,
 redundant payload episode occurrences,
 transfer count,
 canonical preference/transport signature)
```

The health terms are coarse risk classes, not an availability guarantee.
A reported 0-seeder torrent loses to a positively seeded equivalent before
bytes; missing seeder data is explicitly unknown rather than silently zero.
Unknown seeders break ties only after size uncertainty and known bytes: an
unreported seeder field must not make a 34.6 GiB pack beat a smaller E05 single.
Usenet has neither torrent risk count; this says nothing about actual Usenet
retention. Mark health from bounded discovery, without invented numeric seeder
weights. Add `SeedersKnown` to the adapter contract, because a zero-valued
integer alone cannot distinguish reported zero from absent metadata.

A zero-seeder candidate with uniquely higher class still wins under the
outcome-first policy; this feature does not invent a hard health rejection.
The UI must expose that risk. An eventual hard minimum-seeder gate would be a
separate eligibility policy.

Full pack size counts, including unassigned payload. Redundancy is the sum of
payload episode occurrences minus useful predicted-provider assignments.
Unknown sizes are unpriced transfers, not free transfers: fewer unknowns win
before known bytes. Use checked integer addition. Never change comparison
mode because an unused unknown candidate was added.

Tie signature: sort releases by stable slot (pack or episode ID), then compare
`(-QualityRank, -FormatScore, protocol, -knownTorrentSeeders, candidateID)`
lexicographically after equal transfer count and numeric costs. These final
preferences cannot add downloads to improve an already met class. Stable slot
ordering makes equivalent-single choices deterministic and composable.

When unknown sizes exist, report unknown count plus known subtotal, never
“smallest download.” The 2.10 GB/0-seeder versus 2.11 GB/150-seeder equal-class
single fixture must select the latter. In the incident-shaped fixture with
a healthy 34.6 GiB pack and a reported-zero-seeder E05 single of the same
attainable class, the pack wins: explain “the only E05 single reports no
seeders.” Missing seeder metadata must not produce that explanation or result
when the smaller individual has the same outcome and known size. The original 10 GiB singles versus
100 GiB pack fixture must not reverse because an unused unknown-size candidate
appears.

## 5. Discovery — bounded evidence, explicit partial progress

### 5.1 Search season scope first; use targeted fallback

Keep collection side-effect free: no Add, download rows, imports, or file edits.
For pack-eligible scopes, search season alternatives first and merge both packs
and singles returned. Use current capability-aware alternatives: at most one
ID query and two title queries per scope/indexer; generic fallback when needed.
A query is not identity proof.

Whether a season query returns singles is provider-dependent, not assumed.
After season discovery, issue episode queries for each **missing** episode
whose best individual candidate class is below TARGET_MET or absent. Existing
upgrade-wanted episodes normally use the season response alone. Their targeted
queries are eligible at the seven-day deep-upgrade cadence or with surplus
budget, and only after queued missing work has been served. Record this chosen
query scope; a deliberate upgrade-query omission is not a failed search or
evidence that no better single exists. An acceptable
720p single must not suppress a possible target-quality single. Stop further
alternatives for that episode when an individual TARGET_MET candidate is found;
this preserves target satisfaction but intentionally gives up exhaustive price
comparison. A pack alone never satisfies the individual-query stopping rule:
otherwise a target-meeting pack could again hide a much smaller missing-episode
single. A season response containing suitable singles can avoid those lookups.

Complete the chosen procedure across healthy configured indexers before normal
admission. Pagination is part of an issued query: consume advertised pages
within the limits below or record known truncation. The port must return typed
query/page completeness metadata, which the current `[]Release,error` result
and Torznab response model do not expose. Retry timing already exists in
`ports.RemoteError.RetryAt`, populated from Retry-After: preserve it through
collection and scheduling rather than inventing a second retry field.

Missing pagination metadata means successful bounded response of unknown
exhaustiveness. Known omitted pages or malformed continuation mean incomplete
scope. Keep absence, failure, and truncation distinguishable in explanations.

### 5.2 Proposed operating budgets and arithmetic

These are explicit starting defaults, not measured indexer entitlements. Daily
cap configuration, the request ledger, per-indexer concurrency limiting,
per-indexer RSS due times, and checkpointed discovery evidence are **new work**; Torznab has no existing rate
limiter to reuse. Keep the current call timeout and introduce the limiter.

RSS currently runs every 15 minutes: 96 first-page requests per indexer per
24 hours. Allocate it explicitly. Let B be the configured daily cap C, or a
local budget of 250 requests when C is unknown. The latter is a Curator policy,
not a claim about the provider's account allowance. Compute:

```text
interactive reserve I = min(20, floor(0.10 × B))
RSS allocation R      = min(96, floor(0.50 × (B - I)))
search allocation S   = min(100, max(0, B - I - R))
RSS interval minutes  = ceil(1440 / R), when R > 0
```

| Budget B | Interactive reserve I | RSS R / cadence | Search S per 24 h | Unallocated headroom |
|---|---|---|---|---|
| 250 (unknown-cap default) | 20 | 96 / 15 minutes | 100 | 34 |
| 200 | 20 | 90 / 16 minutes | 90 | 0 |
| 100 | 10 | 45 / 32 minutes | 45 | 0 |
| 50 | 5 | 22 / 66 minutes | 23 | 0 |

Enforce rolling 24-hour totals and bucket limits, not merely these projections.
Every page, retry, RSS fetch, capability request, and wire fallback consumes a
token. Additional RSS pages consume R; search-related discovery/probes consume
S; interactive work consumes I, then unallocated headroom, then borrows unspent
S with the debit recorded against S. Never borrow R or exceed B. Manual searches
have priority over automatic work, even if borrowing defers an open discovery
job; show that deferral and recompute its completion feasibility. Borrowing
reduces automatic capacity, not a second allowance or an overdraft.
Show exhaustion and next eligibility. If a bucket is zero, that category defers;
RSS must not keep polling at 15 minutes while silently spending the search
budget. Provider RetryAt overrides local eligibility. External consumers of the
same account remain outside this ledger and can still cause a 429.

The global 15-minute RSS job becomes a wake-up sweep, not permission to call
every indexer each time. Persist `next_rss_at` per indexer. Dispatch only when
due, under its R budget; set the next due time from actual dispatch plus the
computed interval, and honor RetryAt. Wake at the earliest due time (with a
15-minute maximum idle sweep interval) so a 32-minute cadence does not become
45 minutes. Do not catch up missed ticks in a burst after restart. At B=50,
spread 22 calls roughly 66 minutes apart rather than spending them in 5.5 hours.
Additional pages/retries also spend R and may defer later RSS work; disclose
that delay. This pacing is new scheduler work, not a property of a token bucket.

Separate provider NZB/download-grab caps, including client-fetched NZBs, are
outside this request ledger in v1. Do not claim the API-call budget covers them;
provider rejections still enter ordinary failure/backoff handling.

New storage: `indexer_request_usage(indexer_id, requested_at, bucket, units)`
with a time index and atomic token reservation before outbound I/O. Retain at
least the rolling window plus a cleanup margin. A reserved request that crashes
before sending may conservatively count; do not undercount possibly sent calls.
This scheduler table is separate from the single acquisition-plan table.

Persist versioned discovery checkpoints in `jobs.payload`: normalized bounded
candidate summaries, per-scope/page cursor and completion status, query-policy
fingerprint, timestamps, and next eligibility. Add a lease-checked payload
update method; there is no existing discovery-evidence store. Resume/caching
uses that data, not a second acquisition ownership table. Bound payload size
at 2 MiB per job; overflow makes the affected scope explicitly partial rather
than silently dropping higher candidates. Private candidate transport URLs
stay out of public job/plan diagnostics.

| Limit | Proposed default and meaning |
|---|---|
| Automatic share | One rolling 12-hour spending ceiling of S/2 per indexer across backlog, add/profile-change, successor, and RSS-triggered searches; combine fractional credits until a whole request fits. Unused credits never exceed the rolling 24-hour S ceiling or survive it as banked extra allowance. Interactive borrowing reduces S capacity. |
| Page budget | At most 2 pages per query alternative; known continuation beyond this is partial. |
| Concurrency | One outbound request per indexer, with scheduling between requests. |
| Cooldown | Seven days after a complete bounded no-improvement search. New holes/profile/scope changes invalidate it. A new RSS title alone does not. |
| Evidence lifetime | 24 hours; checkpoint successful scopes and refresh expired facts. |
| Work admission | Missing before upgrade-only; preserve `watchedFirst` within tiers. Prefer finishing a bounded feasible open set before opening new jobs; rotate only within that set. New holes use uncommitted headroom, not another open job's promised completion budget. |

All automatic origins enqueue the same deduplicated season/copy job with the
same priority, budgets, and feasibility test. Add/profile-change, successors,
and improving RSS wake this dispatcher immediately when tokens are available;
they neither wait mechanically for the next 12-hour backlog scan nor get an
independent spending path. The periodic backlog discovers additional work.
Explicit interactive search uses the separate priority and borrowing rule above.

**Admit complete comparisons, not arbitrary query fragments.** Limit the open
set to two season jobs per tier, additionally constrained by the feasibility
calculation below; fewer may fit. A queued job has no evidence clock. Start
its 24-hour evidence clock only with its first request. Reserve scheduling
capacity, not spent ledger tokens, for every open job's remaining bounded
queries across all participating indexers.

Before opening a job, simulate its remaining requests together with open work
against actual rolling token replenishment, the 12-hour ceiling, per-indexer
serial dispatch/timeouts, and each evidence deadline. Use the full chosen-scope
upper bounds, including two pages per alternative until responses show fewer;
require completion at least one hour before evidence expiry. Aggregate daily
averages are insufficient. Store estimated remaining cost and deadline in the
checkpoint, and release unused scheduling capacity as queries finish or stop.
If the estimate fits alone but not alongside open work, leave the job queued;
do not start its clock or call it partial.

Rotate by one query alternative only among admitted jobs whose deadlines remain
feasible; use earliest deadline first when slack narrows. Prefer completing open
work over opening more work. A new one-hole job (up to six first-page requests,
twelve with two pages, per indexer) can jump waiting upgrades only if unused
capacity covers it without expiring open comparisons. This bounds the delay
behind admitted work while avoiding weeks behind unopened upgrade sweeps.

If a job's full chosen-scope estimate cannot fit **even alone** inside the
freshness window, mark it intrinsically budget-limited at admission and use the
partial policy with that explicit reason. Do not manufacture partial searches
by opening more feasible jobs than can finish. Provider failures, advertised
truncation, and RetryAt delays remain genuine partial-search causes. If manual
borrowing or an unexpected latency overrun defeats a prior reservation, pause
and refresh/requeue the affected complete comparison; do not automatically
convert the scheduler's interruption into permission to dispatch singles.

Use season-wide planning for mixed missing/upgrade scopes, but spend individual
fallback on holes first. Opportunistic upgrades in returned season results
remain usable without another query.

Exhaustive deep search costs `(E+1) × 3 × N` first-page requests. For E=20 and
N=3 indexers that is 189 total, 63 per indexer, or 126 per indexer with two pages. At
S=100, throughput is about 1.59 deep seasons/day (0.79 with two pages), only
11.1/week before other searches. At B=100, S=45 gives 0.71 first-page deep
seasons/day. These are upper bounds, not promised completion rates.

An upgrade-only season pass is instead up to 3 first-page requests per indexer:
about 33/day at S=100, before other work; deep episode queries wait for their
cadence/surplus. One new hole plus a season query costs at most 6 first-page
requests per indexer: about 16/day at S=100. These figures explain why priority
and reduced upgrade fanout are required under the Best profile.

A 63-request season can span two 50-token spending windows while respecting
its own evidence deadline. A 126-request case cannot fit at S=100 within a
24-hour lifetime and is intrinsically budget-limited. A normal budget yield
inside a feasible reservation only resumes; it dispatches nothing. Expired
evidence due to queue interleaving is refreshed, not treated as a provider
failure or a singles-only decision.

Acceptance example: four fully missing ten-episode seasons, three indexers,
S=100, all successful one-page responses and no TARGET_MET early stop. Each
season costs 33 requests per indexer, 132 total per indexer. Never open all four
at once. With idealized 50-token batches at t=0,12,24 hours, a completion-first
schedule spends 33+17 at t=0, 16+33+1 at t=12, and the final 32 at t=24. The
last season finishes at t=24, not t=12. If conservative two-page admission
bounds delay opening season four until t=24, it can instead spend all 33 then;
that also passes. Each opened season completes inside its own freshness window,
and all four compare packs before dispatch. Waiting in the queue is not partial.
Use fake-clock tests with request durations/margin, not equality at expiry.

RSS invalidates no-improvement cooldown only when its normalized eligible
outcome raises an acquirable episode's class above the best known provider
(including baseline/active predicted supply). Lower quality, re-encodes,
format-only improvements, and identical results do not. Merge improving rows
into fresh checkpoint evidence and replan from it before issuing new queries.
A new hole or profile change still triggers discovery independently of RSS.
Measure provider season-query coverage and request costs in read-only validation.

### 5.3 Incomplete searches may make limited progress

Paul approved the asymmetric policy. Compute a preview from returned evidence,
then constrain admission:

- A single can proceed for its exact eligible episode after that episode's
  configured active-indexer scopes complete or stop at TARGET_MET. It does not
  establish the absence of releases for other episodes.
- A pack cannot be justified by failed/missing single scopes. A necessary-pack
  explanation requires successful bounded absence for the relevant gaps. An
  efficiency-selected pack requires completed comparable scopes, or the
  deliberate individual TARGET_MET stopping rule in §5.1; disclose which rule stopped
  discovery. Discard any incomplete alternative whose missing evidence could
  be the sole reason for choosing its pack.
- Cancellation submits nothing. Budget yield resumes; it is not a search error.
- Limited singles progress can be duplicated by a future pack discovered after
  recovery. This is an explicit availability/efficiency tradeoff, not the same
  guarantee as normal complete planning. Do not submit singles already proven
  redundant by a fully justified selected pack.

A 429 sets the indexer's next eligible time from `RetryAt` and yields discovery
until then. This requires an actual scheduler change: current `Queue.finish`
only computes its own backoff. Introduce a typed deferred-until result that
schedules at the later time without burning the transient retry budget while
waiting. Ordinary transient errors retain the three-attempt queue policy.

After three actual failures, mark the indexer temporarily degraded and exclude
it from required scopes for that scheduler epoch, visibly recording the reduced
provider set. Probe recovery no earlier than `RetryAt`, or one hour for a
transient outage without a provider delay. Authentication errors remain
configuration-visible; they do not veto healthy providers indefinitely.
Expired retry budgets may start a new epoch on the next scheduled backlog day,
not on each duplicate RSS event. Never silently equate an excluded indexer
with complete coverage of all configured providers.

## 6. Durable execution — one plan table, existing download rows

### 6.1 Minimal records and authority

Add `acquisition_plans` with ID, item/copy/season, state, revision, policy
version, snapshot fingerprint, immutable decision JSON, and timestamps. A
partial unique index on item/copy/season for active states is the database
scope claim. Do not add a separate scope table.

Active scope states are `admitted`, `dispatching`, `active`, and
`cancel_requested`. Scope-releasing states are `completed`, `cancelled`,
`needs_replan`, and `settled_with_reservations`. The last releases the season
claim while durable download rows reserve only their affected episodes; it
must not be displayed as all downloads complete. `needs_review` is a row/job
condition, not an indefinite season-wide scope lock. Transition atomically
with preserved episode reservations and successor scheduling (§6.4).

Extend downloads with nullable `plan_id`, stable candidate key, and submission
phase (`pending`, `submitting`, `submitted`, `uncertain`, `rejected`). Existing
transfer identity column is populated before Add by new persistence code;
today SetDownloadHandle populates it only after Add. Add a unique plan/candidate key
constraint. These rows are the selected release intents; no attempts table.
Store transport details only in private execution fields, never explanation
JSON. Legacy rows keep existing behavior.

Persist the selected URL/client/options needed to resume pending work in a
private download execution payload. Migrate existing rows to legacy submission
semantics without re-enqueueing them. Add a distinct `downloads.state=planned`
for unsubmitted rows; rebuild its CHECK constraint and update generated SQL.
Move to `grabbed` atomically with the submission-phase claim before Add. Planned
rows are reservations, not client jobs; failure caps exclude them.

Audit every reader, with explicit opt-in rather than broadening all client-job
queries:

| Reader | Planned/parked treatment |
|---|---|
| `wanted.go:notInFlight`, `graburn.go:alreadyInFlight` | Include planned and parked reservations in a separate reservation query; same-copy episode masks and duplicate titles remain protected. |
| `acquisition.go:Grab` allocation | Check reservations as well as actual client jobs; consume the exact pending row for planned dispatch. |
| `acquisition.go` polling, `subscribe.go` client event matching | Exclude planned; include possibly submitted parked rows for reconciliation, never resubmit them. |
| `importers.go` | Exclude planned; only actual completed payloads enter import. |
| `completed_delete.go`, both `retained_plan.go` active lookups | Do not act on planned rows as payloads; include reservation/custody evidence when protecting shared files from deletion. |
| Queue/Activity/count/history/retention | Display planned and parked distinctly; preserve unresolved custody. Never infer a missing client job from a planned row. |

These cover the nine existing ListActiveDownloads call sites; audit additional
readers at implementation time. Do not redefine that method so an overlooked
caller treats a reservation as a real job.

Decision JSON contains expected provider IDs and an immutable per-release
import allowlist. The latter is a narrow safeguard described in §7, not a
mutable assignment ledger. New decisions receive a new plan after the old
scope has reconciled or narrowed to durable episode reservations; no in-place
reassignment revisions or retained-pack custody. Keep execution phase separate from imported/downloaded state.

### 6.2 Admission and dispatch

1. Collect candidates against a versioned snapshot without a long transaction.
2. Revalidate files, profile/custom formats, identity, monitoring, failure caps,
   holds, and destination/client/indexer configuration. Check the complete
   plan's storage requirements, including extraction/placement temporary space.
3. In one short transaction, validate the relevant durable revisions and insert
   the scope-unique plan plus every pending download intent. Changes between
   snapshot reread and commit must fail the conditional admission.
4. One service worker owns dispatch under existing process serialization.
   Claim each pending row conditionally before Add; check cancellation and
   changed eligibility at that point. Only the successful claimant calls Add.
5. Release database locks before network I/O. Persist handles/status normally.

This is row-first submission made explicit. `Grab` already checks duplicate
release titles and held overlap; it does not itself block a planned single
merely because a pack exists. Do not add an ignore-duplicates boolean. Refactor
its shared submission operation to consume the preallocated row. Other
automatic callers must respect the active plan scope before collecting/grabbing.
Manual choices still use their explicit path and reconcile conflicting work.

### 6.3 Recovery and cancellation boundaries

Do not claim exactly-once client submission. The existing ports cannot return
all transfer IDs; qBittorrent Add may have an empty handle, and a lost Add
response is not proof that nothing started.

- Persist transfer identity before calling Add. Reconcile by handle when
  available, then a unique client-scoped normalized title match. Never attach
  ambiguously matching jobs merely because their titles are alike.
- Restart can submit rows still `pending`. A row left `submitting`, or an Add
  returning an ambiguous transport error, stays reserved as `uncertain` until
  reconciliation, the explicit seven-day risk policy in §6.4, or operator review.
  Do not delete evidence or treat an ambiguous Add as a confirmed rejection.
- Transfer-echo support in client statuses would improve reconciliation, but
  requires explicit port/adapter work; it is not a hidden prerequisite or an
  invented existing capability. Do not add it unless needed for the supported
  clients' acceptance tests.
- Cancellation wins before the conditional submission transition. Afterwards,
  treat the job as possibly accepted and reconcile; cancel does not silently
  delete client payload or imported files.
- Derive completion from reconciled terminal outcomes. Alternatively settle
  the plan with explicit episode reservations under §6.4, allowing a successor
  for unrelated work. Missing rows never count as success. Interrupted admission
  with no submitted work can become `needs_replan`. Active or uncertain work
  remains reserved, never a fresh hole.
- A terminal failed or mismatched episode returns to ordinary wanted planning.
  Existing successful library files are baseline; they are not fetched again
  unless the profile actually still wants an improvement.

No new history-retention subsystem is needed. Keep one guard: active/uncertain
plan execution rows and unresolved placement/hold evidence cannot be deleted
by dismissal, Clear failed, or retention. Presentation may hide them. Durable decision JSON preserves the explanation; only actually reconciled
terminal rows can use ordinary retention. A scope-releasing plan state does
not authorize deleting its parked rows. No retained-fallback payload references
or restrictive assignment foreign keys are introduced.

### 6.4 Bounded season claims and automatic replanning

Release a season claim without pretending uncertain episodes are available.
Persist per-download reserved episode IDs and a parked reason/time alongside
existing execution metadata. The default reservation is the immutable import
allowlist; holds, unknown coverage, or an indivisible multi-episode payload
reserve the full affected coverage conservatively. Never narrow an operator's
hold. A successor treats reserved episodes like protected baseline files:
outside its target vector and absent from its pack import allowlist. A parked
single alone does not prohibit a pack for the rest of the season; its incidental
bytes are redundant cost, not another import authority. An explicit hold,
unsafe overlapping placement, unknown coverage, or duplicate release identity
still fences the affected work conservatively. A multi-episode file crossing a
reservation follows §7.3. Preserve reservations when later client events arrive.

| Condition | Automatic action and claim boundary |
|---|---|
| Torrent without byte progress | After 48 observed hours with zero connected seeds or known availability below 1, or seven observed days of no progress regardless of seed counts, retire as below and replan. |
| Add remains uncertain | Poll automatically; the approved seven-day successful-no-match policy below can later release the episode with duplicate risk recorded. After at least three successful complete inventories spanning 30 minutes without a unique match, park the row and settle the season claim; reserve its episodes, not the whole season. Client outage cannot extend the season claim indefinitely: after 30 minutes wall time park with unavailable-client evidence and continue backoff reconciliation. |
| Import needs review | Park affected episodes/payload; once current publication is resolved or safely fenced to those targets, settle the scope and replan other holes. Do not permit a multi-episode file to evade the full affected reservation. |
| Any active plan reaches 48 hours | Settle to episode reservations for remaining legitimate work, even if it is progressing or paused; do not fail or cancel a healthy/manual-held transfer just because the planning scope ages. |

Close never-submitted planned intents when settling, without charging failure
caps; requeue their needs rather than leaving an unnecessary parked reservation.
Keep possibly submitted rows reserved.

The 30-minute rule does **not** prove rejection. Client listing may lag, omit a
completed job, or reflect changed category/name. Confirmed-absent requires
client-specific authoritative nonacceptance or confirmed removal. Title dedupe
cannot guarantee no duplicate if the original appears after the retry starts.

**Seven-day uncertainty policy — approved by Paul:** after seven consecutive
days of complete successful client inventories with no handle/identity or
unique name match, release the affected episode reservation and enqueue one
fresh planning attempt. Require an unchanged client/category configuration,
no gap longer than six hours in successful inventories, and a final inventory
within five minutes of the decision. A gap or configuration change restarts
this evidence window; failed polls are not absence evidence. A match or
ambiguous multiple match cancels automatic risk release and goes to normal
reconciliation/review. User holds/cancellations never expire under this policy.

Record `retry_with_duplicate_risk`, the evidence window, original download ID,
and successor linkage; show a persistent Activity warning before/alongside the
retry, without requiring another confirmation. This is accepted risk, not
`confirmed_absent` or a release failure: do not blocklist the release or spend
a failed-release cap solely because Add remained uncertain. Normal discovery
budgets and scope/episode eligibility still apply.

Atomically mark the old row superseded for import, release its episode claim,
and persist a deduplicated successor enqueue. Retain its identity/evidence for
late reconciliation; do not delete it just because the broad plan is terminal.
A late match must not silently authorize the superseded job to import alongside
its successor. Reconcile duplicate custody and show the warning; only a current
non-superseded provider can publish automatically. Restart cannot enqueue a
second risk retry for the same original row. A newly submitted successor can
start its own uncertainty window if necessary, never an immediate retry loop.

Stall detection is new port/adapter/service work. Expose optional bytes
completed, **connected** seed count, availability, and operational state.
For qBittorrent these are separate from the tracker scrape's `num_complete`:
use `num_seeds` and `availability`, not a stale claimed swarm count. Persist
progress high-water/time and two observed no-progress durations on the row.
The short timer needs connected seeds zero **or** known availability < 1. The
seven-day backstop needs observed no byte progress, regardless of seed counts.

Count only fresh successful eligible observations with gaps no larger than
five minutes. Any byte increase resets both timers; sufficient availability
and connected seeds reset the short timer, not the long timer. Pause, queue,
check, hold, or outage pauses accrual; unknown seed telemetry cannot satisfy the
short timer, but known unchanged byte telemetry can accrue the long timer.
Unknown byte telemetry cannot accrue either. The existing 30-second poll is
sufficient when healthy. Two distant percentage samples prove neither rule.
Unsupported clients retain scope settlement without invented stall evidence.

At timeout, reread fresh status under the row lock. For a Curator-owned partial
payload proved unimported and unshared with library/recovery/other jobs, remove
the exact handle with `deleteData=true` and verify retirement. Otherwise remove
with `deleteData=false` and keep an explicit cleanup-pending custody record on
the original download row; the existing completed-payload cleanup coordinator
must be extended to reconcile this partial-payload cleanup. It validates paths,
references, and file identity before deletion, and exposes retained files for
review if safety cannot be proved. Do not leave orphan bytes or recursively
remove a shared directory based only on a client path.

Only after the old client job is confirmed inactive, call ordinary failure once,
blocklist, charge its failure budget, and allow replacement. Cleanup-pending
rows survive history retention even when network replacement is safe. Uncertain
removal parks the episode instead. User-paused jobs and Runner holds are not
automatically failed or deleted by this timer.

A terminal/settled transition transaction enqueues a deduplicated season job
when fresh facts have unreserved acquirable work, through the existing jobs
store and within §5 budgets/priorities. An episode failure while its plan is
active records `replan_pending`; explain “waiting for the remaining plan work.”
The settlement timer bounds that wait. If all unmet work is reserved, do not
spin a successor job; reservation release or a new unreserved hole triggers it.
A periodic reconciler repairs missed enqueue intents after restart. Successor
jobs do not reset indexer retry budgets or bypass cooldown for upgrades only;
new holes and actual failed acquisitions follow their explicit retry policy.

## 7. Import — protect existing files without a second importer

### 7.1 Three shared fixes, before enabling broader pack eligibility

1. Honor `UpgradesAllowed` for automatic replacement. `Profile.Upgrade` alone
   does not read that flag. A pack acquired for holes must preserve protected
   existing files, even if its payload is better.
2. Treat `HasFile && Have == nil` as protected unverified data, not missing.
   Skip automatic duplicate placement beside it. Keep manual behavior explicit.
3. Probe and validate measured quality/audio/coverage before publication and
   before authorizing removal. Returning measured quality only after
   `commitPlacement` is too late: it may already have renamed over a target.
   Refactor a shared prepare/validate/publish seam, keeping the existing
   placement journal and digest safeguards. Recheck live files under the
   existing import lock. Apply the shared safety fix to movies/books as well
   where the same claim-based removal pattern exists.

A claimed high-quality file measured lower cannot delete a better existing
file unless current measured language-correction policy genuinely authorizes
that replacement. Unknown evidence is not permission to destroy a known file.
Partial/multi-episode files that overlap protected episodes are indivisible:
review rather than silently replacing one protected sibling to fill another.

### 7.2 Why saturation alone does not eliminate all ordering hazards

The review's rank-only convergence argument is useful, but has two limits:

- With upgrades disabled, both releases can be eligible while an episode is
  missing. If a 1080p pack imports first, the required upgrades-off fix blocks
  the selected 2160p single. If the single imports first, it remains. This
  diverges without any above-target transfer.
- A below-source-target pack can measure at target resolution with unverified
  source. `Profile.Met(current, false)` then stops hunting under ADR 0013,
  even though planning used `Met(claim, true)`. This can occur when a
  contradicted release claim resolves with medium-confidence probe provenance;
  an ordinary uncontradicted release claim remains source-verified. A later source upgrade can be
  blocked. Do not evade that safeguard to make a theoretical merge converge.

Use a static import allowlist for mixed plans: the pack imports only episodes
for which it is the planned provider; each better single imports its own
selected episode. Protected baseline siblings are absent from both lists.
Store these immutable lists in decision JSON and derive them before Add.
At import, filter payload by that list and then apply all live/measured checks.
This avoids transient pack placement in the selected single's slot for every
profile without a mutable assignment table or special upgrades-off exemption.

The immutable planned allowlist can only narrow normal permission; the
terminal-failure fallback below is separately derived from durable facts.
A later manual file, profile edit,
hold, or unverified file may still block import. Do not mutate allowlists
while placements are pending. Normal journal recovery still owns filesystem
safety; this feature does not create distributed assignment fencing.

### 7.3 Failure and payload mismatch

A pack is a coverage claim. Missing finale, wrong numbering, or unexpected
measured quality records a claim mismatch, not “episode unavailable.” After
reconciliation, unmet episodes return to ordinary wanted discovery.

At pack import, derive an effective allowlist under the import lock: immutable
pack providers plus episodes whose selected single is already terminal
`failed` or confirmed `rejected`, with no live/uncertain submission, successor
reservation, manual cancellation, hold, or replacement owner. Apply current
measured policy to every added episode; the pack's quality must still be an
acceptable fill/improvement. Check and reserve these effective episode targets
against successor admission in the same reservation transaction before staging,
then recheck live files under the import lock; a successor cannot claim the hole
between the fallback check and publication. Record the fallback and failed row
ID in the placement evidence. This is a one-time decision from durable terminal facts,
not mutation of the plan or permission to resume the failed single.

An indivisible physical file is importable only if every episode it covers is
in this effective allowlist and passes live protection checks. If it crosses
providers or protected files, skip/review it and record a coverage mismatch for
its otherwise-assigned episodes, then replan them when custody permits. Do not
import a pilot spanning E01–E02 to fill only E01.

If the single fails only after the pack was cleaned up, fallback bytes are not
guaranteed. Replan that hole and reuse payload only if ordinary custody still
owns it; another transfer may be needed. No retained-pack payload subsystem.

Normal cleanup applies after every permitted payload file is reconciled.
An unassigned pack file is not imported merely because it exists. Expected
quality is a prediction, measured library facts determine target satisfaction.

## 8. Explain the actual decision and keep actions visible

Persist scope/trigger, snapshot counts, target/floor/language, policy version,
searches/pages/errors/budgets, degraded indexers, candidates compared, chosen
releases, expected providers, import allowlists, unresolved gaps, and decisive
outcome/health/cost facts. Retain bounded alternative summaries, not secrets
or unbounded indexer dumps. Never backfill reasons for historical rows.

Reason examples:

> Chose the season pack because it supplies three episodes with no acceptable
> single in this bounded search. Three better singles fill episodes where the
> pack is below your target. Existing protected files will be kept.

> Chose the 15 GB pack because it meets your 1080p target for all ten episodes.
> The 250 GB Remux singles exceed that target and would not improve satisfaction.

> Acquiring E05 from completed searches. One indexer is rate-limited; pack
> comparison is deferred. Other releases may be found after the retry.

Use `ActionDialog` for Why chosen and explicit search results per current
[CLAUDE.md](../CLAUDE.md). Keep searching, deferred-until, partial progress,
downloaded, imported, and needs-review results in the viewport. Focus moves
into opened work and returns on dismissal. Background polling cannot move
scroll/focus. Separate Identity match from Selection reason. All selected
download rows link to the same plan explanation. Add summary/detail to OpenAPI
and regenerate web/mobile consumers; verify phone and desktop geometry.

## 9. Build sequence — one reviewed PR, coherent commits

Follow the current repository rules: independent clone outside `~/code`,
`codex/` branch, one batched PR, status page, review when ready to merge, then
one final gate. Accepted ADRs are immutable. Write a new proposed ADR for
selection policy; verify the next number/migration at build time (0024/0038
were the next candidates at the inspected baseline).

| Commit-sized milestone | Required observable evidence at final validation |
|---|---|
| A: policy/domain | Saturated outcomes, uniform candidate types, cost ordering, normalization invariants, brute-force oracle fixtures. |
| B: safe importer | Upgrades-off, unverified-file, measured-before-publication fixes and immutable allowlist tests. This is prerequisite work within the same batched PR. |
| C: discovery/scheduler | Feasible open-set admission and shared origin queue; missing-first/watched priority; season-first upgrades; new pagination, reuse of RetryAt; caps/ledger/limiter/checkpoints and per-indexer RSS due times; interactive borrowing and cooldowns. |
| D: admission/execution | One execution-plan table, planned-state migration/readers, conditional submission, uncertain-row parking, stall telemetry/retirement, bounded scope settlement, and durable successor enqueue. |
| E: integration/UI | Every automatic entry point rerouted; expected outcomes and real reasons on Activity, visible actions, OpenAPI/mobile changes. |
| F: final build handoff | Updated VERSION/usage/settings/status, final adversarial review and gate, PR/merge state and workspace cleanup. Deployment is outside this plan. |

Milestones organize commits and evidence, not repeated test runs. Read-only
shadow collection is a development/validation technique, not a production
feature flag. Ship the completed behavior enabled; any unavoidable enable
control follows the Dev-settings requirements in current CLAUDE.md.

Paul explicitly excluded deployment on 2026-10-01. There is no server or client
deployment work in this plan. The current deliverable is documents only.

This document revision is not authorization to implement, acquire media,
change live configuration, deploy, or merge. The implementation handoff should
retain the same one-PR scope unless Paul explicitly changes it.

## 10. Acceptance matrix and final gate

| Area | Required fixtures |
|---|---|
| Coverage | 5/6 single; 3/15 pack; mixed pack plus higher singles; protected three versus upgrade-wanted three; no supply without policy violation. |
| Saturation | 15 GB target pack beats 250 GB above-target singles; format-only/above-target singles never add to an adequate pack; better below-target episodes still win. |
| Cost/health | 0 versus 150 seeds; unknown seeds cannot force a 34.6 GiB pack over a small single; reported zero can, with correct explanation. Outcome before known-zero risk, unknown seeds after bytes; transitivity and pool-invariance. |
| Domain | Real `Profile.Upgrade` monotonicity across targets/floors/languages/verification; TARGET_MET interchangeability; reject unsupported payload shapes. |
| Import | Both completion orders with upgrades on/off, source confidence, language corrections, existing unverified siblings, and multi-episode physical files. |
| Measurement | Claim-high/measure-low rejected before rename/removal; valid language correction preserved; live file change and crash recovery retain existing protections. |
| Failure | Missing payload returns wanted as mismatch; single fails before pack imports → derived safe fallback; fails after cleanup → replan. E01–E02 crosses allowlists → no partial import. No aggregate season re-search. |
| Discovery | Season responses with/without singles; target stopping versus below-target fallback; pages and malformed/unknown pagination. Permutations preserving the same successful scope set/stop policy preserve attained class vectors, not necessarily winning release IDs or cost. Pure Build alone guarantees winner invariance for the same normalized pool. |
| Partial progress | Singles can proceed from completed scopes; failed scopes cannot justify a necessary pack; degraded indexer exclusion visible; cancellation submits nothing. |
| Scheduler | Four E10 seasons complete four pack comparisons without artificial partial; queued work has no evidence clock; full two-page estimates, one-hour margin, open-set deadlines, shared origins, and no carryover burst. New holes use headroom. Manual borrowing defers automatic work without fabricating partial; B50 RSS stays paced all day. Genuine oversized jobs and provider failures retain partial behavior. |
| Execution | Planned-reader opt-in, snapshot/CAS/crash/cancel; uncertainty parks at 30 minutes and retries once after seven valid days with warning. Outage/configuration change resets evidence; user hold does not expire. Crash at risk-release/enqueue and late original arrival cannot double-enqueue or double-import. |
| Liveness | Stale tracker seeds with zero connected seeds/low availability triggers 48h retirement; seven-day no-progress backstop; byte progress resets both. Pauses/outages do not accrue. Data deleted only for proven unshared partial payload or owned by explicit cleanup record. Parked single permits safe successor pack; holds and crossed multi-episode payload remain fenced. |
| Retention | Active/uncertain intents and unresolved placements survive dismiss/Clear failed/retention; reconciled terminal rows can expire without erasing decision history. |
| API/UI | Old rows say not recorded; JSON has no credentials; mobile/web explanations match actual choices; viewport geometry and keyboard focus for actions. |

Pure properties: input permutations and duplicates do not change stable winners;
all selected releases contribute; every target provider is eligible; no input
mutation; the comparator is transitive; exhaustive small supported fixtures
agree with the restricted optimizer. Tests must independently exercise policy,
not merely replay the implementation's loop.

After the implementation PR is ready and adversarial findings are addressed,
run the required gate once with the pinned toolchain:

```bash
GOTOOLCHAIN=go1.25.7 make lint       # pinned lint/format checks
GOTOOLCHAIN=go1.25.7 make test       # unit/integration and planner oracle
make test-web                      # UI unit tests
GOTOOLCHAIN=go1.25.7 make test-e2e   # browser acceptance and viewport checks
```

Reverify the toolchain in [go.mod](../go.mod). Repeat checks only for failures,
review fixes, or substantive changes invalidating results. Do not claim the
runtime gate ran for this documentation-only revision.

## 11. Remaining engineering review questions

1. Confirm static allowlists are the smallest sufficient protection for
   upgrades-off and source-confidence ordering; do not assume rank-only
   convergence covers those cases.
2. Measure season query coverage and page usage on configured providers.
   Confirm proposed caps/cooldown and configuration presentation without
   pretending arbitrary daily allowances are known provider entitlements.
3. Test ambiguous-Add behavior, the approved seven-day risk retry, and late
   original arrivals. The risk warning must survive restart; a retry does not
   turn absence evidence into proof of nonacceptance.
4. Verify the outcome-first health policy: a uniquely better zero-seeder
   candidate remains eligible. Changing that requires an explicit hard gate.
5. Verify schema and migration numbering, every legacy caller, API generation,
   and the exact current CLAUDE.md before building.

The target-saturation, limited-progress, and seven-day uncertainty-retry
product decisions are settled.
Implementation correctness and operating-budget defaults still require the
specified evidence; this proposal does not claim that source code exists yet.
