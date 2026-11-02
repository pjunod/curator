#!/usr/bin/env python3
"""Build a task-only image and verify its executable without starting it."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import tempfile
import uuid


def command(*args):
    return subprocess.run(args, text=True, capture_output=True, check=True).stdout.strip()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--tag", required=True)
    args = parser.parse_args()
    if not re.fullmatch(r"codex-nfs-qual:[a-f0-9-]+", args.tag):
        raise ValueError("task-owned image tag required")
    bundle = Path(__file__).resolve().parent
    receipt = bundle / "build_receipt.json"
    if receipt.exists():
        raise RuntimeError("build receipt exists; use a fresh immutable bundle")
    files = ("main_linux.go", "qualify.py", "trace_capture.py", "bench", "Dockerfile")
    hashes = {name: hashlib.sha256((bundle/name).read_bytes()).hexdigest() for name in files}
    command("docker", "build", "-t", args.tag, str(bundle))
    image = command("docker", "image", "inspect", args.tag, "--format", "{{.Id}}")
    owner = uuid.uuid4().hex
    cid = command("docker", "create", "--name", "codex-nfs-verify-"+owner,
                  "--label", "com.pjunod.codex.nfs-verification="+owner, image)

    def verify():
        item = json.loads(command("docker", "inspect", cid))[0]
        if (item["Id"]!=cid or item["Image"]!=image
                or item["Config"]["Labels"].get("com.pjunod.codex.nfs-verification")!=owner
                or item["State"]["Running"] or item["State"]["Pid"]!=0
                or item["State"]["Status"]!="created"):
            raise RuntimeError("never-started verification container identity changed; retained")
    verify()
    with tempfile.TemporaryDirectory(prefix="nfs-image-verify-", dir=bundle) as directory:
        path = Path(directory)/"bench"
        command("docker", "cp", cid+":/bench", str(path))
        if not stat.S_ISREG(path.lstat().st_mode):
            raise RuntimeError("image executable is not a regular file")
        actual = hashlib.sha256(path.read_bytes()).hexdigest()
        if actual != hashes["bench"]:
            raise RuntimeError("image executable differs from the reviewed worker artifact")
    verify()
    command("docker", "rm", cid)
    for name, expected in hashes.items():
        if hashlib.sha256((bundle/name).read_bytes()).hexdigest()!=expected:
            raise RuntimeError("bundle changed during image verification")
    with receipt.open("x") as out:
        json.dump({"imageId": image, "tag": args.tag, "benchSHA256": actual,
                   "sourceHashes": hashes, "verification": "never-started container executable SHA256 matches reviewed artifact"}, out, indent=2)
        out.flush()
        os.fsync(out.fileno())
    print(image)


if __name__ == "__main__":
    main()
