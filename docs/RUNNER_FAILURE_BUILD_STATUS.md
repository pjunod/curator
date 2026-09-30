# Runner failure build — implementation and evidence ledger

**Status:** implemented with passing local gates; release qualification pending · **Updated:** 2026-09-30

Companion to [the build contract](RUNNER_FAILURE_BUILD.md). This ledger records
implementation and tests, separately from deployment and native-mount evidence.
No production changes or retained-media mutations are authorized by this build.

## B0 — source and preserved work

Runner was fast-forwarded from `f959cf922b47c581713df9401003c7c6f96cf5f7`
to fetched upstream `ab25d0cbecfdb5adf299072813a2948383192d9f`, exactly the
diagnosed source. No resets or force checkout were used. README additions were
already upstream. Colliding lifecycle/mobile/history drafts were compared and
saved byte-for-byte under `/private/tmp/runner-failure-baseline-20260930` before
the fast-forward; differing drafts predated the upstream implementation status.
Other untracked files remain in place. Cargo.lock changes in this fast-forward
are upstream changes, not new dependency changes by this build.

Monarr baseline: `1fad7b466132dd181f2946490c36c5cef1009cdb`, VERSION `0.35.0`.
Its pre-existing tracked patch is saved in that same backup directory. Existing
failure/inventory changes are part of the required audit; unrelated discovery
work must remain intact.

Toolchain pins: Runner Rust `1.97.1`, MSRV `1.95`; Monarr Go `1.25.7`.
Rust commands use `/Users/pjunod/.cargo/bin` ahead of Homebrew. `par2`, `7z`,
Node and Go are available. All final local gates passed; results are recorded below.

### Initial dirty paths

Runner before advancement:
```text
 M README.md
?? Claude outputs/
?? docs/BITTORRENT_MAGNET_DHT_PLAN.md
?? docs/FILE_LIFECYCLE_PLAN.md
?? docs/FILE_LIFECYCLE_REVIEW.md
?? docs/HISTORY_LOADING_PLAN.md
?? docs/MOBILE_QUEUE_PARITY_PLAN.md
?? docs/MOBILE_QUEUE_PARITY_REVIEW.md
```

Monarr before implementation:
```text
 M README.md
 M docs/settings.md
 M docs/usage.md
 M internal/adapters/nzbd/events.go
 M internal/adapters/nzbd/events_test.go
 M internal/adapters/nzbd/nzbd.go
 M internal/app/acquisition/acquisition.go
 M internal/app/acquisition/completed.go
 M internal/app/acquisition/completed_test.go
 M internal/app/acquisition/handoff_test.go
 M test/e2e/tests/completed-folders.spec.ts
?? docs/RUNNER_FAILURE_BUILD.md
?? docs/RUNNER_FAILURE_DIAGNOSIS.md
?? docs/RUNNER_FAILURE_FIX_PLAN.md
?? docs/RUNNER_FAILURE_REVIEW.md
```

## Phase evidence

| Phase | Local state | Evidence / limitations |
|---|---|---|
| B0 | Implemented | Runner fast-forward preserved collisions; four diagnostic fixtures ported |
| B1 | Implemented and target-tested | Persisted owner fences, consumer suppression, bounded extractor attempts, durable relocation uniqueness |
| B2 | Implemented and target-tested | Per-segment data flush, restart CRC revalidation, stable partial IDs, resource probes/backoff/budget, injected write/sync/finalize exhaustion |
| B3 | Implemented and target-tested | Safe yEnc names, full PAR identity, nested repair workspace, shared-parent publication, retained archives; exhaustive crash/namespace matrix remains a release requirement |
| B4 | Synthetic implementation; disabled in ordinary calls | Typed actual path, registry generations, reviewed-operation reconciliation; native mount and reader isolation gates pending |
| B5 | Implemented and verified in Monarr | Migration 0037, active holds, atomic overlap guard, Activity resume, read-only recovery planner and placement/receipt revalidation |
| B6 | Locally verified | All available required local gates passed; mount and release-image qualification remain pending |

Monarr's shared HEAD advanced concurrently to
`1be5b9a1398c9ebb5afce58612d1de979cb8c047` (merge of origin/main). Existing
work was retained. VERSION was reread as `0.35.0` and advanced to `0.35.1`.
Runner remains based on `ab25d0cbecfdb5adf299072813a2948383192d9f`.
Initial evidence concerns the working trees used for the first PR commits.
Release-image qualification remains pending.

Cargo.lock deliberately adds dependency edges only: existing locked
`serde_json` for types, `crc32fast` for engine, and `md-5` for PAR parsing
and the engine's synthetic packet fixture. No dependency version was updated.
SQL and API bindings were regenerated with sqlc `v1.31.1` and oapi-codegen
`v2.8.0`; generated files were not hand-edited.

## Verification commands and results

Pinned environment: Runner `PATH=/Users/pjunod/.cargo/bin:$PATH` (Rust
1.97.1; MSRV 1.95); Monarr `GOTOOLCHAIN=go1.25.7` and
`GOCACHE=/private/tmp/monarr-failure-go-cache`. Local socket fixtures needed
sandbox escalation; no production services were contacted.

| Command | Result / evidence |
|---|---|
| Monarr `make lint` | Passed, 0 issues; `/private/tmp/monarr-failure-lint-final.log` |
| Monarr `make test` | Passed full Go suite; `/private/tmp/monarr-failure-test-final.log` |
| Monarr `make test-web` | Passed; `/private/tmp/monarr-failure-web-final.log` |
| Monarr `make test-e2e` | Passed, 128 tests on 0.35.1; `/private/tmp/monarr-failure-e2e-final.log` |
| Monarr affected packages `go test ... -shuffle=on -count=3` | Passed, both packages three shuffled runs; `/private/tmp/monarr-failure-repeat.log` |
| Runner targeted engine/post/state/types/nzb/par2 suites | Passed before final additional fixtures; logs in `/private/tmp/runner-failure-*.log` |
| Runner nested damaged PAR fixture | Passed with native par2, damaged after 16 KiB |
| Runner private cluster range assembly | Passed after preserving the authenticated sparse-checkpoint lane |
| Runner `make ui-test` | Passed both native harnesses; `/private/tmp/runner-failure-ui.log` |
| Runner `RUST_TEST_THREADS=1 make check` | Passed including MSRV; `/private/tmp/runner-failure-check-final.log` |
| Runner `RUST_TEST_THREADS=1 make test-strict` | Passed with mandatory tools; `/private/tmp/runner-failure-strict-final.log` |

Initial full Runner runs caught the private range compatibility defect and
were not counted as green evidence. A native-history attribution test and
quorum test also failed in broad runs; the history test passed in isolation.
Final complete runs passed. The history assertion was updated to wait for
the upstream asynchronous observation worker (bounded five seconds), with no
production history behavior change. Quorum tests passed in serialized runs.

## Review-to-regression matrix

| Finding | Concrete regression coverage |
|---|---|
| R1 namespace/PAR identity | `par_catalog_subdirectories_should_be_restored_before_verification`; recursive bounded namespace, set-aware loader and parser digest tests |
| R2 shared parent deletion | `extraction_shared_parent_preserves_siblings_and_conflicts`; `unpack_then_cleanup` now asserts retained archive |
| R3 damaged nested repair | `corrupt_nested_payload_after_prefix_gets_repaired_without_losing_original`; `insufficient_parity_holds_originals_without_terminal_history` |
| R4 writer terminalization | `write_sync_and_publication_exhaustion_preserve_same_file_for_resume`; `writer_resource_errors_hold_pending_segments_and_latch_out_of_space`; `unverified_holes_remain_partial_and_nonterminal` |
| R5 quota/retry | EDQUOT injected alongside ENOSPC; `resource_failure_never_runs_fallback`; persisted admission probe count/backoff |
| R6 publication/reader custody | `unsupported_publication_copies_zero_payload_and_repeated_review_is_one_claim`; `lost_commit_acknowledgement_reconciles_same_publication`; source/workspace identity tests |
| R7 consumer regrab | `TestResourceHoldPersistsAndSuppressesReplacement`; `TestControlRevisionOrderingAndContinuity`; `TestHeldSeasonPackSuppressesEpisodeAndPack`; held-job reload/resume browser test |
| R8 custody versus media validity | `registry_generation_is_stable_retains_source_and_failed_media_status`; types `usenet_ready_requires_successful_post_processing`; adapter failed-history regressions |
| R9 stale recovery authority | `TestRetainedPlanMixedReleasesPartialAndPendingReceipts`; `TestRetainedPlanActiveParentIsProvisionalAndFreshOverlapBlocksValidation`; existing recovery receipt/target-generation suite |

Golden contract: Rust `control_v1_golden_keeps_decimal_revision_and_ignores_future_fields`
and Go `TestControlGoldenDecimalRevisionAndUnknownFields`. Golden files have
identical bytes. Legacy snapshots without reliable checkpoint bytes are
revalidated or held; legacy failed rows are not blanket-reclassified.

## Release and rollback constraints

- Native deployment mounts, remounts, storage-server power loss and every
  deployed reader/path mapping remain unverified. Registry fallback is
  unreachable from ordinary relocation calls; no enablement setting was added.
- Cross-volume source bytes, original archives and transform workspaces remain
  retained. Local flush/DB commit is not permission to retire them. Independent
  receipt/ownership retirement and exhaustive crash-boundary qualification
  remain required before relaxing this conservative behavior.
- No release image was built or inspected. The existing build system's
  `NZBD_GIT_DESCRIBE` stamping must be checked on the intended release image;
  `+unknown` remains a release blocker. A local test binary is not that image.
- Migration 0037 upgrades existing rows with empty control. Its rollback
  refuses any nonempty stored control; explicit reconciliation is required
  before a downgrade can discard those facts. No live database was migrated.
- The retained plan grants no mutation authority. Active observations are
  provisional; real recovery still requires the existing leases, fresh target
  snapshot, placement journal and receipt coordinator.

Implemented/local test evidence is distinct from mount-verified, deployable
and deployed. Nothing was deployed. No real downloads, import, staging delete,
blocklist clearing or Monster recovery occurred.

## Initial build source fingerprints

SHA-256 covers sorted changed source/fixture paths and bytes (including VERSION
and Cargo.lock), excluding prose docs. This identifies the dirty source more
precisely than the base commit alone. These identify the initial PR commits;
review follow-up evidence and fingerprints are recorded below.

- nzbd: `47d86bada5b92bee3050a51455a945f0d9533fabda28718604007b39d7a7650c`
- monarr: `abcc9aa708ce140f6766e745d6e78a72e45bdc330fec09c9513fdee8016b09cf`

## PR delivery

Runner branch: `codex/runner-resource-holds`. Monarr branch:
`codex/runner-held-acquisitions`. Before committing, both fetched `origin/main`
refs matched the local HEADs. Monarr advanced to
`ebae5d3ed178fc184f5d8fe1d47a84abac5b4b28`; the intervening changes add
unrelated test coverage. Both initial tested source fingerprints matched at
first delivery.

Runner PR: [#244](https://github.com/pjunod/runner/pull/244).
Curator/Monarr PR: [#56](https://github.com/pjunod/curator/pull/56).

## Initial explicit commit commands

Review shared changes before staging. These include the authorized earlier
failure/inventory patch in touched files and exclude unrelated untracked Runner
notes, preserved backups and other projects. The build was delivered in
separate Runner and Monarr PRs using these explicit staging scopes.

```bash
cd /Users/pjunod/code/nzbd
git add \
  Cargo.lock \
  crates/nzbd-api/src/lib.rs \
  crates/nzbd-cluster/src/range.rs \
  crates/nzbd-engine/Cargo.toml \
  crates/nzbd-engine/src/events.rs \
  crates/nzbd-engine/src/lib.rs \
  crates/nzbd-engine/src/owner.rs \
  crates/nzbd-engine/src/pool.rs \
  crates/nzbd-engine/src/queue.rs \
  crates/nzbd-engine/src/snapshot.rs \
  crates/nzbd-engine/src/writer.rs \
  crates/nzbd-engine/tests/e2e.rs \
  crates/nzbd-nzb/src/lib.rs \
  crates/nzbd-nzb/tests/failure_subject.rs \
  crates/nzbd-par2/Cargo.toml \
  crates/nzbd-par2/src/lib.rs \
  crates/nzbd-post/src/deobfuscate.rs \
  crates/nzbd-post/src/lib.rs \
  crates/nzbd-post/src/manager.rs \
  crates/nzbd-post/src/namespace.rs \
  crates/nzbd-post/src/par2.rs \
  crates/nzbd-post/src/rename.rs \
  crates/nzbd-post/src/repair_workspace.rs \
  crates/nzbd-post/src/tools.rs \
  crates/nzbd-post/tests/failure_regressions.rs \
  crates/nzbd-post/tests/pp_pipeline.rs \
  crates/nzbd-state/src/artifacts/mod.rs \
  crates/nzbd-state/src/artifacts/relocation.rs \
  crates/nzbd-state/src/artifacts/relocation_failure_tests.rs \
  crates/nzbd-state/src/artifacts/transforms.rs \
  crates/nzbd-state/src/capacity.rs \
  crates/nzbd-state/src/fileops.rs \
  crates/nzbd-state/src/lib.rs \
  crates/nzbd-types/Cargo.toml \
  crates/nzbd-types/fixtures/job-control-v1.json \
  crates/nzbd-types/src/lib.rs \
  crates/nzbd/tests/integration_events.rs \
  docs/FILE_LIFECYCLE_OPERATIONS.md
git commit -m "Preserve held transfers and journal safe payload recovery"
```

```bash
cd /Users/pjunod/code/monarr
git add \
  VERSION \
  docs/RUNNER_FAILURE_BUILD_STATUS.md \
  docs/adr/0021-file-lifecycle-recovery.md \
  docs/integration.md \
  docs/settings.md \
  docs/usage.md \
  internal/adapters/nzbd/events.go \
  internal/adapters/nzbd/events_test.go \
  internal/adapters/nzbd/nzbd.go \
  internal/adapters/nzbd/testdata/job-control-v1.json \
  internal/api/acquisition_handlers.go \
  internal/api/gen/api.gen.go \
  internal/api/openapi.yaml \
  internal/api/recovery_handlers.go \
  internal/app/acquisition/acquisition.go \
  internal/app/acquisition/completed.go \
  internal/app/acquisition/completed_test.go \
  internal/app/acquisition/download_control.go \
  internal/app/acquisition/download_control_test.go \
  internal/app/acquisition/handoff.go \
  internal/app/acquisition/handoff_test.go \
  internal/app/acquisition/importers.go \
  internal/app/acquisition/retained_plan.go \
  internal/app/acquisition/retained_plan_test.go \
  internal/infra/sqlite/acquisition.go \
  internal/infra/sqlite/gen/acquisition.sql.go \
  internal/infra/sqlite/gen/models.go \
  internal/infra/sqlite/migrations/0037_download_control.sql \
  internal/infra/sqlite/queries/acquisition.sql \
  internal/ports/downloadclient.go \
  test/e2e/tests/completed-folders.spec.ts \
  web/src/api.ts \
  web/src/pages/Activity.tsx
git commit -m "Preserve held transfers and journal safe payload recovery"
```

## Astra review follow-up — 2026-09-30

The first PR commits reproduced all six reported regressions before changes.
The permanent probes retain the review test names and extend their coverage.
Curator VERSION is `0.35.2` for these visible resume/completion fixes.

| Review finding | Change and regression evidence |
|---|---|
| Runner extraction hold overwritten | `PostError::Held` preserves the already-persisted stage control; `review_manager_preserves_extractor_capacity_hold` |
| Runner fallback blocked after ENOSPC | Journaled extraction generations verify prior identities and retain bytes; `review_diskfull_fallback_can_be_retried_in_same_workspace` also restores capacity and checks successful output and retained partials |
| Runner delayed parity never fetched | Isolated repair returns missing-block counts, unpauses volumes, waits and reloads the same set; `review_missing_parity_requests_available_paused_volume` |
| Runner nested set restored at job root | Candidate and target paths stay under the set root; `review_nested_par_set_keeps_its_catalog_root` |
| Curator missed resume strands completion | Terminal Runner params preserve `*Control:v1`; polling and SSE retain it, including queue retirement; `TestReviewCompletionAfterMissedResume`, `TestReviewCompletionControlSurvivesPollAndPush`, `review_terminal_history_and_event_keep_resolved_control` |
| Curator equal running revision freezes progress | Identical control accepts progress/stage updates; older, changed-content and instance conflicts remain fenced; `TestReviewRunningProgressWithSameControlRevision` |

`extraction_retry_generations_survive_restart_and_reject_changed_identity`
checks journal recovery, retained partial bytes and directory identity changes.
These tests use temporary engines, synthetic archives, native PAR2, simulated
extractor failures, temporary HTTP services and SQLite databases.

All final source gates passed: Runner `make check` (including Rust 1.95 MSRV),
`make test-strict` and `make ui-test`; Curator `make lint`, `make test`,
`make test-web` (119 tests) and `make test-e2e` (128 tests on 0.35.2).
Curator acquisition and Runner adapter packages passed three shuffled runs.
The runtime additions afterward change container recipes, documentation and one
stale Go comment; lint was rerun. Functional Go/Rust source remains the gated
source.

Logs: `/private/tmp/runner244-review-{check,strict,ui}.log`,
`/private/tmp/curator56-review-{lint,test,web,e2e,repeat}.log`.

### Container tool audit and verification

Runner installs `par2`, real `unrar`, `7z`, `7zz`, `ca-certificates` and `tini`.
The named `runtime-tools` stage fails the build if a configured default tool is
missing. Native non-root container fixtures verified PAR2 repair, ZIP creation
and extraction, plus RAR3 and RAR5 multipart test/extraction. The public
[rarfile multipart fixtures](https://github.com/markokr/rarfile/tree/master/test/files)
were mounted read-only; all outputs stayed in the disposable container.

Curator uses Cinema/plurx's signed Jellyfin Bookworm repository and
`jellyfin-ffmpeg8` package. Both binaries, the bundled library tree, transitive
system libraries and package license metadata are copied into distroless.
Build checks require Cinema's AC-4, `dovi_rpu` and `tonemapx apply_dovi`
capabilities and execute both binaries in the actual runtime base.
`MONARR_FFPROBE=/usr/lib/jellyfin-ffmpeg/ffprobe` selects the same tool path.
The local build produced `8.1.3-Jellyfin`; the shared major tracks repository
updates as Cinema's current Dockerfile does. A synthetic 160×90 NUT stream
was generated and measured successfully as UID 1000 in that runtime base.
The existing embedding executable remains included in the final image.

Tool-layer build and smoke logs: `/private/tmp/runner244-runtime-build.log`,
`/private/tmp/runner244-runtime-smoke.log`,
`/private/tmp/runner244-unrar-multipart.log`,
`/private/tmp/curator56-runtime-build.log`,
`/private/tmp/curator56-jellyfin-version.log` and
`/private/tmp/curator56-runtime-smoke.json`. Full release daemon images, native
mount/power-loss qualification and deployment remain pending.

Follow-up fingerprints include both container recipes and the permanent
review test file, exclude prose docs, and cover the full PR source scopes:

- Runner: `ebaa46b90d8aceb31d6dafb524900e086d321f2c7c84b266806bc7aa4e9b3003`
- Curator: `19d6d477c315fec928747ebcfc4234c243c51b5adf831dfdef61433c9b721c50`
