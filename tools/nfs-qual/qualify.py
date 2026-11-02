#!/usr/bin/env python3
"""Run reviewed scratch-only NFS writeback measurements on a Linux Docker host.

Run from a persistent, task-owned results directory. The source and target
directories are exclusively created using the run ID. Never alters production
containers, exports, mount settings, caches globally, or network availability.
"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import random
import re
import stat
import subprocess
import sys
import time
import urllib.request
import uuid

INCIDENT = 1_400_000_000
LARGE = 10 * 1024**3
POLICIES = [("whole", 1024**3), ("periodic", 256*1024**2),
            ("periodic", 1024**3), ("concurrent", 1024**3), ("range", 1024**3)]


def command(*args, timeout=30):
    return subprocess.run(args, text=True, capture_output=True, check=True,
                          timeout=timeout).stdout.strip()


def save(path, value):
    temporary = path.with_suffix(path.suffix + ".new")
    with temporary.open("w") as out:
        json.dump(value, out, indent=2)
        out.flush()
        os.fsync(out.fileno())
    os.replace(temporary, path)
    fd = os.open(path.parent, os.O_RDONLY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def mounts():
    result, current = {}, None
    for line in Path("/proc/self/mountstats").read_text().splitlines():
        if line.startswith("device "):
            words = line.split()
            current = words[words.index("on")+1]
            result[current] = {"header": line}
        elif current and line.strip().startswith("age:"):
            result[current]["age"] = float(line.split(":",1)[1])
        elif current and line.strip().startswith("opts:"):
            result[current]["options"] = line.strip()
        elif current and line.strip().startswith(("WRITE:", "READ:", "COMMIT:", "CLOSE:", "DELEGRETURN:")):
            name, numbers = line.strip().split(":", 1)
            result[current][name] = [int(x) for x in numbers.split()]
    return result


def memory():
    info = {}
    for line in Path("/proc/meminfo").read_text().splitlines():
        key, value = line.split(":", 1)
        if key in ("Dirty", "Writeback", "MemAvailable", "MemTotal"):
            info[key] = int(value.split()[0]) * 1024
    vm = dict(line.split() for line in Path("/proc/vmstat").read_text().splitlines())
    for key in ("nr_dirty_threshold", "nr_dirty_background_threshold"):
        info[key] = int(vm[key]) * os.sysconf("SC_PAGE_SIZE")
    return info


def production_imports():
    # Count only operational stages; never save media titles or source paths.
    try:
        with urllib.request.urlopen("http://127.0.0.1:7676/api/v1/system/transfers", timeout=0.25) as response:
            rows = json.load(response)
        if isinstance(rows, dict):
            rows = rows.get("transfers", [])
        return sum(row.get("stage") == "importing" for row in rows)
    except (OSError, ValueError, TypeError):
        return None


def cgroup_snapshot(pid):
    try:
        relative = next(line.split("::", 1)[1] for line in Path(f"/proc/{pid}/cgroup").read_text().splitlines() if "::" in line)
        base = Path("/sys/fs/cgroup") / relative.lstrip("/")
        stats = dict(line.split() for line in (base / "memory.stat").read_text().splitlines())
        states = {}
        for task in Path(f"/proc/{pid}/task").iterdir():
            fields = (task / "stat").read_text().rsplit(")", 1)[1].split()
            states[task.name] = fields[0]
        return {"memoryMax": (base / "memory.max").read_text().strip(),
                "memoryCurrent": int((base / "memory.current").read_text()),
                "fileDirty": int(stats.get("file_dirty", 0)),
                "fileWriteback": int(stats.get("file_writeback", 0)), "taskStates": states}
    except (OSError, StopIteration, ValueError):
        return None


def events(path):
    rows = []
    for line in path.read_text().splitlines():
        try:
            rows.append(json.loads(line))
        except ValueError:
            pass  # an interrupted final log line is not a successful event
    return rows


def confirmed_phase_coverage(record):
    stop = record.get("stop")
    received = [e for e in record.get("events",[]) if e["phase"]=="signal-received"]
    return bool(stop and stop["signal"]=="TERM" and record.get("signalSent")
        and record.get("waitCode")==2 and not record.get("state",{}).get("OOMKilled",False)
        and received and received[0].get("signal_number")==15
        and received[0].get("observed_phase")==stop["phase"]
        and any(e["phase"]=="finished" and e["result"]=="cancelled" for e in record.get("events",[])))


def attribute_trace(path, record):
    result = {"valid": False, "reasons": [], "ownWriteBytes": 0,
              "foreignWriteBytes": 0, "ownSourceReadBytes": 0}
    created = next((e for e in record["events"] if e["phase"]=="target-created"),None)
    source = next((e for e in record["events"] if e["phase"]=="source-opened"),None)
    ready, done = record.get("traceReady"), record.get("traceDone")
    if not created or not source or not ready or not done:
        result["reasons"].append("trace/descriptor identity evidence missing")
        return result
    if ready["ino64"] != "Y" or done.get("stopReason") != "controller" or done["endWall"] < record["exitVerifiedAt"]:
        result["reasons"].append("trace inode mode or temporal coverage unknown")
    if created["device"] != ready["device"] or source["device"] != ready["sourceDevice"]:
        result["reasons"].append("trace and descriptor devices differ")
    for raw in done.get("perCPU",{}).values():
        if any(int(value)>0 for value in re.findall(r"^(?:overrun|commit overrun|dropped events):\s*(\d+)",raw,re.M)):
            result["reasons"].append("trace records lost")
    pattern = re.compile(r"nfs_initiate_(write|read):.*?fileid=([0-9a-f]+):([0-9a-f]+):(\d+).*?count=(\d+)")
    for line in path.read_text().splitlines():
        match = pattern.search(line)
        if not match:
            continue
        operation, major, minor, fileid, count = match.groups()
        device = os.makedev(int(major,16),int(minor,16))
        fileid,count = int(fileid),int(count)
        if operation=="write" and device==created["device"]:
            key = "ownWriteBytes" if fileid==created["inode"] else "foreignWriteBytes"
            result[key] += count
        if operation=="read" and device==source["device"] and fileid==source["inode"]:
            result["ownSourceReadBytes"] += count
    if record["completedPolicyWork"] and not record["size"]*0.90 <= result["ownWriteBytes"] <= record["size"]*1.10:
        result["reasons"].append("own write trace coverage/retransmission ratio outside declared class")
    errors_pattern = re.compile(r"nfs_writeback_done:.*?error=(-?\d+).*?fileid=([0-9a-f]+):([0-9a-f]+):(\d+)")
    for line in path.read_text().splitlines():
        match = errors_pattern.search(line)
        if match:
            error, major, minor, fileid = match.groups()
            if int(error) and os.makedev(int(major,16),int(minor,16))==created["device"] and int(fileid)==created["inode"]:
                result["reasons"].append("task NFS write returned an error")
    result["valid"] = not result["reasons"]
    return result


class Qualification:
    def __init__(self, root, source_parent, target_parent, image):
        self.root = Path(root).resolve()
        self.root.mkdir(parents=True, exist_ok=False, mode=0o700)
        os.chmod(self.root, 0o700)
        self.run_id = uuid.uuid4().hex
        self.source = Path(source_parent).resolve() / (".codex-nfs-qual-" + self.run_id)
        self.target = Path(target_parent).resolve() / (".codex-nfs-qual-" + self.run_id)
        self.image = command("docker", "image", "inspect", image, "--format", "{{.Id}}")
        self.context = command("docker", "context", "show")
        endpoint = command("docker", "context", "inspect", self.context, "--format", "{{.Endpoints.docker.Host}}")
        daemon_name = command("docker", "info", "--format", "{{.Name}}")
        if not endpoint.startswith("unix://") or daemon_name.split(".")[0] != os.uname().nodename.split(".")[0]:
            raise RuntimeError("qualification requires the local host Docker daemon")
        os.environ["DOCKER_CONTEXT"] = self.context
        build_receipt = json.loads(Path(__file__).with_name("build_receipt.json").read_text())
        if build_receipt["imageId"]!=self.image:
            raise RuntimeError("image is not bound to the verified build receipt")
        for name, expected in build_receipt["sourceHashes"].items():
            if hashlib.sha256(Path(__file__).with_name(name).read_bytes()).hexdigest()!=expected:
                raise RuntimeError("bundle differs from verified image build receipt")
        self.manifest = {"runId": self.run_id, "source": str(self.source), "target": str(self.target),
                         "image": self.image, "buildReceipt": build_receipt, "requestedImage": image, "policies": POLICIES, "rounds": 5, "minimumConfirmedTERMTrials": 20, "maxStopAttempts": 60,
                         "incidentBytes": INCIDENT, "largeBytes": LARGE,
                         "budget": {"pairedThroughputRatio": 0.90, "candidateStopSeconds": 60,
                                    "foreignFraction": 0.05, "writeRPCOverheadAllowance": 4096},
                         "sourceHashes": {name: hashlib.sha256(Path(__file__).with_name(name).read_bytes()).hexdigest() for name in ("main_linux.go","qualify.py","trace_capture.py","bench")},
                         "kernel": os.uname().release, "dockerContext": self.context, "daemonName": daemon_name, "trials": [], "phase": "preparing"}

    def checkpoint(self):
        save(self.root / "manifest.json", self.manifest)

    def prepare(self):
        self.checkpoint()
        for parent in (self.source.parent, self.target.parent):
            stat = os.statvfs(parent)
            if stat.f_bavail * stat.f_frsize < 100*1024**3:
                raise RuntimeError("less than 100 GiB free on a qualification export")
        self.source.mkdir(mode=0o700)
        self.target.mkdir(mode=0o700)
        for directory in (self.source, self.target):
            with (directory / "owner.json").open("x") as out:
                json.dump({"runId": self.run_id, "purpose": "scratch-only NFS qualification"}, out)
        small, large = hashlib.sha256(), hashlib.sha256()
        written = 0
        with (self.source / "payload.bin").open("xb", buffering=0) as out:
            while written < LARGE:
                block = os.urandom(min(1024**2, LARGE-written))
                count = out.write(block)
                if count != len(block):
                    raise OSError("short source preparation write")
                large.update(block)
                if written < INCIDENT:
                    small.update(block[:min(len(block), INCIDENT-written)])
                written += count
                if written % (1024**3) == 0:
                    self.manifest["preparedSourceBytes"] = written
                    self.checkpoint()
            os.fsync(out.fileno())
        os.chmod(self.source / "payload.bin", 0o400)
        self.manifest["sourceSHA256"] = {str(INCIDENT): small.hexdigest(), str(LARGE): large.hexdigest()}
        self.manifest["initialMounts"] = mounts()
        self.manifest["initialMemory"] = memory()
        self.manifest["phase"] = "ready"
        self.checkpoint()

    def verify_owned(self, cid):
        item = json.loads(command("docker", "inspect", cid))[0]
        if item["Id"] != cid or item["Config"]["Labels"].get("com.pjunod.codex.nfs-qualification") != self.run_id:
            raise RuntimeError("container ownership mismatch; no signal or cleanup permitted")
        return item

    def trial(self, size, policy, window, shape, round_id, stop=None, close_stress=False):
        index = len(self.manifest["trials"])
        name = f"codex-nfs-qual-{self.run_id[:12]}-{index}"
        target = self.target / (f"trial-{index}.bin")
        record = {"index": index, "name": name, "size": size, "policy": policy,
                  "window": window, "shape": shape, "round": round_id, "stop": stop,
                  "closeStress": close_stress, "phase": "starting", "target": str(target),
                  "startedAt": time.time()}
        self.manifest["trials"].append(record)
        self.checkpoint()
        # Evict only this task's read-only source, never drop host caches.
        with (self.source / "payload.bin").open("rb") as source:
            os.posix_fadvise(source.fileno(), 0, 0, os.POSIX_FADV_DONTNEED)
        args = ["docker", "create", "--name", name, "--network", "none", "--read-only",
                "--user", f"{os.getuid()}:{os.getgid()}", "--label",
                "com.pjunod.codex.nfs-qualification=" + self.run_id,
                "--mount", f"type=bind,src={self.source},dst=/source,readonly",
                "--mount", f"type=bind,src={self.target},dst=/target"]
        if shape == "512MiB":
            args += ["--memory", "512m", "--memory-swap", "512m"]
        args += [self.image, "--size", str(size), "--policy", policy,
                 "--window", str(window), "--target", "/target/" + target.name]
        if close_stress:
            args.append("--close-stress")
        cid = command(*args)
        record["containerId"] = cid
        self.verify_owned(cid)
        before_raw = Path("/proc/self/mountstats").read_text()
        (self.root / f"trial-{index}.mountstats.before.txt").write_text(before_raw)
        before = mounts()
        trace_path = self.root / f"trial-{index}.trace"
        trace_id = uuid.uuid4().hex
        tracer = subprocess.Popen(["sudo", "-n", sys.executable, str(Path(__file__).with_name("trace_capture.py")),
                                   "--id", trace_id, "--output", str(trace_path),
                                   "--mount", str(self.target.parent), "--source-mount", str(self.source.parent)])
        ready_path = Path(str(trace_path)+".ready.json")
        trace_deadline = time.monotonic()+15
        while not ready_path.exists():
            if tracer.poll() is not None or time.monotonic()>trace_deadline:
                raise RuntimeError("private NFS attribution capture did not become ready; no writer started")
            time.sleep(0.1)
        record["traceReady"] = json.loads(ready_path.read_text())
        record["tracePID"] = tracer.pid
        self.checkpoint()
        log_path = self.root / f"trial-{index}.events.jsonl"
        sample_path = self.root / f"trial-{index}.samples.jsonl"
        with log_path.open("w") as log, (self.root / f"trial-{index}.stderr").open("w") as stderr:
            process = subprocess.Popen(["docker", "start", "--attach", cid], stdout=log, stderr=stderr)
            record["attachPID"] = process.pid
            record["phase"] = "running"
            self.checkpoint()
            pid, primary_phase, signal_at = 0, "starting", None
            deadline = time.monotonic() + 1800
            with sample_path.open("w") as samples:
                while process.poll() is None:
                    current_events = events(log_path)
                    for event in current_events:
                        if event["phase"] in ("copy", "copy-returned", "final-sync", "final-sync-returned", "close", "close-returned", "finished"):
                            primary_phase = event["phase"]
                    last_bytes = max((e.get("bytes", 0) for e in current_events), default=0)
                    if not pid:
                        pid = self.verify_owned(cid)["State"]["Pid"]
                    sample = {"at": time.time(), "primaryPhase": primary_phase,
                              "bytesObserved": last_bytes, "memory": memory(),
                              "cgroup": cgroup_snapshot(pid) if pid else None,
                              "mounts": mounts(), "productionImportOperations": production_imports(),
                              "configuredScratchWorkers": 1}
                    samples.write(json.dumps(sample) + "\n")
                    samples.flush()
                    should_stop = False
                    if stop and signal_at is None:
                        if stop["phase"] == "copy":
                            should_stop = primary_phase == "copy" and last_bytes >= size*stop["offset"]
                        else:
                            should_stop = primary_phase == stop["phase"]
                    if should_stop:
                        self.verify_owned(cid)
                        record["signalRequestStartWall"] = time.time()
                        record["signalAt"] = record["signalRequestStartWall"]
                        record["signalPhase"] = primary_phase
                        record["signalBytesObserved"] = last_bytes
                        self.checkpoint()  # persist identity/phase before signaling
                        signal_at = time.monotonic()
                        try:
                            command("docker", "kill", "--signal=" + stop["signal"], cid)
                            record["signalCommandSucceeded"] = True
                            record["signalRequestEndWall"] = time.time()
                        except (subprocess.CalledProcessError, subprocess.TimeoutExpired) as exc:
                            record["signalCommandSucceeded"] = False
                            record["signalRequestEndWall"] = time.time()
                            record["signalCommandError"] = str(exc)
                            self.checkpoint()  # keep waiting; an error is never exit proof
                    if time.monotonic() > deadline:
                        record["phase"] = "pending_actual_exit"
                        self.manifest["phase"] = "pending_actual_exit"
                        self.checkpoint()
                        raise TimeoutError("scratch writer has not exited; retained container/files; no next trial")
                    time.sleep(0.2 if stop else 1)
        # Client EOF/error alone is not exit proof.
        record["waitCode"] = int(command("docker", "wait", cid))
        old = self.verify_owned(cid)
        if old["State"]["Running"] or old["State"]["Pid"] != 0 or old["State"]["Status"] != "exited":
            raise RuntimeError("actual scratch writer exit was not proved")
        record["state"] = old["State"]
        if signal_at is not None:
            record["signalToWaitSeconds"] = time.monotonic() - signal_at
        record["exitVerifiedAt"] = time.time()
        record["signalSent"] = signal_at is not None and record.get("signalCommandSucceeded", False)
        Path(str(trace_path)+".stop").write_text("stop after verified actual exit\n")
        tracer.wait(timeout=15)
        done_path = Path(str(trace_path)+".done.json")
        record["traceDone"] = json.loads(done_path.read_text()) if done_path.exists() else None
        record["phase"] = "exit_verified"
        self.checkpoint()
        record["events"] = events(log_path)
        (self.root / f"trial-{index}.mountstats.after.txt").write_text(Path("/proc/self/mountstats").read_text())
        record["mountsBefore"], record["mountsAfter"] = before, mounts()
        record["phaseCoverageValid"] = confirmed_phase_coverage(record)
        record["actualBytes"] = target.stat().st_size if target.exists() else 0
        final = next((e for e in reversed(record["events"]) if e["phase"] == "finished"), None)
        copied = next((e for e in reversed(record["events"]) if e["phase"] == "copy-returned"), None)
        record["completedPolicyWork"] = bool(not stop and not close_stress and record["waitCode"] == 0
            and final and final["result"] == "completed" and copied and copied["bytes"] == size
            and copied.get("sha256") == self.manifest["sourceSHA256"][str(size)]
            and any(e["phase"] == "final-sync-returned" for e in record["events"]))
        def owner(snapshot, path):
            keys = [key for key in snapshot if path.is_relative_to(Path(key))]
            return snapshot[max(keys,key=len)] if keys else {}
        target_before = owner(before,self.target.parent)
        target_after = owner(record["mountsAfter"],self.target.parent)
        if "WRITE" in target_before and "WRITE" in target_after:
            write_bytes = target_after["WRITE"][3] - target_before["WRITE"][3]
            write_ops = target_after["WRITE"][0] - target_before["WRITE"][0]
            record["writeBytesSent"] = write_bytes
            record["writeRPCs"] = write_ops
            record["foreignEstimatedFraction"] = max(0, write_bytes - record["actualBytes"] - write_ops*4096) / max(1, record["actualBytes"])
            record["commitRPCs"] = target_after.get("COMMIT", [0])[0] - target_before.get("COMMIT", [0])[0]
        source_before = owner(before,self.source.parent)
        source_after = owner(record["mountsAfter"],self.source.parent)
        if "READ" in source_before and "READ" in source_after:
            record["sourceReadBytesReceived"] = source_after["READ"][4] - source_before["READ"][4]
        kernel = subprocess.run(["journalctl", "-k", "--since", "@"+str(record["startedAt"]), "--no-pager", "-o", "cat"], text=True, capture_output=True, timeout=15)
        record["kernelLogAccessible"] = kernel.returncode == 0 and not kernel.stderr.strip()
        record["nfsKernelMessages"] = [line for line in kernel.stdout.splitlines() if "nfs" in line.lower() or "rpc" in line.lower()]
        record["loggedNFSOutage"] = any("not responding" in line.lower() or "timed out" in line.lower() for line in record["nfsKernelMessages"])
        reasons = []
        if not record["completedPolicyWork"]:
            reasons.append("workload did not durably complete")
        for label, initial, final_mount in (("target",target_before,target_after),("source",source_before,source_after)):
            if not initial or initial.get("header") != final_mount.get("header") or initial.get("options") != final_mount.get("options") or initial.get("age",0) > final_mount.get("age",-1):
                reasons.append(label+" mount changed or is missing")
            for operation in ("WRITE","READ","COMMIT"):
                if operation not in initial or operation not in final_mount or any(b<a for a,b in zip(initial.get(operation,[]),final_mount.get(operation,[]))):
                    reasons.append(label+" RPC counters missing/reset")
        attribution = attribute_trace(trace_path,record)
        record["attribution"] = attribution
        if not attribution["valid"]:
            reasons.extend(attribution["reasons"])
        elif attribution["foreignWriteBytes"] / max(1,size) > 0.05:
            reasons.append("attributed foreign writes exceed 5% of own payload")
        if attribution.get("ownSourceReadBytes",0) < size*0.90 or attribution.get("ownSourceReadBytes",0) > size*1.10:
            reasons.append("realized source-read traffic is not in the declared cold class")
        if not record["kernelLogAccessible"]:
            reasons.append("kernel outage-log visibility unknown")
        if record["loggedNFSOutage"]:
            reasons.append("logged NFS/RPC outage")
        record["comparisonEligible"] = not reasons
        record["comparisonIneligibilityReasons"] = reasons
        record["phase"] = "exit_verified"
        self.checkpoint()
        # Require the exclusive-create descriptor receipt and current identity,
        # as well as actual process exit. A name alone is not cleanup authority.
        created = next((e for e in record["events"] if e["phase"] == "target-created"), None)
        if target.exists():
            current = target.lstat()
            if not created or not stat.S_ISREG(current.st_mode) or current.st_dev != created["device"] or current.st_ino != created["inode"]:
                raise RuntimeError("uncertain scratch object retained; cleanup identity was not proved")
            target.unlink()
        command("docker", "rm", cid)
        record["phase"] = "cleaned"
        self.checkpoint()
        print(json.dumps({"trial": index, "shape": shape, "policy": policy, "window": window,
                          "successful": record["completedPolicyWork"], "waitCode": record["waitCode"]}), flush=True)
        time.sleep(5)

    def run(self):
        self.prepare()
        for shape in ("unlimited", "512MiB"):
            for round_id in range(5):
                for index in range(len(POLICIES)):
                    policy, window = POLICIES[(index+round_id) % len(POLICIES)]
                    self.trial(INCIDENT, policy, window, shape, round_id)
            for policy, window in POLICIES:
                self.trial(LARGE, policy, window, shape, "large")
        # Reproducible offsets are declared before measurement, not selected
        # after seeing a favorable tail. Close stress is deliberately separate.
        rng = random.Random(20261101)
        for index in range(60):
            if index<8:
                shape=("unlimited","512MiB")[index%2]
                size=(INCIDENT,LARGE)[(index//2)%2]
                phase="copy" if index<4 else "final-sync"
                signal="KILL"
            else:
                attempt=index-8
                shape=("unlimited","512MiB")[attempt%2]
                size=(INCIDENT,LARGE)[(attempt//2)%2]
                phase=("copy","final-sync","close")[(attempt//4)%3]
                signal="TERM"
            self.trial(size,"whole",1024**3,shape,"stop",
                       {"phase":phase,"offset":rng.uniform(0.2,0.8),"signal":signal},close_stress=phase=="close")
            confirmed=[r for r in self.manifest["trials"] if r.get("phaseCoverageValid")]
            cells={(r["shape"],r["size"],r["stop"]["phase"]) for r in confirmed}
            if len(confirmed)>=20 and len(cells)==12:
                break
        self.manifest["phase"] = "measurements_complete_not_policy_qualification"
        self.checkpoint()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--results", required=True)
    parser.add_argument("--image", required=True)
    parser.add_argument("--source-parent", default="/mnt/qnap/working")
    parser.add_argument("--target-parent", default="/mnt/qnap/20t")
    args = parser.parse_args()
    q = Qualification(args.results, args.source_parent, args.target_parent, args.image)
    try:
        q.run()
    except Exception as exc:
        q.manifest["error"] = str(exc)
        q.manifest["phase"] = "failed_or_pending_custody_retained"
        q.checkpoint()
        raise


if __name__ == "__main__":
    main()
