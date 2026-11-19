# Operation concurrency status

Companion to the [architecture blueprint](architecture.md). This page tracks
the combined Wanted and file operation change.

| Work | Owner | State |
|---|---|---|
| Wanted admission, leasing, API and regression coverage | Codex | Implemented, pending final review |
| Wanted web and native, interactive search controls | Codex with Sol | Implemented, pending final review |
| Offload admission, durable queue, settlement and web/native status | Codex with Sol | In progress |
| Season provider readiness, plan dispatch, Re-match recovery and row controls | Codex with Sol | Implemented, pending final review |
| Merge-candidate adversarial review | Astra | Pending complete diff |
| Fast-lane checks and merge | Codex | Pending review and fixes |

GitHub branch is `codex/operation-concurrency`. The combined PR will name
the owners, remaining checks and any merge blocker. Search and file operations
will reuse the existing concurrency and recovery contracts for indexers,
downloads and filesystem writers. A queued Offload must wait for execution
authority before changing monitoring or deleting bytes.

## Follow-up boundaries

Item Auto search has pack and additional-copy semantics separate from Wanted's
exact target search. Its established endpoint remains available. The parallel
Activity effort owns durable import actions. Further partitioning of reviewed
storage actions and same-title file writers depends on evidence about shared
paths, provider usage and resource contention.
