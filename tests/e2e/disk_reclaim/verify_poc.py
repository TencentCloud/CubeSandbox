#!/usr/bin/env python3
"""Offline known-content checks and restart-from-snapshot check for POC artifacts."""
import argparse
import hashlib
import json
import os
import pathlib
import pty
import select
import subprocess
import time
import tty
from run_poc import api, UnixHTTP, file_hash, allocation


def offline(run):
    checks = []
    for source_name, expect_reuse in (("disk.raw", False), ("parent.raw", True)):
        source = run / source_name
        inspect = run / ("inspect-" + source_name)
        subprocess.run(["cp", "--reflink=always", str(source), str(inspect)], check=True)
        try:
            repair = subprocess.run(["e2fsck", "-p", "-E", "journal_only", str(inspect)],
                                    stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
            if repair.returncode not in (0, 1):
                raise RuntimeError(("journal replay", source_name, repair.stdout))
            for name, length, byte in ([("keep", 1048576, 0x5a)] +
                                      ([("reuse", 33554432, 0x3c)] if expect_reuse else [])):
                dump = run / ("verify-" + source_name + "-" + name)
                assert not dump.exists()
                try:
                    command = "dump /upper/%s %s" % (name, dump)
                    result = subprocess.run(["debugfs", "-R", command, str(inspect)],
                                            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
                    assert result.returncode == 0 and dump.exists(), (command, result.stderr)
                    expected = hashlib.sha256(bytes([byte]) * length).hexdigest()
                    assert dump.stat().st_size == length and file_hash(dump) == expected
                    checks.append({"disk": source_name, "file": name, "sha256": expected, "bytes": length})
                finally:
                    if dump.exists():
                        dump.unlink()
            if not expect_reuse:
                stat = subprocess.run(["debugfs", "-R", "stat /upper/reuse", str(inspect)],
                                      stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
                assert "File not found" in stat.stderr, stat.stdout + stat.stderr
        finally:
            inspect.unlink()
    result = {"success": True, "checks": checks}
    (run / "offline-verification.json").write_text(json.dumps(result, indent=2) + "\n")
    return result


def restore(source_run, vmm):
    run = source_run / "restart-restore"
    run.mkdir()
    clone = run / "disk.raw"
    parent = source_run / "parent.raw"
    parent_hash = file_hash(parent)
    subprocess.run(["cp", "--reflink=always", str(parent), str(clone)], check=True)
    config = json.loads((source_run / "snapshot/config.json").read_text())
    disks = config["disks"]
    disks[0]["path"] = str(clone)
    sock = run / "api.sock"
    master, slave = pty.openpty()
    tty.setraw(slave)
    stderr = (run / "vmm.log").open("wb")
    serial = (run / "serial.log").open("wb")
    process = subprocess.Popen([str(vmm), "-v", "--log-stderr", "--api-socket", str(sock)],
                               stdin=slave, stdout=slave, stderr=stderr)
    os.close(slave)
    result = {"events": [], "source_run": str(source_run)}
    try:
        deadline = time.monotonic() + 20
        while not sock.exists():
            assert process.poll() is None
            if time.monotonic() > deadline:
                raise TimeoutError("restore API socket")
            time.sleep(0.05)
        start = time.monotonic()
        api(sock, "vm.restore", {"source_url": "file://" + str(source_run / "snapshot"), "disks": disks})
        result["restore_ms"] = (time.monotonic() - start) * 1000
        connection = UnixHTTP(sock)
        connection.request("GET", "/api/v1/vm.info")
        response = connection.getresponse()
        info = json.loads(response.read())
        connection.close()
        result["state_after_restore"] = info["state"]
        if info["state"] == "Paused":
            api(sock, "vm.resume")
        os.write(master, b"\n")
        pending = b""
        deadline = time.monotonic() + 45
        while time.monotonic() < deadline:
            ready, _, _ = select.select([master], [], [], 1)
            if not ready:
                assert process.poll() is None
                continue
            chunk = os.read(master, 65536)
            serial.write(chunk)
            serial.flush()
            pending += chunk
            while b"\n" in pending:
                line, pending = pending.split(b"\n", 1)
                marker = line.find(b"POC_JSON ")
                if marker < 0:
                    continue
                event = json.loads(line[marker + 9:])
                result["events"].append(event)
                assert event["stage"] != "fatal", event
                if event["stage"] == "after_pause":
                    result["allocation_after_delete"] = allocation(clone)
                    os.write(master, b"\n")
                if event["stage"] == "done":
                    result["success"] = True
                    assert file_hash(parent) == parent_hash
                    (run / "result.json").write_text(json.dumps(result, indent=2) + "\n")
                    return result
        raise TimeoutError("restored guest")
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
        serial.close()
        stderr.close()


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--base", type=pathlib.Path, required=True)
    p.add_argument("--restore-run", type=pathlib.Path)
    args = p.parse_args()
    base = args.base.resolve()
    if not base.name.startswith("cube-reclaim-poc-"):
        p.error("requires dedicated POC directory")
    runs = sorted(base.glob("*/result.json"))
    assert runs
    for file in runs:
        run = file.parent
        assert json.loads(file.read_text()).get("success")
        print(json.dumps({"run": run.name, "offline": offline(run)["success"]}), flush=True)
    if args.restore_run:
        source = args.restore_run.resolve()
        assert source.parent == base
        result = restore(source, base / "poc-vmm")
        print(json.dumps({"restart_restore": result}), flush=True)


if __name__ == "__main__":
    main()
