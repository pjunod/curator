# Operation concurrency status

Companion to the [architecture blueprint](architecture.md). This page tracks
the combined Wanted and file operation change.

| Work | Owner | State |
|---|---|---|
| Wanted admission, leasing, API and regression coverage | Codex | In progress |
| Wanted web and native, interactive search controls | Codex with Sol | In progress |
| Offload admission, durable queue, settlement and web/native status | Codex with Sol | In progress |
| Related dispatch and action bottlenecks | Codex | Pending integration assessment |
| Merge-candidate adversarial review | Astra | Pending complete diff |
| Fast-lane checks and merge | Codex | Pending review and fixes |

GitHub branch is `codex/operation-concurrency`. The combined PR will name
the owners, remaining checks and any merge blocker. Search and file operations
will reuse the existing concurrency and recovery contracts for indexers,
downloads and filesystem writers. A queued Offload must wait for execution
authority before changing monitoring or deleting bytes.
