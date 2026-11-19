# Activity actions — delivery status

**Review fixes implemented, recheck pending · Updated 2026-11-18 · Effort E-FDF467296C**

Codex owns the implementation and coordinates the Runner and Curator changes.
The Activity changes are awaiting their pull requests, merge, and cleanup.
The owned external-drive workspaces remain active. Runner’s automatic failure
cleanup dependency merged as [PR #341](https://github.com/pjunod/runner/pull/341)
at `bcc7b69d`. Integration is in progress.

| Milestone | Status on 2026-11-18 |
|---|---|
| Proposal and architecture review | The owner approved the rendered direction. Astra 6.2 completed the architecture review against Curator, Runner, and Cinema. |
| Runner disposition | Identity-bound stop and disposal and the canonical owner request are implemented. Dependency integration is in progress. |
| Curator operations | Durable intent, normalized per-target operation records, lineage and late-publication fences, and the cleanup worker are implemented. |
| Activity on web and mobile | Both clients use the server’s reasons, prerequisites, and action descriptors. Frozen bulk selections and operation results are implemented. |
| Storage and History | Storage uses root-only observations. Activity History opens a log of immutable recorded job events. |
| Regression coverage | API, database, worker, and browser cases are written for frozen selection, replay, partial results, file preservation, and delayed Runner receipts. Execution is pending. |
| Adversarial code review | Completed. Fixes for 14 grouped findings are implemented. Adversarial recheck is pending. |
| Final validation | Pending the adversarial recheck and dependency integration. No tests have run. |
| Pull requests and merge | Pending the review and validation gates. Related changes will ship in one batch per repository. |
| Workspace cleanup | Pending merge. The implementation workspaces remain available for review and verification. |

The architecture review found that clearing a queue row can record cancellation before
Runner has stopped or removed its files. The design therefore preserves the
real transfer outcome, records the user's delete intent durably, and verifies
each owner's cleanup before reporting success. Installed Library media and
unrelated files remain protected. See the [pipeline contract](PIPELINE_CONTRACT.md).

The architecture report is an external receipt. Its path on the development
machine is
`/Volumes/Untitled/agent/gpt/workspaces/curator-attention-proposal-20261118/astra-review/ARCHITECTURE_REVIEW.md`.
