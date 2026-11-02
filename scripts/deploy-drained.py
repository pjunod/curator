#!/usr/bin/env python3
"""Drain one captured Compose container, TERM it, prove exit, then replace it.

Timeouts preserve custody. This command never sends KILL or removes a live
container. Use a persistent receipt outside caches; it contains an owner secret.
MONARR_API_KEY supplies native API credentials when authentication is required.
"""

import argparse
import contextlib
import fcntl
import json
import os
from pathlib import Path
import secrets
import subprocess
import sys
import tempfile
import time
import uuid


class Deployment:
    def __init__(self, receipt, run=subprocess.run, clock=time.monotonic, sleep=time.sleep):
        self.path = Path(receipt)
        self.run, self.clock, self.sleep = run, clock, sleep
        self.record = {}
        self.measurement_session = uuid.uuid4().hex
        self.term_started = None

    def save(self, phase):
        self.record["phase"] = phase
        self.record["updatedAt"] = time.time()
        self.record.setdefault("phaseTimes", {}).setdefault(phase, self.record["updatedAt"])
        if self.path.is_symlink():
            raise RuntimeError("receipt must not be a symlink")
        # Atomic durable replacement preserves the owner token across a crash.
        temporary = None
        try:
            with tempfile.NamedTemporaryFile(mode="w", dir=self.path.parent, prefix=".monarr-receipt-", delete=False) as out:
                temporary = out.name
                os.fchmod(out.fileno(), 0o600)
                json.dump(self.record, out, indent=2)
                out.flush()
                os.fsync(out.fileno())
            os.replace(temporary, self.path)
            temporary = None
            directory = os.open(self.path.parent, os.O_RDONLY)
            try:
                os.fsync(directory)
            finally:
                os.close(directory)
        finally:
            if temporary is not None:
                os.unlink(temporary)

    def command(self, *args, payload=None, timeout=30):
        result = self.run(["docker", "--context", self.record["context"], *args],
                          input=payload, text=True, capture_output=True,
                          timeout=timeout, check=True)
        return result.stdout.strip()

    def compose(self, *args, timeout=30):
        return self.command("compose", "-f", self.record["composeFile"], *args, timeout=timeout)

    def inspect(self):
        rows = json.loads(self.command("inspect", self.record["containerId"]))
        if len(rows) != 1 or rows[0]["Id"] != self.record["containerId"]:
            raise RuntimeError("captured container identity changed")
        return rows[0]

    def verify_daemon_and_service(self):
        if self.command("info", "--format", "{{.ID}}") != self.record["daemonId"]:
            raise RuntimeError("Docker daemon identity changed")
        current = self.compose("ps", "--all", "--quiet", self.record["service"])
        if current != self.record["containerId"]:
            raise RuntimeError("Compose service no longer identifies the captured container")

    def control(self, operation):
        original = self.inspect()
        if (original["Config"].get("Labels") or {}).get("org.pjunod.curator.deployment-drain") != "v1":
            raise RuntimeError("captured image has no drain capability; use the documented first-upgrade actual-exit procedure")
        payload = json.dumps({"apiKey": os.environ.get("MONARR_API_KEY", ""),
                              "ownerToken": self.record["ownerToken"],
                              "generation": self.record.get("generation", "")})
        # Execute the old image's built-in client inside that exact container.
        # There is no externally supplied URL that could drain a different node.
        return json.loads(self.command("exec", "-i", self.record["containerId"],
                                       "/monarr", "maintenance", operation,
                                       payload=payload, timeout=20))

    def prepare(self, compose_file, service, stop_wait, drain_timeout):
        if self.path.exists():
            raise RuntimeError("receipt exists; use resume or abort, or choose a new receipt")
        context = self.run(["docker", "context", "show"], text=True, capture_output=True,
                           timeout=15, check=True).stdout.strip()
        self.record = {"context": context, "composeFile": str(Path(compose_file).resolve()),
                       "service": service, "stopWait": stop_wait, "drainTimeout": drain_timeout,
                       "ownerToken": secrets.token_hex(32)}
        self.record["daemonId"] = self.command("info", "--format", "{{.ID}}")
        self.record["containerId"] = self.compose("ps", "--all", "--quiet", service)
        if not self.record["containerId"] or "\n" in self.record["containerId"]:
            raise RuntimeError("exactly one existing service container is required")
        original = self.inspect()
        if not original["State"]["Running"]:
            raise RuntimeError("initial deploy requires a running old container")
        if (original["Config"].get("Labels") or {}).get("org.pjunod.curator.deployment-drain") != "v1":
            raise RuntimeError("old image cannot drain; do not execute an unknown subcommand; use first-upgrade actual-exit procedure")
        self.record["restartPolicy"] = original["HostConfig"]["RestartPolicy"]
        # Build before pausing the application; no replacement happens here.
        self.compose("build", service, timeout=None)
        self.verify_daemon_and_service()
        self.save("prepared")

    def load(self):
        if self.path.is_symlink():
            raise RuntimeError("receipt must not be a symlink")
        self.record = json.loads(self.path.read_text())

    def drain_and_replace(self):
        self.verify_daemon_and_service()
        phase = self.record["phase"]
        if phase in ("aborted", "replaced"):
            raise RuntimeError("this receipt is already terminal")
        if phase not in ("term_requested", "exit_verified"):
            status = self.control("drain")  # repeatable if the original response was lost
            if self.record.get("generation") and status["generation"] != self.record["generation"]:
                raise RuntimeError("maintenance generation changed")
            self.record["generation"] = status["generation"]
            self.save("draining")
            deadline = self.clock() + self.record["drainTimeout"]
            while status["phase"] != "drained":
                print(json.dumps({k: status[k] for k in ("phase", "active", "elapsedSeconds", "work")}), flush=True)
                if status["generation"] != self.record["generation"] or status["phase"] != "draining":
                    raise RuntimeError("maintenance ownership/phase changed")
                if self.clock() >= deadline:
                    raise TimeoutError("drain is still pending; admission remains paused; resume waiting or explicitly abort")
                self.sleep(1)
                status = self.control("status")
            self.save("drained")
            self.verify_daemon_and_service()
            # Prevent Docker's restart policy from creating another old writer.
            self.save("restart_disable_requested")
            self.command("update", "--restart=no", self.record["containerId"])
            self.term_started = self.clock()
            self.record["termMeasurementSession"] = self.measurement_session
            self.save("term_requested")  # persist before signaling, including a lost client reply
            try:
                self.command("kill", "--signal=TERM", self.record["containerId"])
            except subprocess.CalledProcessError:
                print("TERM call failed; actual-exit verification is still required", file=sys.stderr)
        self.verify_daemon_and_service()
        current = self.inspect()
        if current["HostConfig"]["RestartPolicy"]["Name"] != "no":
            raise RuntimeError("captured container restart policy is not disabled")
        if current["State"]["Running"]:
            # A persisted intent may precede a crash or lost Docker response.
            # The supported image retains SIGTERM handling through cleanup;
            # repeating TERM requests cancellation without sending KILL.
            try:
                self.command("kill", "--signal=TERM", self.record["containerId"])
            except subprocess.CalledProcessError:
                print("Repeated TERM failed; actual exit is still required", file=sys.stderr)
        print("Waiting for actual old-container exit; no KILL or forced removal will be sent", flush=True)
        exit_code = int(self.command("wait", self.record["containerId"], timeout=self.record["stopWait"]))
        old = self.inspect()
        state = old["State"]
        if (state["Running"] or state["Pid"] != 0 or state["Status"] != "exited"
                or state["ExitCode"] != exit_code or state["FinishedAt"].startswith("0001-")
                or old["HostConfig"]["RestartPolicy"]["Name"] != "no"):
            raise RuntimeError("actual stopped state was not established; replacement refused")
        self.record["exit"] = state
        self.record["termToVerifiedExitSeconds"] = self.clock()-self.term_started if self.term_started is not None else None
        if self.term_started is None:
            self.record["exitTimingLimitation"] = "resumed receipt; original signal-to-exit monotonic interval unavailable"
        self.save("exit_verified")
        self.verify_daemon_and_service()
        self.compose("up", "--detach", "--no-deps", "--no-build", "--force-recreate", self.record["service"], timeout=120)
        new_id = self.compose("ps", "--quiet", self.record["service"])
        if not new_id or "\n" in new_id or new_id == self.record["containerId"]:
            raise RuntimeError("replacement identity was not established")
        self.record["replacementId"] = new_id
        self.save("replaced")
        print("Replacement started: " + new_id, flush=True)

    def abort(self):
        self.verify_daemon_and_service()
        if self.record["phase"] in ("term_requested", "exit_verified", "replaced", "aborted"):
            raise RuntimeError("cannot reopen admission after TERM or a terminal receipt")
        if not self.inspect()["State"]["Running"]:
            raise RuntimeError("captured old process is no longer running")
        # Recover a lost initial response using the same owner, never another
        # owner's generation. The application checks terminal shutdown too.
        status = self.control("drain")
        if self.record.get("generation") and status["generation"] != self.record["generation"]:
            raise RuntimeError("maintenance generation changed")
        self.record["generation"] = status["generation"]
        policy = self.record["restartPolicy"]
        restart = policy["Name"]
        if restart == "on-failure" and policy.get("MaximumRetryCount"):
            restart += ":" + str(policy["MaximumRetryCount"])
        self.command("update", "--restart=" + restart, self.record["containerId"])
        self.control("abort")
        self.save("aborted")
        print("Admission resumed for the captured old container", flush=True)


@contextlib.contextmanager
def deployment_lock(compose_file):
    # Keep one lock per Compose directory, shared by this tool's cooperating
    # callers. It does not fence unrelated operators or a second Docker client.
    path = Path(compose_file).resolve().parent / ".monarr-deployment.lock"
    fd = os.open(path, os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, "a") as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError as exc:
            raise RuntimeError("another deployment command owns this Compose directory") from exc
        yield


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("deploy", "resume", "abort"))
    parser.add_argument("--receipt", required=True, help="persistent private receipt path; retain across timeouts")
    parser.add_argument("--compose-file")
    parser.add_argument("--service", default="monarr")
    parser.add_argument("--stop-wait", type=float, help="required measured post-drain exit wait, in seconds; never a KILL deadline")
    parser.add_argument("--drain-timeout", type=float, default=1800, help="operator wait budget; timeout leaves admission paused")
    args = parser.parse_args()
    if args.action == "deploy" and (not args.compose_file or not args.stop_wait or args.stop_wait <= 0 or args.drain_timeout <= 0):
        parser.error("deploy requires --compose-file and a positive measured --stop-wait / --drain-timeout")
    try:
        deploy = Deployment(args.receipt)
        if args.action != "deploy":
            deploy.load()
        compose_file = args.compose_file if args.action == "deploy" else deploy.record["composeFile"]
        with deployment_lock(compose_file):
            if args.action == "deploy":
                deploy.prepare(args.compose_file, args.service, args.stop_wait, args.drain_timeout)
            else:
                deploy.load()
                if args.stop_wait is not None:
                    if args.stop_wait <= 0:
                        raise ValueError("--stop-wait must be positive")
                    deploy.record["stopWait"] = args.stop_wait
            if args.action == "abort":
                deploy.abort()
            else:
                deploy.drain_and_replace()
    except (RuntimeError, ValueError, OSError, subprocess.SubprocessError) as exc:
        # Do not dump subprocess input: it contains credentials/owner secrets.
        print("Deployment not completed: " + str(exc), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
