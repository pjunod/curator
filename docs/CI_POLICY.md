# CI policy — protected merges and disposable build machines

**Applied:** 2026-11-13 · **Scope:** `pjunod/curator`, branch `main`

Companion to [SECURITY_CHECKS.md](SECURITY_CHECKS.md) (scanner operation) and
[deployment.md](deployment.md) (application deployment). This policy governs
source merges and CI machines; it does not deploy Curator or require a
second maintainer to approve the solo owner's PRs.

## Every merge requires a tested pull request

`main` requires a pull request, resolved review conversations and these
checks from their verified GitHub publishers. Branches must be up to date;
protection applies to administrators. Required approval count is **zero**.
Force pushes and branch deletion are disabled. Merge commits remain allowed.

| Publisher | Required checks |
|---|---|
| GitHub Actions, app 15368 | Go race shard 0–7; Unit tests (Go + web); End-to-end (real binary + browser); Mobile (iOS + Android bundles); Fake-consumer flows + contract shapes; Personalities against the real binary; Docker image builds; golangci-lint + gofmt; Review dependency changes; CodeQL (go); CodeQL (javascript-typescript) |
| GitHub Code Scanning, app 57789 | CodeQL aggregate security gate |

There are 19 required checks. Source binding prevents an unrelated status
publisher from impersonating them. It does not replace owner review of a
workflow edit: workflows themselves are source code.

**How to read it:** successful checks on an old base are insufficient when
GitHub says the branch is behind. Update the PR branch, resolve any conflicts
and wait for checks on that revision. Zero approvals permits the owner to
merge a passing PR; it is not independent code review or certification.

## Metadata changes still run CI

Build numbers, documentation and version-only changes use the normal PR
checks. No path exemption or synthetic success is provided. Do not use
`[skip ci]`, `[ci skip]`, `[no ci]`, `[skip actions]`, `[actions skip]` or a
`skip-checks` trailer on a PR intended to merge. GitHub leaves required
checks pending when these instructions suppress workflows. Push a new commit
without a skip instruction to start the checks.

This prevents future unchecked merges through the normal protected path.
It does not rewrite Scorecard's historical sample: an earlier unchecked PR
remains historical evidence until it leaves the rolling window. A smaller
metadata-only test path would need real validation and stable required-check
semantics before adoption; marking omitted tests successful is not a policy.

## CI uses fresh hosted virtual machines

Every job selects `ubuntu-24.04`, a standard GitHub-hosted VM. Each job gets
a fresh machine; no persistent Curator lab runner is part of the final pool.
There are always eight independent Go race/coverage partitions. Actions
remain SHA-pinned, default workflow tokens are read-only, and checkouts do
not retain write credentials. PR workflow changes require owner scrutiny
before external-contributor runs are approved.

The repository's `CI_RUNNER_MODE` variable is set to `github` so older
workflow revisions also route new work to hosted machines during rollout.
The workflow source no longer offers a lab fallback. Runner registration is
the server-side boundary: a malicious workflow cannot acquire a lab machine
by substituting its labels when the repository has no lab registrations.
This does not change runner access for other repositories.

**Transition:** pre-existing main workflows which already selected lab
machines are allowed to finish, including dependent aggregation jobs. Both
Curator lab registrations are removed after those workflows drain; an idle
moment between jobs is not proof that the workflow has finished. Do not
terminate a busy job or remove its captured runner class mid-workflow.
Completion evidence must show an empty repository runner inventory.

## Recovery requires an explicit administrator action

There is no standing approval or CI bypass actor. The repository owner
retains the administrative ability to edit protection during an actual
recovery incident. Save the complete settings before changing them, record
the reason, and restore and verify the protection afterward. This is not a
routine route for skipping CI or merging build-number changes.

Settings receipts and runner before/after inventories are kept with the
security follow-up task. Editing this document alone does not change GitHub
settings. Independent-review requirements and the optional OpenSSF Best
Practices application remain separate owner decisions.
