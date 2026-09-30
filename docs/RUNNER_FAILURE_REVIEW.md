# Download failure review — adversarial findings and dispositions

**Status:** revised proposal reviewed · **Reviewed:** 2026-09-30

Companion to the [root-cause and fix plan](RUNNER_FAILURE_FIX_PLAN.md) and
[incident evidence](RUNNER_FAILURE_DIAGNOSIS.md). This records an independent
agent's source-backed challenge of the proposal, the resulting changes, and
what the review does not establish. “Addressed” below means addressed in the
proposal; the Runner fixes are not implemented or deployed.

## Verdict and review method

The separate `adversarial_review` agent inspected the complete draft, the
archived Runner checkout, and the diagnostic harness. It sent preliminary
source findings, reviewed the full proposal, and reread the amendments after
the primary agent revised the document. It used the engineering code-review
skill's correctness, concurrency, and security checks.

Its final verdict was:

> The revised proposal has no remaining design blocker identified in this
> review. It is suitable for implementation planning, not approval to deploy
> or recover/delete live media. The required compatibility, crash, integrity,
> and actual-mount tests remain release gates.

The reviewer did not edit files, mutate production, independently repeat the
host probes, or run a full release test suite. The source is the host checkout
at `ab25d0cbecfdb5adf299072813a2948383192d9f`, not a proven build identity of
the running `0.2.0+unknown` binary. Source references below are relative to
that checkout unless explicitly identified as Monarr.

## Findings that changed the proposal

P1 means a high-priority correctness or data-loss risk in the proposed fix.
P2 means a contract ambiguity that could produce incorrect recovery behavior.
These priorities describe the challenged designs, not a newly measured
severity for every live download.

### R1 — P1: creating PAR parent directories is insufficient

`rename.rs:112`, `par2.rs:42,160`, and `tools.rs:253` expose one-value prefix
hash matching, merged recovery sets, basename/full-path disagreement, and
shallow archive discovery. A directory-only fix leaves these stages inconsistent.

**Disposition:** addressed in plan §5.1. The proposal includes validated PAR
set identity, normalized relative paths, a bounded recursive inventory,
collision handling, and consistent verification/cleanup. M2 tests independent
sets, repeated names/prefixes, unsafe paths, and crash recovery.

### R2 — P1: extraction can erase other files under a shared parent

`manager.rs:2163` recursively deletes an existing extraction destination
directory. Two archives emitting the same parent can erase earlier output or
archive inputs. This is a source-backed hazard; it is not proven to have caused
the observed Monster failures.

**Disposition:** addressed in plan §5.1. Each extraction has an owned generation
and output manifest. Publication merges files only after collision checks,
retains source volumes, and never recursively removes a destination directory.
M2 explicitly tests archives with a shared parent.

### R3 — P1: requiring an intact digest before repair excludes repairable files

The initial full draft required full-file digest identity before restoring
catalog paths. A file corrupted after its first 16 KiB could never reach the
repair pipeline, even with sufficient parity.

**Disposition:** addressed in plan §5.1. Intact identity and repairable input
mapping are separate paths. Originals remain intact while isolated repair
workspaces use PAR block verification to test mappings. Repaired output must
match full size/digest before publication. M2 tests damaged nested inputs,
sufficient parity, and ambiguous mappings.

### R4 — P1: extractor-only fixes miss permanent writer failures

`owner.rs:1095,2521` terminalizes remaining segments after writer errors.
`writer.rs:146` and `owner.rs:2623` can finalize a failed file after detecting
exhaustion. Retrying post-processing alone cannot recover episode 5.

**Disposition:** addressed in plan §3.4 and §5.3. The proposal requires durable
segment coverage and expected size, resumable write/finalize states, and
same-job recovery. M1 injects exhaustion during write, sync, finalization,
extraction, and relocation, including restart.

### R5 — P1: free-space readings can resume a persistent quota failure

The initial draft did not define “valid capacity recovery.”
`owner.rs:3227` can clear the observed-exhaustion latch from aggregate free
space even while the writer's quota remains exhausted.

**Disposition:** addressed in plan §5.3. Holds persist by filesystem, cause,
and quota context. Release needs capacity admission and bounded write/flush
probes; `EDQUOT` additionally needs authoritative quota headroom or explicit
operator release. Retry limits survive restart. M1 tests ample `statvfs`
headroom alongside quota failure, failed probes, and recurring exhaustion.

### R6 — P1: a portable publication fix must preserve reader and writer safety

Checking `exists()` before ordinary rename creates an overwrite race. Moving
to registry publication also changes path and discovery contracts; a dot-prefix
alone cannot hide incomplete files from readers.

**Disposition:** addressed in plan §5.4. Unsupported publication first holds
before an expensive copy. Existing reviewed/running operations are reconciled
before another attempt. The proposed generation fallback requires consumer
discovery isolation, actual-path propagation, durable registry state, and
identity revalidation. M3 tests collisions, remounts, publication crashes,
and source retirement. The fallback remains conditional on those gates.

### R7 — P1: blameless failure still triggers replacement acquisition

Monarr's `acquisition.go:1178` re-searches after failure even when blocklisting
is skipped. A new unknown hold enum, failed event, or failed history row could
also cause a legacy consumer to replace the download.

**Disposition:** addressed in plan §4.2. Holds require an existing nonterminal
wire representation, persistent suppression across all automatic acquisition
paths, and no failed completion event for held jobs. Old/new client combinations
must be tested. Choosing the exact compatible representation is still an
implementation/release decision, not an already verified behavior.

### R8 — P2: a verified copy can contain invalid media

The initial draft's “ready/published” terminology could treat faithful custody
transfer of failed archives as successful media validation.

**Disposition:** addressed in plan §4.1. Custody publication and media eligibility
are independent. Failed/held job semantics survive relocation. M3 explicitly
tests that a successfully parked failed PAR payload appears in recovery preview
without triggering ordinary automatic import.

### R9 — P2: an active inventory is not authority for later mutations

An inventory captured while writers or the user's import are running can be
stale by the time a repair or deletion starts.

**Disposition:** addressed in plan §7. Preliminary inventory is labeled
provisional. Mutations require an authoritative snapshot under the relevant
ownership leases after quiescence, with identity and receipt revalidation.

## Additional review conclusions

The filename proposal avoids a parser-only fix: yEnc names remain untrusted,
stable file IDs drive writers, conflicting metadata enters review, and
publication includes collision and restart behavior.

The reviewer confirmed that the following limits remain explicit:

- The deployed binary is not reproducibly tied to the inspected checkout.
- Four matching PAR prefix samples, apparent file lengths, and Matroska headers
  do not establish full-file or whole-season integrity.
- The first HONE extraction's disk-full cause is an inference; its extractor
  stderr is missing. The replacement's write/finalize ENOSPC is explicit.
- Four narrow diagnostic failures do not establish Runner release readiness
  under its locked dependency graph.
- The generation fallback is conditionally viable. Until every consumer and
  storage boundary passes its gates, unsupported publication remains held.
- Historical measurements do not establish a current cleanup estimate or the
  final outcome of the user's Monster import.

No unresolved design blocker was identified after amendment. Implementation,
native-mount validation, compatibility tests, integrity checks, and any
separately authorized recovery/cleanup remain outstanding work.
