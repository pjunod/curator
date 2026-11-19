# Operation concurrency status

Companion to the [architecture blueprint](architecture.md). This page tracks
the combined Wanted and file operation change.

| Work | Owner | State |
|---|---|---|
| Wanted admission, leasing, API and regression coverage | Codex | Implemented |
| Wanted web and native, interactive search controls | Codex with Sol | Implemented |
| Offload admission, durable queue, settlement and web/native status | Codex with Sol | Implemented |
| Season provider readiness, plan dispatch, Re-match recovery and row controls | Codex with Sol | Implemented |
| Merge-candidate adversarial review | Astra | Complete, findings addressed |
| Fast-lane checks and merge | Codex | See the combined GitHub PR for live check and merge state |

The [combined GitHub PR][pr] records ownership, verification and merge state.
The branch is `codex/operation-concurrency`. Search and file operations
will reuse the existing concurrency and recovery contracts for indexers,
downloads and filesystem writers. A queued Offload must wait for execution
authority before changing monitoring or deleting bytes.

## Follow-up boundaries

Item Auto search has pack and additional-copy semantics separate from Wanted's
exact target search. Its established endpoint remains available. The parallel
Activity effort owns durable import actions. Further partitioning of reviewed
storage actions and same-title file writers depends on evidence about shared
paths, provider usage and resource contention.

## Astra review

Astra reviewed the combined diff before tests. Findings covered anchored
filesystem authority, activation against changed catalog or Cinema state,
Stop after interrupted unlink, dispatcher starvation, and request replay.
The fixes add an unlink journal boundary, use exact file generation when
retiring catalog rows, and let eligible Offloads use released execution slots.


[pr]: https://github.com/pjunod/curator/pulls?q=is%3Apr+head%3Acodex%2Foperation-concurrency
