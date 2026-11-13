# Documentation index

Every file under `docs/` is listed here, and `go test ./internal/docs` fails
when one is missing or a relative link in any document does not resolve. Start with
[integration.md](integration.md) for how the three applications work together and
[PIPELINE_CONTRACT.md](PIPELINE_CONTRACT.md) for why; [usage.md](usage.md) and
[settings.md](settings.md) are the user manuals. The repository front page is
[../README.md](../README.md); the project status ledger is [../STATUS.md](../STATUS.md).

## Start here

| File | What it is |
|---|---|
| [integration.md](integration.md) | Every seam with nzbd and plurx: what each does, where you look at it, the command that proves it. |
| [PIPELINE_CONTRACT.md](PIPELINE_CONTRACT.md) | Pipeline contract v2 — the design: one terminal fact per job, one owner per boundary. Wins over every other page. |
| [usage.md](usage.md) | Using Monarr: every screen, the Pipeline tiles, Resolve, the delivery log. |
| [settings.md](settings.md) | Every setting, field by field. |
| [CI_POLICY.md](CI_POLICY.md) | Required merge checks, metadata CI and ephemeral runner policy. |
| [architecture.md](architecture.md) | Architecture blueprint. |
| [deployment.md](deployment.md) | Storage, floating nodes and HA. |
| [SECURITY_CHECKS.md](SECURITY_CHECKS.md) | Security workflows, badge interpretation and remaining repository controls. |
| [SECURITY_TRIAGE.md](SECURITY_TRIAGE.md) | 2026-11-13 security finding triage, boundary evidence, dependency remediation and owner decisions. |
| [SECURITY_DEPENDENCY_PATCHES.md](SECURITY_DEPENDENCY_PATCHES.md) | Provenance, validation and removal of local upstream dependency fixes. |

## Decisions

| Path | What it is |
|---|---|
| [adr/README.md](adr/README.md) | Architecture decision records, 0001 onward; [0026](adr/0026-pipeline-contract-v2.md) records pipeline contract v2. Each record is listed in that index. |

## Feature plans and reviews

| File | What it is |
|---|---|
| [plan-calendar.md](plan-calendar.md) | Calendar rebuilt: a readable month and a rich agenda. |
| [plan-discover.md](plan-discover.md) | Discover, a browse surface inside Monarr. |
| [plan-quality-truth.md](plan-quality-truth.md) | Quality truth: implementation plan. |
| [plan-release-identity.md](plan-release-identity.md) | Release identity: aliases, country qualifiers and direct ID search. |
| [plan-search-result-preview-review.md](plan-search-result-preview-review.md) | Review of the search result preview plan. |
| [plan-search-result-preview.md](plan-search-result-preview.md) | Search result preview: inspect a title before adding it. |
| [plan-season-acquisition-review.md](plan-season-acquisition-review.md) | Season acquisition review: decisions, corrections and build status. |
| [plan-season-acquisition.md](plan-season-acquisition.md) | Season acquisition planner. |
| [plan-smart-media-discovery-review.md](plan-smart-media-discovery-review.md) | Smart media discovery review: the build contract challenged. |
| [plan-smart-media-discovery.md](plan-smart-media-discovery.md) | Smart media discovery: build theme search and related-series suggestions. |
| [plan-wanted-groups.md](plan-wanted-groups.md) | Wanted groups: expandable titles and searches with explicit scope. |

## Delivery and status ledgers

- [UI_CONSOLIDATION_STATUS.md](UI_CONSOLIDATION_STATUS.md) — implementation, review, validation and delivery of the approved queue/Settings consolidation.

Each records what was built, reviewed and verified for one piece of work.

| File | What it is |
|---|---|
| [ADHOC_JOB_PRIORITY_STATUS.md](ADHOC_JOB_PRIORITY_STATUS.md) | Ad-hoc job priority: delivery and verification. |
| [BAD_DOWNLOAD_API.md](BAD_DOWNLOAD_API.md) | Bad media reports and replacement operations. |
| [COMPLETED_FOLDER_STATUS.md](COMPLETED_FOLDER_STATUS.md) | Completed-folder accounting: delivery status. |
| [DOWNLOAD_LIFECYCLE_STATUS.md](DOWNLOAD_LIFECYCLE_STATUS.md) | Download lifecycle: implementation and review status. |
| [FABLE_REVIEW_HANDOFF.md](FABLE_REVIEW_HANDOFF.md) | Fable review handoff for the Runner and Curator download lifecycle. |
| [IMPORT_QUEUE_STATUS.md](IMPORT_QUEUE_STATUS.md) | Import Queue: implementation, review and verification. |
| [LIFECYCLE_REMEDIATION_STATUS.md](LIFECYCLE_REMEDIATION_STATUS.md) | Lifecycle remediation: remaining fixes and delivery status. |
| [NFS_DEPLOYMENT_DRAIN.md](NFS_DEPLOYMENT_DRAIN.md) | Deploying after an import drain. |
| [NFS_QUALIFICATION_PROTOCOL.md](NFS_QUALIFICATION_PROTOCOL.md) | NFS shutdown qualification: frozen workload and evidence gates. |
| [NFS_QUALIFICATION_RESULTS.md](NFS_QUALIFICATION_RESULTS.md) | NFS shutdown qualification results. |
| [NFS_SHUTDOWN_IMPLEMENTATION.md](NFS_SHUTDOWN_IMPLEMENTATION.md) | NFS shutdown implementation (historical design, superseded by ADR 0025). |
| [NFS_SHUTDOWN_IMPLEMENTATION_STATUS.md](NFS_SHUTDOWN_IMPLEMENTATION_STATUS.md) | NFS shutdown: implementation, review and delivery status. |
| [RELEASE_IDENTITY_STATUS.md](RELEASE_IDENTITY_STATUS.md) | Release identity: implementation and delivery record. |
| [SCAN_DISK_STATUS.md](SCAN_DISK_STATUS.md) | Disk scan status: visible progress through durable completion. |
| [SEASON_ACQUISITION_STATUS.md](SEASON_ACQUISITION_STATUS.md) | Season acquisition: implementation and verification status. |
| [SERIES_MONITORING_STATUS.md](SERIES_MONITORING_STATUS.md) | Series monitoring: delivery status. |
| [STORAGE_ASSOCIATION_STATUS.md](STORAGE_ASSOCIATION_STATUS.md) | Storage associations: shared-folder receipts must not claim every payload. |
| [TV_QUALITY_STATUS.md](TV_QUALITY_STATUS.md) | TV quality details: delivery status. |
| [smart-media-discovery-qualification.md](smart-media-discovery-qualification.md) | Smart media discovery: delivered verification and its limits. |
| [status.html](status.html) | The project status page (rendered). |
| [wanted-groups-status.md](wanted-groups-status.md) | Wanted groups: implementation status. |

## Smart media discovery evidence

The M0 gates of [plan-smart-media-discovery.md](plan-smart-media-discovery.md): reports and the frozen data they judge.

| File | What it is |
|---|---|
| [smart-media-discovery-m0a-annotations.md](smart-media-discovery-m0a-annotations.md) | Smart media discovery M0a — candidate evidence annotation. |
| [smart-media-discovery-m0a-revision.md](smart-media-discovery-m0a-revision.md) | Smart media discovery M0a revision — bounded retrieval after the failed gate. |
| [smart-media-discovery-m0a.md](smart-media-discovery-m0a.md) | Smart media discovery M0a — retrieval coverage evidence. |
| [smart-media-discovery-m0b.md](smart-media-discovery-m0b.md) | Smart Media Discovery M0b: standalone helper packaging evidence. |
| [smart-media-discovery-m0c-candidate-correction.md](smart-media-discovery-m0c-candidate-correction.md) | Smart Media Discovery M0c: corrected candidate construction. |
| [smart-media-discovery-m0c-corpus.md](smart-media-discovery-m0c-corpus.md) | Smart Media Discovery M0c: frozen corpus and judgment limits. |
| [smart-media-discovery-m0c-four-seed-tuning-v2.md](smart-media-discovery-m0c-four-seed-tuning-v2.md) | Smart Media Discovery M0c: generic four-seed tuning. |
| [smart-media-discovery-m0c-four-seed-tuning.md](smart-media-discovery-m0c-four-seed-tuning.md) | Smart Media Discovery M0c: four-seed tuning comparison. |
| [smart-media-discovery-m0c-gate.md](smart-media-discovery-m0c-gate.md) | Smart Media Discovery M0c gate. |
| [smart-media-discovery-m0c-policy-freeze.md](smart-media-discovery-m0c-policy-freeze.md) | Smart Media Discovery M0c: scoring policy freeze. |
| [smart-media-discovery-m0c-probe.md](smart-media-discovery-m0c-probe.md) | Smart Media Discovery M0c: first real-model ranking probe. |
| [smart-media-discovery-m0c-tuning-v2.md](smart-media-discovery-m0c-tuning-v2.md) | Smart Media Discovery M0c: corrected tuning probe. |
| [smart-media-discovery-m0c-tuning-v3.md](smart-media-discovery-m0c-tuning-v3.md) | Smart Media Discovery M0c: evidence and shallow-input correction. |
| [smart-media-discovery-m0d-bounded-capture-spec.md](smart-media-discovery-m0d-bounded-capture-spec.md) | Smart Media Discovery M0d: proposed bounded capture. |
| [smart-media-discovery-m0d-coverage-audit.md](smart-media-discovery-m0d-coverage-audit.md) | Smart Media Discovery M0d: coverage stage audit. |
| [smart-media-discovery-m0d-focused-source-policy.md](smart-media-discovery-m0d-focused-source-policy.md) | Smart Media Discovery M0d: focused seed-source revision. |
| [smart-media-discovery-m0d-retrieval-freeze.md](smart-media-discovery-m0d-retrieval-freeze.md) | Smart Media Discovery M0d: bounded seed source mixture. |

Data files (corpus, labels, candidates and fixtures the reports above cite):

- [smart-media-discovery-fresh-capture.json](smart-media-discovery-fresh-capture.json)
- [smart-media-discovery-integrated-resources.json](smart-media-discovery-integrated-resources.json)
- [smart-media-discovery-m0a-annotation-fixture.json](smart-media-discovery-m0a-annotation-fixture.json)
- [smart-media-discovery-m0a-fixture.json](smart-media-discovery-m0a-fixture.json)
- [smart-media-discovery-m0a-revision-fixture.json](smart-media-discovery-m0a-revision-fixture.json)
- [smart-media-discovery-m0c-adjudications.json](smart-media-discovery-m0c-adjudications.json)
- [smart-media-discovery-m0c-aux-seed-labels.json](smart-media-discovery-m0c-aux-seed-labels.json)
- [smart-media-discovery-m0c-aux-seeds.json](smart-media-discovery-m0c-aux-seeds.json)
- [smart-media-discovery-m0c-badbuddy-extra-labels.json](smart-media-discovery-m0c-badbuddy-extra-labels.json)
- [smart-media-discovery-m0c-candidates-v2.json](smart-media-discovery-m0c-candidates-v2.json)
- [smart-media-discovery-m0c-candidates.json](smart-media-discovery-m0c-candidates.json)
- [smart-media-discovery-m0c-capture-omissions.json](smart-media-discovery-m0c-capture-omissions.json)
- [smart-media-discovery-m0c-corpus-plan.json](smart-media-discovery-m0c-corpus-plan.json)
- [smart-media-discovery-m0c-identity-snapshot.json](smart-media-discovery-m0c-identity-snapshot.json)
- [smart-media-discovery-m0c-invalid-first-run.json](smart-media-discovery-m0c-invalid-first-run.json)
- [smart-media-discovery-m0c-korean-labels.json](smart-media-discovery-m0c-korean-labels.json)
- [smart-media-discovery-m0c-labels-v2.json](smart-media-discovery-m0c-labels-v2.json)
- [smart-media-discovery-m0c-labels.json](smart-media-discovery-m0c-labels.json)
- [smart-media-discovery-m0c-looking-extra-labels.json](smart-media-discovery-m0c-looking-extra-labels.json)
- [smart-media-discovery-m0c-other-theme-tuning-labels.json](smart-media-discovery-m0c-other-theme-tuning-labels.json)
- [smart-media-discovery-m0c-qtext-fixtures.json](smart-media-discovery-m0c-qtext-fixtures.json)
- [smart-media-discovery-m0c-r3-origins.json](smart-media-discovery-m0c-r3-origins.json)
- [smart-media-discovery-m0c-repaired-v2.json](smart-media-discovery-m0c-repaired-v2.json)
- [smart-media-discovery-m0c-repaired-v3.json](smart-media-discovery-m0c-repaired-v3.json)
- [smart-media-discovery-m0c-seed-combined-tuning-labels.json](smart-media-discovery-m0c-seed-combined-tuning-labels.json)
- [smart-media-discovery-m0c-tales-extra-labels.json](smart-media-discovery-m0c-tales-extra-labels.json)
- [smart-media-discovery-m0d-mixed-candidates.json](smart-media-discovery-m0d-mixed-candidates.json)
- [smart-media-discovery-m0d-mixed-labels.json](smart-media-discovery-m0d-mixed-labels.json)
- [smart-media-discovery-m0d-mixed-tuning-v1.json](smart-media-discovery-m0d-mixed-tuning-v1.json)
- [smart-media-discovery-m0d-new-tuning-labels.json](smart-media-discovery-m0d-new-tuning-labels.json)

## Archive

Superseded plans and proposals, kept as history. Each starts with a one-line pointer to what replaced it.

| File | What it is |
|---|---|
| [DOWNLOAD_LIFECYCLE_PROPOSAL.md](archive/DOWNLOAD_LIFECYCLE_PROPOSAL.md) | The approved download lifecycle proposal; sections 4.1-4.2 superseded by the pipeline contract. |
| [RUNNER_FAILURE_BUILD.md](archive/RUNNER_FAILURE_BUILD.md) | Runner PR #247 build handoff. |
| [RUNNER_FAILURE_BUILD_STATUS.md](archive/RUNNER_FAILURE_BUILD_STATUS.md) | Runner PR #247 implementation ledger. |
| [RUNNER_FAILURE_DIAGNOSIS.md](archive/RUNNER_FAILURE_DIAGNOSIS.md) | Runner failure incident evidence. |
| [RUNNER_FAILURE_FIX_PLAN.md](archive/RUNNER_FAILURE_FIX_PLAN.md) | Runner failure root causes and proposed fixes. |
| [RUNNER_FAILURE_REVIEW.md](archive/RUNNER_FAILURE_REVIEW.md) | Adversarial review of the Runner failure proposal. |
| [plan-integration.md](archive/plan-integration.md) | The original nzbd / monarr / plurx plan; its contract is superseded by the pipeline contract. |

## Renderings

- [renderings/](renderings/) — HTML and PNG captures the proposals and previews were reviewed against (`download-lifecycle/`, `media-preview/`).

- [Household discovery plan](HOUSEHOLD_DISCOVERY_PLAN.md) — approved daily-use journeys and renderings.
- [Household discovery status](HOUSEHOLD_DISCOVERY_STATUS.md) — review findings, milestones and delivery evidence.
- [ADR 0029](adr/0029-shared-availability.md) — shared observed availability and bounded owner shelves.
