#!/usr/bin/env python3
"""Boot the POC VMM with a fresh, dedicated RAW disk; never uses deployed services."""
import argparse
import hashlib
import http.client
import json
import os
import pathlib
import pty
import tty
import select
import re
import socket
import subprocess
import time


class UnixHTTP(http.client.HTTPConnection):
    def __init__(self, path):
        super().__init__("localhost", timeout=30)
        self.path = str(path)

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(30)
        self.sock.connect(self.path)


def api(path, action, body=None):
    connection = UnixHTTP(path)
    try:
        data = json.dumps(body).encode() if body is not None else b""
        connection.request("PUT", "/api/v1/" + action, data, {"Content-Type": "application/json"})
        response = connection.getresponse()
        content = response.read()
        if response.status not in (200, 204):
            raise RuntimeError((action, response.status, content.decode(errors="replace")))
        return content
    finally:
        connection.close()


def allocation(disk):
    st = disk.stat()
    fs = os.statvfs(disk.parent)
    return {"logical_bytes": st.st_size, "allocated_bytes": st.st_blocks * 512,
            "host_free_bytes": fs.f_bavail * fs.f_frsize}


def process_usage(pid):
    fields = pathlib.Path("/proc/%s/stat" % pid).read_text().rsplit(")", 1)[1].split()
    status = pathlib.Path("/proc/%s/status" % pid).read_text()
    rss = re.search(r"^VmRSS:\s+(\d+)", status, re.M)
    return {"cpu_ms": (int(fields[11]) + int(fields[12])) * 1000 / os.sysconf("SC_CLK_TCK"),
            "rss_kib": int(rss.group(1)) if rss else None}


def sample(disk, minimum=0.3, maximum=1.0):
    start = time.monotonic()
    rows = []
    while True:
        row = allocation(disk)
        row["elapsed_ms"] = (time.monotonic() - start) * 1000
        rows.append(row)
        if time.monotonic() - start >= maximum:
            return rows
        if time.monotonic() - start >= minimum and len(rows) >= 4:
            if len(set(x["allocated_bytes"] for x in rows[-4:])) == 1:
                return rows
        time.sleep(0.1)


def one_run(base, vmm, kernel, initramfs, mode, backend, index, snapshot):
    run = base / ("%02d-%s-%s" % (index, backend, mode))
    run.mkdir()
    disk = run / "disk.raw"
    with disk.open("xb") as f:
        f.truncate(256 * 1024 * 1024)
    subprocess.run(["mkfs.ext4", "-q", "-F", "-E",
                    "nodiscard,lazy_itable_init=0,lazy_journal_init=0", str(disk)], check=True)
    sock = run / "api.sock"
    cmd = [str(vmm), "-v", "--log-stderr", "--kernel", str(kernel), "--initramfs", str(initramfs),
           "--cpus", "boot=1", "--memory", "size=256M",
           "--cmdline", "console=ttyS0 panic=-1 quiet poc.mode=" + mode,
           "--serial", "tty", "--console", "off",
           "--disk", "path=%s,discard=%s,_disable_io_uring=%s" %
           (disk, "off" if mode == "off" else "on", "on" if backend == "sync" else "off"),
           "--api-socket", str(sock)]
    result = {"mode": mode, "backend": backend, "index": index, "command": cmd,
              "events": [], "stages": {}, "lifecycle": {}}
    logfile = (run / "vmm.log").open("wb")
    serial = (run / "serial.log").open("wb")
    master, slave = pty.openpty()
    tty.setraw(slave)
    process = subprocess.Popen(cmd, stdin=slave, stdout=slave, stderr=logfile)
    os.close(slave)
    pending = b""
    deadline = time.monotonic() + 120
    checkpoints = {"baseline", "large_written", "large_deleted", "large_trimmed",
                   "small_written", "small_deleted", "small_trimmed", "before_pause", "after_pause"}
    try:
        while time.monotonic() < deadline:
            ready, _, _ = select.select([master], [], [], 1)
            if not ready:
                if process.poll() is not None:
                    raise RuntimeError("VMM exited: %s; see %s" % (process.returncode, run))
                continue
            chunk = os.read(master, 65536)
            if not chunk:
                raise RuntimeError("VMM output closed; see %s" % run)
            serial.write(chunk)
            serial.flush()
            pending += chunk
            while b"\n" in pending:
                line, pending = pending.split(b"\n", 1)
                marker = line.find(b"POC_JSON ")
                if marker < 0:
                    continue
                event = json.loads(line[marker + 9:].decode())
                result["events"].append(event)
                stage = event["stage"]
                if stage == "fatal":
                    raise RuntimeError(event)
                if stage == "device":
                    expected = 0 if mode == "off" else 134217728
                    if event["discard_max_bytes"] != expected:
                        raise AssertionError(("discard capability", expected, event))
                if stage in checkpoints:
                    maximum = 10 if stage in ("large_deleted", "large_trimmed") else 1
                    rows = sample(disk, minimum=maximum, maximum=maximum)
                    result["stages"][stage] = rows
                    result.setdefault("process_usage", {})[stage] = process_usage(process.pid)
                    if stage == "before_pause":
                        start = time.monotonic()
                        api(sock, "vm.pause")
                        result["lifecycle"]["pause_ms"] = (time.monotonic() - start) * 1000
                        clone = run / "parent.raw"
                        subprocess.run(["cp", "--reflink=always", str(disk), str(clone)], check=True)
                        result["lifecycle"]["parent_sha256_before"] = file_hash(clone)
                        if snapshot:
                            snap = run / "snapshot"
                            snap.mkdir()
                            start = time.monotonic()
                            api(sock, "vm.snapshot", {"destination_url": "file://" + str(snap)})
                            result["lifecycle"]["snapshot_ms"] = (time.monotonic() - start) * 1000
                        start = time.monotonic()
                        api(sock, "vm.resume")
                        result["lifecycle"]["resume_ms"] = (time.monotonic() - start) * 1000
                    if stage == "after_pause":
                        clone = run / "parent.raw"
                        assert file_hash(clone) == result["lifecycle"]["parent_sha256_before"]
                        result["lifecycle"]["parent_unchanged"] = True
                    print(json.dumps({"run": run.name, "stage": stage,
                                      "allocated": rows[-1]["allocated_bytes"]}), flush=True)
                    os.write(master, b"\n")
                if stage == "done":
                    assert all(row["logical_bytes"] == 268435456 for rows in result["stages"].values() for row in rows)
                    start = result["stages"]["large_written"][-1]["allocated_bytes"]
                    baseline = result["stages"]["baseline"][-1]["allocated_bytes"]
                    end_stage = "large_trimmed" if mode == "batch" else "large_deleted"
                    end = result["stages"][end_stage][-1]["allocated_bytes"]
                    result["large_reclaimed_bytes"] = start - end
                    result["large_reclaim_fraction"] = (start - end) / (start - baseline)
                    if mode != "off":
                        assert result["large_reclaim_fraction"] >= 0.8
                    logs = (run / "vmm.log").read_text(errors="replace")
                    expected_backend = "asynchronous RAW" if backend == "async" else "synchronous RAW"
                    assert expected_backend in logs, ("backend not proven", expected_backend)
                    previous = "large_trimmed" if mode == "batch" else "large_deleted"
                    previous_bytes = result["stages"][previous][-1]["allocated_bytes"]
                    small_start = result["stages"]["small_written"][-1]["allocated_bytes"]
                    small_end_stage = "small_trimmed" if mode == "batch" else "small_deleted"
                    small_end = result["stages"][small_end_stage][-1]["allocated_bytes"]
                    result["small_reclaimed_bytes"] = small_start - small_end
                    result["small_reclaim_fraction"] = (small_start - small_end) / (small_start - previous_bytes)
                    if mode != "off":
                        assert result["small_reclaim_fraction"] >= 0.8
                    result["backend_confirmed"] = True
                    result["success"] = True
                    (run / "result.json").write_text(json.dumps(result, indent=2) + "\n")
                    return result
        raise TimeoutError("guest run timeout: " + str(run))
    except Exception as error:
        result["error"] = repr(error)
        (run / "result.json").write_text(json.dumps(result, indent=2) + "\n")
        raise
    finally:
        if process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)
        os.close(master)
        logfile.close()
        serial.close()


def file_hash(path):
    h = hashlib.sha256()
    with path.open("rb") as f:
        for block in iter(lambda: f.read(1024 * 1024), b""):
            h.update(block)
    return h.hexdigest()


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--base", required=True, type=pathlib.Path)
    p.add_argument("--kernel", required=True, type=pathlib.Path)
    p.add_argument("--rounds", type=int, default=1)
    p.add_argument("--modes", nargs="+", choices=["off", "online", "batch"], default=["off", "online", "batch"])
    p.add_argument("--backends", nargs="+", choices=["async", "sync"], default=["async"])
    p.add_argument("--snapshot", action="store_true")
    args = p.parse_args()
    base = args.base.resolve()
    if not base.is_dir() or not base.name.startswith("cube-reclaim-poc-"):
        p.error("--base must be the dedicated cube-reclaim-poc-* directory")
    if args.rounds not in range(1, 6):
        p.error("--rounds must be between 1 and 5")
    results = []
    for index in range(args.rounds):
        modes = args.modes if index % 2 == 0 else list(reversed(args.modes))
        for backend in args.backends:
            for mode in modes:
                results.append(one_run(base, base / "poc-vmm", args.kernel, base / "poc-initramfs.cpio",
                                       mode, backend, index, args.snapshot))
    (base / "results.json").write_text(json.dumps(results, indent=2) + "\n")
    print(json.dumps({"success": True, "runs": len(results), "base": str(base)}), flush=True)


if __name__ == "__main__":
    main()
