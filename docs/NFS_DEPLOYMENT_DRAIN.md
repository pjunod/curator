# Deploying after an import drain

**Status:** implementation under adversarial review; runtime qualification and measured stop wait pending · **Updated:** 2026-11-01

Companion to [ADR 0025](adr/0025-import-cancellation-and-deployment-drain.md),
[qualification protocol](NFS_QUALIFICATION_PROTOCOL.md), and
[implementation status](NFS_SHUTDOWN_IMPLEMENTATION_STATUS.md).

## 1. Pause admission and let admitted work finish

`POST /api/v1/system/deployment-drain` takes a caller-generated 64-character
hexadecimal `ownerToken`. Repeating it with the same token recovers a lost
response. Another owner receives 409. The response and read-only GET expose
`accepting`, `draining`, `drained`, or terminal `stopping`, an opaque generation,
active work and elapsed time. The owner secret is never returned by GET.

Draining rejects new native/compat mutations with 503 and pauses scheduler,
job claims and acquisition producers. Registered children retain their actual
ownership through file close, bookkeeping and heartbeat joins. Status remains
available. Unstarted durable queue entries remain available after restart or
resume. Operational telemetry continues; drained does not mean every database
write has stopped.

`DELETE /api/v1/system/deployment-drain` with the exact `ownerToken` and
`generation` explicitly resumes admission. A stale owner/generation or terminal
shutdown cannot reopen it. An operator wait timeout leaves admission paused.

## 2. Use the exact-container deployment command

Run from an authorized deployment checkout with its real Compose file:

```sh
python3 scripts/deploy-drained.py deploy \
  --compose-file /absolute/path/to/compose.yaml --service monarr \
  --receipt /private/persistent/path/deploy-receipt.json \
  --stop-wait "$MEASURED_POST_DRAIN_WAIT_SECONDS"
```

The stop wait is a positive value qualified for that deployment shape. No
production value has been selected yet. Set `MONARR_API_KEY` in the environment
when native API authentication requires it. The tool passes credentials and the
owner token through stdin to the captured container's built-in maintenance
client. They do not appear in process arguments. Preserve its private receipt
outside compiler/cache directories.

The tool builds before draining, captures the exact old container, binds Docker
context/daemon/service identity, disables its restart policy, sends TERM, waits
for Docker's actual exit result and verifies stopped state/PID 0. Only then does
it recreate the stopped service container. It never sends KILL or force-removes
a live container. A zombie leader, pending NFS thread, timeout or ambiguous exit
prevents replacement. The Compose-directory lock coordinates this command's
callers; it does not fence unrelated operators.

Resume waiting with the same receipt:

```sh
python3 scripts/deploy-drained.py resume --receipt /private/persistent/path/deploy-receipt.json
```

Before TERM, explicitly abort with `abort --receipt ...` to restore the old
restart policy and resume admission. After TERM, abort is refused. Resume revalidates the exact daemon/service/container and disabled restart policy,
then repeats TERM if that old container is still running. This recovers a crash
before signal delivery or a lost Docker response. Replacement still requires
actual exit.

## 3. First upgrade from an image without drain support

Older images do not recognize the maintenance subcommand; executing it can start
a second application process. The tool therefore checks image label
`org.pjunod.curator.deployment-drain=v1` before any exec or build and refuses an
unsupported old image.

For the first upgrade, build the new image without replacing the old service,
coordinate the deployment so no other operator starts a replacement, capture
and verify the old container's full ID, and disable that exact container's
restart policy. Send TERM to the captured ID and wait for actual exit. Verify
Docker reports `Running=false`, `Pid=0`, `Status=exited`, a nonzero finished
timestamp and the same wait/inspect exit code before recreating the stopped
service. If NFS blocks exit, retain the container and wait. This initial upgrade
cannot provide the new reversible drain API until the supported image runs.

## 4. Cancellation retains custody

Activity sends `generationKey` and a decimal-string import `epoch`. A stale
request receives 409. HTTP 202 means intent persisted and actual writer or
publication reconciliation remains pending; 204 means cancellation finalized.
Committed files remain in the library. Source, uncertain targets and interrupted
stages remain protected from automatic pruning/deletion. Explicit retry gets a
new epoch and stage identity while preserving Runner acquisition identity.

Review retained import opens an in-viewport dialog. It lists publication and
stage evidence without NFS reads. Finalizing uncertain authorization requires an
explicit acknowledgement that every external writer was fenced and the exact
displayed publication IDs were reviewed. It revokes authority and retains bytes.
Retained targets require explicit verified Library adoption or an operator's
attestation that listed bytes were independently handled. The application does
not delete those bytes or infer cleanup permission from a filename or digest.

## 5. Supported storage envelope

Storage work remains in Curator. A measured post-drain wait is an observed
engineering margin, not an outage bound. Hard NFS can block restart during a NAS
outage. Automatic cross-node stage resume and failover recovery remain outside
the qualified envelope. Basic advisory-lock exclusion does not prove namespace,
lease, delegation or server-recovery behavior.

## Measured nuc3 Docker wait

The [qualification results](NFS_QUALIFICATION_RESULTS.md) record six successful
post-drain exits between 0.113 and 0.194 seconds for the inspected unlimited-memory
Docker shape. Set `MEASURED_POST_DRAIN_WAIT_SECONDS=30` for that shape. This adds
scheduling/cleanup margin; it is a wait before returning a pending receipt,
not permission to KILL or replace a live process. Direct TERM without draining
and the failed 512 MiB shape are outside this selection. NAS outages can still
block restart until actual I/O and process exit complete.
