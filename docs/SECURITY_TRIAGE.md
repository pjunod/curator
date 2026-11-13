# Security triage — findings, boundaries and remaining decisions

**Snapshot:** 2026-11-13 · **Base:** `7fadefc66f53c6c620fb65d51a0483c015843039`

Companion to [SECURITY_CHECKS.md](SECURITY_CHECKS.md) (ongoing checks) and
[PIPELINE_CONTRACT.md](PIPELINE_CONTRACT.md) (authoritative custody and
publication rules). This records the first complete alert triage after
security scanning was enabled. Review the linked PRs and their final CI;
local validation is not a replacement for successful hosted checks.

## Snapshot and priorities

The GitHub code-scanning API returned **157 open alerts across two pages**:
142 CodeQL findings and 15 Scorecard findings. Full Go/JS SARIF dataflows and
the artifact from [Scorecard run 43453396308](https://github.com/pjunod/curator/actions/runs/43453396308)
were inspected. Counts describe this snapshot, not a promise about later
scans. No alerts were dismissed and no scan rules or severity gates changed.

| Finding family | Count | Assessment and action |
|---|---:|---|
| Go path injection | 83 | Two library sinks expose a confirmed containment defect, fixed in [PR #149](https://github.com/pjunod/curator/pull/149). Other sinks require the boundary-specific assessment below. |
| Go log injection | 52 | Structured `slog` fields reach escaping text/JSON handlers. [PR #148](https://github.com/pjunod/curator/pull/148) verifies that boundary and fixes a separate query-credential disclosure found during review. |
| Go sensitive-data hashing | 3 | One real password-storage defect, fixed with migration in [PR #147](https://github.com/pjunod/curator/pull/147). Two configuration/cache fingerprints are not password authenticators. |
| Go cookies without Secure | 2 | PR #147 adds TLS/trusted-proxy handling while preserving LAN HTTP. Conditional support may remain visible to static analysis. |
| JS log injection | 1 | Test fixture diagnostics now use JSON in PR #148. |
| JS missing URL regexp anchor | 1 | The test now anchors HTTPS, IMDb host and title path in PR #148. |
| Scorecard PinnedDependencies | 10 | Nine external Docker `FROM` references receive verified multi-platform digests. One reference names a local build stage, `media-runtime`, and cannot receive an external image digest. Actions were already SHA-pinned. |
| Scorecard Vulnerabilities | 1 | The original aggregate listed 102 advisory IDs. The fresh dependency audit and remediation are described below. |
| Scorecard SAST | 1 | Scanner coverage history: 3 of the sampled 30 commits had SAST. CodeQL now runs on main and PRs; a rolling history score is not a missing-workflow defect. |
| Scorecard BranchProtection | 1 | Repository policy decision; no ruleset or protection was silently imposed. |
| Scorecard CodeReview | 1 | Historical approval practice; owner-created PRs and passing CI are not independent approval. No synthetic approvals were added. |
| Scorecard Best Practices | 1 | No OpenSSF Best Practices certification was claimed. Applying for a badge requires an honest owner assessment. |

**Priority:** credential storage and transport, actual path escapes, accidental
secret logging and vulnerable dependencies precede certification or historical
score changes. This work does not implement the product/architecture proposal
in [PR #119](https://github.com/pjunod/curator/pull/119).

## Paths — operator capabilities and mutation authority differ

All 83 Go path results were grouped by sink: **6 library**, **75 acquisition
or probe**, and **2 retained-plan** locations. Their dataflows originate at
14 HTTP decode sites. Curator is an operator tool: root registration,
filesystem browsing and manual import intentionally accept configured or
explicitly selected absolute paths. A generic restriction to the application
data directory would break the product and would not establish custody.
Native API authentication is opt-in; deployments that expose the service to
untrusted clients must configure authentication and network access controls.

| Entry or operation | Boundary traced | Assessment |
|---|---|---|
| Library adoption and manual entry | `rootFolderFor` originally compared lexical parent only | Confirmed bypass with an outside symlink. PR #149 resolves parent and target, preserves root aliases/internal links and revalidates queued adoption. |
| Library root registration | `AddRootFolder` requires an absolute, accessible directory | Intentional operator configuration. Restricting it to previously configured roots would make initial setup impossible. |
| Library folder repair | `RepairFolder` resolves the selection and roots, requires containment, checks claims/media and preserves registered root spelling | Already protected path. Its initial metadata reads are not a grant of mutation authority. |
| Manual source selection and item-import planning | `selectedFiles`, generation snapshots, `admitEpisodeTargets`, publication lookup and admission | Explicit operator source selection is supported; selections cannot silently switch to siblings or bypass accepted delivery ownership. Planning observations do not authorize later cleanup. |
| Publication placement and file resolution | Exact membership in stored publication files, accepted outcome, download identity and the placement fence | A request path is a selection among accepted files. It is not an arbitrary destination or a new manifest entry. |
| Delivery import | Exact relative path in accepted manifest before mapping to its publication | The downstream path helpers do not encode this provenance in their string types, so taint remains visible to the scanner. |
| Completed deletion and storage review | Fresh fingerprint, ownership, mount identity, live reservations, unresolved placements, protected library/recovery overlap, rooted deletion | Cached paths alone do not authorize deletion. Preserve these checks and per-file publication outcomes. |
| Re-match | File IDs, title leases, preview revisions, journaled moves and placement checks | Operator selection is bound to stored library files; reads/probes do not by themselves grant deletion authority. |
| Retained recovery dry-run | Absolute regular non-symlink files, digest/generation observation, explicit non-authorizing plan | Intentional inventory of externally evidenced media. Neither a matching digest nor a plan authorizes mutation or disposal. |
| Probe and placement primitives | Called after the above admission/selection; regular-file, source/destination symlink, generation, digest and journal checks at the relevant boundaries | Scanner paths spanning these functions are not evidence that each primitive independently validates HTTP input. They remain open for boundary-specific review rather than blanket dismissal. |

Relevant regressions already include
`TestAcq2ManualImportRejectsASelectionOutsideTheScannedPath`,
`TestAnchoredDeliveryPathReadersRejectTraversalAndSymlinkComponents`,
`TestReviewedStorageDeletionNeverFollowsSymlink`,
`TestCompletedRootsAndLibraryAliasesAreProtected`,
`TestPlacementRejectsSourceBytesThatChangedAfterManifestVerification`,
`TestPlaceIsIdempotentForTheSameFile` and
`TestARunningImportCanBeCancelledAndRestartedLater`.

**Limits:** this is not a claim that all 83 findings are false positives or
that path-string validation creates a sandbox against hostile local writers.
The concrete adoption bypass is reproducible; the other groups have distinct
capabilities and authority checks. Keep the findings and scans available as
those boundaries evolve. Do not replace rooted deletion, retained-media
protections, cancellation, hard-link handling or journaling with a superficial
path sanitizer.

## Logging and hashing — follow use, not field names

The 52 Go log dataflows end in structured logger attributes. Both configured
handlers escape CR/LF and terminal controls; regression tests retain the
original JSON field values while proving they cannot forge another record.
User-visible titles and paths are not rewritten. Unknown compatibility-route
logs did include raw queries and could therefore disclose the supported
`apikey` parameter; PR #148 retains diagnostic parameter names only.

The SHA-256 alert in native login is a real weak-password-storage defect.
PR #147 writes versioned PBKDF2 credentials and upgrades a legacy credential
only after successful authentication, using one atomic hash-setting write.
Failed migration persistence keeps the previous login usable. The legacy
verifier necessarily remains until credentials have migrated.

`torznab.cacheKey` uses a truncated API-key digest to invalidate an in-memory
capabilities cache after configuration changes. It neither stores a human
password verifier nor authorizes requests. `clientFingerprint` detects
changes in a download client's connection configuration in a persisted plan;
it is not used to authenticate a login, and that configuration store already
holds the external credential needed to contact the client. Replacing these
revision/cache fingerprints with a password KDF would not fix login storage
and could break plan compatibility. Neither fingerprint should be exposed as
a credential, logged, or promoted to an authentication check.

## Dependency remediation and residual findings

A fresh OSV query covered all declared Go modules/toolchain, all three npm
lockfiles, the Cargo lockfile and the four Python reference requirements.
It found **183 unique advisory IDs on the base and 2 after remediation**,
across 1,047 and 1,055 distinct package/version tuples respectively. This
fresh inventory is broader than the Scorecard artifact's 102-ID summary;
counts are not interchangeable, and OSV can report aliases of one defect.

The remediation updates the supported Go baseline to 1.26.11 and affected
`x/net`/`x/mod` modules, with their required transitive versions. Docker
builders use that baseline and external image indexes are pinned by digest,
verified to include Linux amd64 and arm64. Tags remain readable for
Dependabot. The `media-runtime` alias continues to refer to the preceding
local stage; pinning it as an external image would change the build.

npm updates stay within declared ranges except two explicit parent-scoped
overrides: `workbox-build` uses patched `glob` 14.0.4 or later, and
`@expo/fingerprint` uses patched `minimatch` 11.0.5 or later. Lockfiles are
generated and clean-installed with CI's npm 10; tests and builds validate
these transitive compatibility changes. The PWA plugin is not downgraded
to satisfy npm's suggested audit fix.

Candle 0.11.1 permits patched ZIP 9.5.1, and rustix advances to 1.1.6.
Python reference requirements advance to patched releases and gain weekly
Dependabot coverage. The reference tool explicitly requires local
safetensors and disables remote code; it still verifies every pinned model
file. Original committed vector fixtures remain unchanged.

| Remaining dependency | Exposure and follow-up |
|---|---|
| `deepmerge` 4.3.1, [GHSA-gcqx-w48m-w2h4](https://github.com/advisories/GHSA-gcqx-w48m-w2h4) | Low-severity prototype poisoning in the Rollup/PWA build-tool chain, not a served runtime dependency. No upstream fixed release was available in the audit. Track a compatible upstream replacement or patch; do not force a PWA downgrade or hide the alert. npm expands this one issue into four low-severity package-chain entries. |
| `paste` 1.0.15, [RUSTSEC-2024-0436](https://rustsec.org/advisories/RUSTSEC-2024-0436.html) | Unmaintained compile-time macro crate reached through Candle's GEMM dependencies. This is maintenance risk, not a demonstrated runtime exploit. No patched version exists. Track upstream migration to a maintained replacement; do not relabel the existing crate as fixed. |

Local validation covers web type/unit/PWA build, mobile type/unit/both
exports, Rust unit tests plus pinned tokenizer/vector parity, and regenerated
Python reference vectors compared with the original tolerance. Maximum
absolute vector errors were below 0.0000002. Go vulnerability analysis found
no findings. Final race/lint and hosted-CI status belong in the remediation
PR; an open PR is not a deployed fix.

## Repository controls — owner decisions remain explicit

Owner-compatible branch protection can require pull requests, stable required
CI checks, resolved conversations and blocked force pushes/deletions while
requiring **zero approvals** until an independent reviewer is available.
Select actual final check names after healthy CI, document a recovery path
and decide who may bypass it. Requiring approval of the solo owner's own PR
would lock out normal maintenance. No setting change is bundled here, and
zero required approvals must not be described as independent code review.

PR job routing to hosted runners is useful but is not a boundary against a
malicious workflow edit selecting an available lab runner. Review all
external workflow changes before approving them. A stronger option is to
remove shared lab runners from public-repository access or provision isolated
ephemeral runners with no lab secrets/network access. Organization runner
groups can additionally restrict trusted workflow references; see
[GitHub's access controls](https://docs.github.com/en/actions/how-tos/manage-runners/self-hosted-runners/manage-access).
This requires an infrastructure/ownership decision, not another label in a
PR-editable workflow.

SAST history will improve as scanned commits enter Scorecard's sample.
Best Practices certification and independent code review require actual
practice and owner participation. No badges, approvals or protections are
fabricated to improve the numerical score.
