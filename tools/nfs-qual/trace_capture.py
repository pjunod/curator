#!/usr/bin/env python3
"""Capture NFS write metadata in one private tracing instance; never payloads.

Invoked through sudo by the qualification controller. Only this process's
randomly named tracefs instance is enabled/disabled/removed. Existing tracing
instances, global tracing, exports and mount options are untouched.
"""

import argparse
import json
import os
from pathlib import Path
import re
import selectors
import time


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--id", required=True)
    parser.add_argument("--output", required=True)
    parser.add_argument("--mount", required=True)
    parser.add_argument("--source-mount", required=True)
    args = parser.parse_args()
    if not re.fullmatch(r"[a-f0-9]{32}", args.id):
        raise ValueError("invalid private instance ID")
    if os.geteuid() != 0 or "SUDO_UID" not in os.environ:
        raise RuntimeError("invoke through sudo from the task owner")
    uid, gid = int(os.environ["SUDO_UID"]), int(os.environ["SUDO_GID"])
    output = Path(args.output).resolve()
    parent = output.parent.stat()
    if parent.st_uid != uid or parent.st_mode & 0o077:
        raise RuntimeError("results directory must be private and owned by the caller")
    if output.exists():
        raise RuntimeError("refusing to overwrite tracing output")
    device = os.stat(args.mount).st_dev
    major, minor = os.major(device), os.minor(device)
    kernel_dev = (major << 20) | minor
    source_device = os.stat(args.source_mount).st_dev
    source_kernel_dev = (os.major(source_device) << 20) | os.minor(source_device)
    instance = Path("/sys/kernel/tracing/instances") / ("codex_nfs_qual_" + args.id)
    metadata = {"id": args.id, "device": device, "major": major, "minor": minor,
                "sourceDevice": source_device, "startWall": time.time(), "startMonotonic": time.monotonic(),
                "ino64": Path("/sys/module/nfs/parameters/enable_ino64").read_text().strip()}
    instance.mkdir()  # exclusive; never attach to someone else's instance
    fd, stream, selector = None, None, None
    stop = Path(str(output) + ".stop")

    def write_metadata(suffix):
        path = Path(str(output) + suffix)
        flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW
        record_fd = os.open(path, flags, 0o600)
        os.fchown(record_fd, uid, gid)
        with os.fdopen(record_fd, "w") as out:
            json.dump(metadata, out, indent=2)
            out.flush()
            os.fsync(out.fileno())

    try:
        (instance / "tracing_on").write_text("0")
        (instance / "buffer_size_kb").write_text("256")
        (instance / "trace_clock").write_text("mono")
        metadata["formats"] = {}
        for event in ("nfs_initiate_write", "nfs_writeback_done", "nfs_initiate_read"):
            base = instance / "events/nfs" / event
            metadata["formats"][event] = (base / "format").read_text()
            (base / "filter").write_text(f"dev == {kernel_dev} || dev == {source_kernel_dev}")
            (base / "enable").write_text("1")
        fd = os.open(instance / "trace_pipe", os.O_RDONLY | os.O_NONBLOCK)
        stream_fd = os.open(output, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
        os.fchown(stream_fd, uid, gid)
        stream = os.fdopen(stream_fd, "wb", buffering=0)
        selector = selectors.DefaultSelector()
        selector.register(fd, selectors.EVENT_READ)
        (instance / "tracing_on").write_text("1")
        write_metadata(".ready.json")
        deadline = time.monotonic() + 45*60
        stopped = False
        while True:
            if not stopped and (stop.exists() or time.monotonic() >= deadline):
                (instance / "tracing_on").write_text("0")
                metadata["stopReason"] = "controller" if stop.exists() else "capture_deadline"
                stopped = True
            ready = selector.select(timeout=0.2)
            if not ready and stopped:
                break
            empty_after_stop = False
            for _key, _mask in ready:
                try:
                    data = os.read(fd, 65536)
                except BlockingIOError:
                    data = b""
                if data:
                    stream.write(data)
                elif stopped:
                    empty_after_stop = True
            if empty_after_stop:
                break
        metadata["perCPU"] = {p.parent.name: p.read_text() for p in (instance / "per_cpu").glob("cpu*/stats")}
        metadata["endWall"], metadata["endMonotonic"] = time.time(), time.monotonic()
        stream.flush()
        os.fsync(stream.fileno())
        write_metadata(".done.json")
    finally:
        (instance / "tracing_on").write_text("0")
        for event in ("nfs_initiate_write", "nfs_writeback_done", "nfs_initiate_read"):
            (instance / "events/nfs" / event / "enable").write_text("0")
        if selector is not None:
            selector.close()
        if fd is not None:
            os.close(fd)
        if stream is not None:
            stream.close()
        instance.rmdir()


if __name__ == "__main__":
    main()
