# Bad media reports and replacement operations

Companion to [DOWNLOAD_LIFECYCLE_PROPOSAL.md](DOWNLOAD_LIFECYCLE_PROPOSAL.md);
this page records the implemented HTTP contract for library reports and
explicit replacements.

Curator exposes generation-bound review and replacement endpoints for the
library UI and Cinema. The implementation is asynchronous: reports only flag
files for review, while an explicitly accepted replacement operation owns its
selected copy and episode scope through cleanup, search, import, and retry.

All paths below are relative to `/api/v1`. Native API key clients send
`X-Api-Key: <configured-key>` (the compatibility query parameter is
`?apikey=...`); browser sessions use the existing session cookie. When auth is
disabled, the native API does not require credentials.

## Report a library file

`POST /library/{id}/files/{fileId}/report` records evidence against the exact
current file generation. It does not change quality, monitoring, coverage,
blacklists, or acquisition state.

```json
{
  "fileGeneration": "current-library-file-generation",
  "reasonCode": "corrupt_media",
  "reason": "Playback reports corrupt frames.",
  "evidence": { "player": "example" }
}
```

The server returns `204` when saved and `409` when the file generation is
stale. Open flags are separate from the planner's media-quality and coverage
state; automatic redundancy cleanup retains a flagged generation.

`GET /library/{id}/replacement-operations` returns durable replacement
operations and open reports for the item. It remains useful after an Activity
row or the reported file has been removed. Resolve an open report with
`POST /library/{id}/review-flags/{flagId}/resolve`, passing its exact
`fileGeneration`; resolving a report only closes that review flag.

## Prepare an existing file for replacement

Legacy library files may not have original release provenance. Before an
explicit replacement, prepare a read-only byte observation:

`POST /library/{id}/files/{fileId}/replacement-observations`

```json
{ "requestId": "observation-action-id", "fileGeneration": "current-generation" }
```

The response is `202` with an `observationId`; replaying the same request
returns `200`. Hashing and file identity checks run in the background. Poll
`GET /replacement-observations/{observationId}` until `state` is `ready` or,
for an explicitly bad file, `absent`.
`sha256`, `size`, and `fileGeneration` bind the subsequent user decision to
those observed bytes. `stale` or `failed` observations cannot authorize a
replacement. This preparation is read-only and does not change library state.

An `absent` observation is allowed only when the root has a durable storage
identity pin enrolled by a prior verified, present-file observation, or that
pin can be recovered from an earlier ready observation of the exact file
generation. Curator compares the currently mounted source to that pin and
checks absence under an anchored parent. It carries no fabricated digest and
can authorize only `replace_bad`, which reconciles that selected stale library
record before searching. A legacy file already absent without prior trusted
storage evidence remains untouched; the observation fails with
`storage identity unverified; restore expected storage`. An accessible path
below a different mount fails with `expected storage identity changed; restore
expected storage`. Curator never enrolls a storage identity based on absence.

## Start and inspect a replacement

`POST /library/{id}/replacement-operations` requires an explicit request ID,
scope, target copy, anchor `sourceFileId`, intent, blacklist choice, reason,
and observed file list.
Series operations require a nonempty `episodeIds` list when `scope` is
`episodes`; movie/book and whole-download actions use `scope: "download"`.
The route item ID is authoritative; the body carries `copyId` but not a second
item ID.

```json
{
  "requestId": "replacement-action-id",
  "sourceGeneration": "original-download-generation",
  "sourceTransfer": "t-123-a1b2c3",
  "scope": "episodes",
  "copyId": 0,
  "sourceFileId": 34,
  "episodeIds": [1805],
  "intent": "replace_bad",
  "blacklistSourceRelease": false,
  "reasonCode": "corrupt_media",
  "reason": "The selected episode is corrupt.",
  "observedFiles": [
    { "id": 34, "fileGeneration": "library-file-generation", "observationId": "observation-id" }
  ]
}
```

`blacklistSourceRelease` must be explicitly `true` or `false`; there is no
default. `true` is accepted only when the source has a stable indexer ID and
GUID. `false` excludes the source from this replacement operation's retries
without globally blacklisting it. Legacy media with no source GUID can still
be explicitly replaced with the choice set to `false`; Curator does not infer
or fabricate an origin. `sourceFileId` anchors the selected source even when
its provenance is unknown. For an unknown legacy source, `scope: "download"`
authorizes that one anchored file; it does not treat other files with similar
titles as part of the same release. Source generation and transfer are
optional for legacy files and, when supplied, must match the anchored library
provenance.
Every replacement-operation response includes the saved
`blacklistSourceRelease` boolean. It reports the choice made for that
operation, including in detail and history responses; it does not reflect a
later change to the global blacklist.
Retry exclusion uses exact indexer ID plus GUID when both sides have complete
stable identity. With incomplete identity, normalized release title plus
indexer name is an explicitly ambiguous request-local fallback; Curator never
creates a global blacklist from that guess. The operation response leaves
`sourceGuid` empty and `sourceIndexerId` zero when provenance is unknown.

Each observed file must belong to the selected copy and cover at least one
selected episode. Include every current file in the selected source scope; the
UI prepares all files before submission. An observation must be ready and
match the exact current file path, generation, and bytes, or be an `absent`
observation for an explicit bad-file action. A changed or omitted file returns
`409`; refresh the scope and prepare new observations before retrying. Request
IDs are idempotent:
same ID and body replay the saved operation; changed content returns `409`.
Conflicting active operations over the same file/episode scope also return
`409`; independent sibling episodes may proceed separately.

New operations return `202`; an exact replay returns `200` with the same
operation ID, while changed content under that ID returns `409`. GET
`/replacement-operations/{operationId}` returns current state, source/copy and
episode scope, affected file snapshots, retry timing, and errors. States
include `accepted`, `removing`, `cleanup_blocked`, `searching`,
`waiting_for_release`, `replacing`, `complete`, `cancelled`, and `failed`.
Automatic retry timing is persisted and survives restart. A blocked warning is
set after thirty minutes.

For `replace_bad`, selected disposable bytes are verified and removed before
replacement search. A physical file shared with an unselected good episode is
retained and only the selected episode association is detached. For
`replace_quality`, the usable current file remains until a selected replacement
is committed. Search prefers individual episodes and can fall back to a pack;
the import and excess-output cleanup remain limited to selected episode scope.
The operation stays bound to its selected copy, including additional copies.

The Media Detail UI exposes **Report**, **Mark bad & replace**,
**Replace quality**, and **Delete** separately. Reports do not start
acquisition. Open reports and operation status appear in the item-level review
panel after a dialog closes or the source file is removed. A report can be
explicitly resolved there. Replacement requires ready observations for all
current files in the selected scope, a selected episode scope for series, and
an affirmative **Yes** or **No** blacklist choice. Cinema can submit an
automatic report flag, but replacement remains an explicit user action and
uses the same observation and blacklist prompt flow.

## Manual acquisition compatibility

The existing manual release route is `POST /grab` (full path
`/api/v1/grab`). Every manual request must now say
`"intent": "additional_copy"` and include a stable, caller-generated
`requestId` that is reused for retries of that one action:

```json
{
  "mediaItemId": 42,
  "copyId": 0,
  "season": 1,
  "episode": 3,
  "title": "Example.Show.S01E03.1080p.WEB-DL-GROUP",
  "downloadUrl": "https://indexer.example/release/123",
  "indexer": "Example Indexer",
  "protocol": "torrent",
  "intent": "additional_copy",
  "requestId": "6ce15f80-5c2d-4f71-93aa-3132da27e099"
}
```

The first accepted request returns `201 {"id": 123}`. An exact retry
reuses the same reserved copy, download row, transfer token, and client
submission; changed content under the same `requestId` returns `409`. A
manual grab always creates an independent, unmonitored copy in the selected
library/profile target, including when the primary copy is already better.
It never upgrades or replaces the primary copy. A permanent manual failure
does not start an automatic replacement. If its Activity row was dismissed
after completion, replay returns `409` with code `request_retained`; start a
new deliberate action with a fresh request ID instead of allocating another
copy by accident.

If a release is already blocked, the first request returns `409` with code
`blacklist_confirmation_required`. After the user confirms a one-time
override, retry with `downloadAnyway: true` and a fresh `requestId`, because
the override is a distinct action fingerprint. That override does not remove
the automatic blacklist. Without the override, the request stays blocked.
Older external callers that omit either `intent` or `requestId` now receive
`400`; they must send the explicit additional-copy intent and stable key.

## Scope and compatibility

These routes are described in `internal/api/openapi.yaml`. Their current paths
are `/library/{id}/files/{fileId}/report`,
`/library/{id}/files/{fileId}/replacement-observations`,
`/replacement-observations/{observationId}`,
`/library/{id}/replacement-operations` (POST to create, GET to list),
`/replacement-operations/{operationId}`, and
`/library/{id}/review-flags/{flagId}/resolve`. They use Curator's existing API
authentication and permissions. No endpoint performs an unchecked bulk
approval of historical backlog.
