#!/usr/bin/env python3
"""Regression snapshot for cube-envd (not an upstream conformance test).

Records a *normalized* snapshot of the deterministic data-plane behavior and
compares it against a checked-in baseline, so a change in wire shape or
semantics is caught without needing the upstream Go envd binary. Passing this
proves "no regression against the recorded baseline", not "byte-identical to
upstream envd".

    python3 scripts/conformance.py            # compare against the baseline
    python3 scripts/conformance.py --update   # rewrite the baseline

Environment overrides:
    CUBE_ENVD_BIN                path to the cube-envd binary
    CUBE_ENVD_CONFORMANCE_PORT   listen port (default 46000)
"""
import base64
import difflib
import http.client
import json
import os
import struct
import subprocess
import sys
import tempfile
import time

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(HERE)
BASELINE = os.path.join(HERE, "conformance.baseline.json")
HOST = "127.0.0.1"
PORT = int(os.environ.get("CUBE_ENVD_CONFORMANCE_PORT", "46000"))
BIN = os.environ.get("CUBE_ENVD_BIN", os.path.join(REPO, "target", "release", "cube-envd"))
LOG = os.path.join(tempfile.gettempdir(), "cube-envd-conformance.log")


def envelope(payload: bytes, flags: int = 0) -> bytes:
    return bytes([flags]) + struct.pack(">I", len(payload)) + payload


def request(method, path, body=b"", headers=None):
    conn = http.client.HTTPConnection(HOST, PORT, timeout=15)
    conn.request(method, path, body=body, headers=headers or {})
    resp = conn.getresponse()
    data = resp.read()
    result = (resp.status, {k.lower(): v for k, v in resp.getheaders()}, data)
    conn.close()
    return result


def stream_request(method, path, body=b"", headers=None):
    conn = http.client.HTTPConnection(HOST, PORT, timeout=15)
    conn.request(method, path, body=body, headers=headers or {})
    return conn, conn.getresponse()


def read_exact(resp, n):
    buf = b""
    while len(buf) < n:
        chunk = resp.read(n - len(buf))
        if not chunk:
            raise EOFError("stream ended early")
        buf += chunk
    return buf


def read_envelope(resp):
    header = read_exact(resp, 5)
    flags = header[0]
    length = struct.unpack(">I", header[1:5])[0]
    return flags, read_exact(resp, length)


def collect_process_stream(conn, resp):
    frames = []
    while True:
        flags, payload = read_envelope(resp)
        if flags & 0x02:
            break
        frames.append(json.loads(payload))
    conn.close()
    return frames


def run_command(args):
    payload = {"process": {"cmd": args[0], "args": args[1:]}}
    conn, resp = stream_request(
        "POST",
        "/process.Process/Start",
        envelope(json.dumps(payload).encode()),
        {"Content-Type": "application/connect+json", "Connect-Protocol-Version": "1"},
    )
    return collect_process_stream(conn, resp)


def process_output(frames):
    stdout = b""
    end = None
    for frame in frames:
        event = frame["event"]
        if "data" in event and "stdout" in event["data"]:
            stdout += base64.b64decode(event["data"]["stdout"])
        if "end" in event:
            end = event["end"]
    return stdout, end


def rpc(method, body):
    return request(
        "POST",
        f"/filesystem.Filesystem/{method}",
        json.dumps(body).encode(),
        {"Content-Type": "application/json"},
    )


def start_server():
    if not os.path.exists(BIN):
        raise RuntimeError(f"binary not found: {BIN} (run `cargo build --release`)")
    process = subprocess.Popen(
        [BIN, "--port", str(PORT)],
        stdout=open(LOG, "wb"),
        stderr=subprocess.STDOUT,
    )
    for _ in range(50):
        try:
            if request("GET", "/health")[0] == 204:
                return process
        except OSError:
            pass
        time.sleep(0.1)
    process.terminate()
    raise RuntimeError("cube-envd did not become ready")


def snapshot():
    cases = {}

    cases["health_status"] = request("GET", "/health")[0]
    status = json.loads(request("GET", "/status")[2])
    cases["status"] = {
        "service": status["service"],
        "ready": status["ready"],
        "keys": sorted(status.keys()),
    }
    cases["envs_keys"] = sorted(json.loads(request("GET", "/envs")[2]).keys())
    cases["metrics_keys"] = sorted(json.loads(request("GET", "/metrics")[2]).keys())

    stdout, end = process_output(run_command(["/bin/bash", "-c", "echo -n out; exit 4"]))
    cases["process_exit"] = {
        "stdout": stdout.decode(),
        "exitCode": end.get("exitCode"),
        "exited": end["exited"],
        "status": end["status"],
        "reason": end["termination"]["reason"],
    }
    _, end = process_output(run_command(["/bin/sh", "-c", "kill -9 $$"]))
    cases["process_signal"] = {
        "exitCode": end.get("exitCode"),
        "exited": end["exited"],
        "status": end["status"],
        "reason": end["termination"]["reason"],
        "signal": end["termination"]["signal"],
        "signalName": end["termination"]["signalName"],
    }

    tmp = tempfile.mkdtemp(prefix="cube-envd-conformance-")
    target = os.path.join(tmp, "f.txt")
    cases["files_write_status"] = request(
        "POST",
        f"/files?path={target}",
        b"0123456789",
        {"Content-Type": "application/octet-stream"},
    )[0]
    cases["files_read"] = request("GET", f"/files?path={target}")[2].decode()
    status, headers, body = request(
        "GET", f"/files?path={target}", headers={"Range": "bytes=2-5"}
    )
    cases["files_range"] = {
        "status": status,
        "contentRange": headers.get("content-range"),
        "body": body.decode(),
    }
    cases["files_range_invalid"] = request(
        "GET", f"/files?path={target}", headers={"Range": "bytes=99-"}
    )[0]
    last_modified = request("GET", f"/files?path={target}")[1]["last-modified"]
    cases["files_not_modified"] = request(
        "GET", f"/files?path={target}", headers={"If-Modified-Since": last_modified}
    )[0]

    directory = os.path.join(tmp, "d")
    cases["makedir_status"] = rpc("MakeDir", {"path": directory})[0]
    entry = json.loads(rpc("Stat", {"path": directory})[2])["entry"]
    cases["stat"] = {
        "keys": sorted(entry.keys()),
        "type": entry["type"],
        "permissions_prefix": entry["permissions"][0],
    }
    cases["listdir"] = sorted(
        item["name"] for item in json.loads(rpc("ListDir", {"path": tmp})[2])["entries"]
    )
    cases["stat_missing_status"] = rpc("Stat", {"path": os.path.join(tmp, "nope")})[0]

    watch_dir = os.path.join(tmp, "w")
    os.makedirs(watch_dir)
    conn, resp = stream_request(
        "POST",
        "/filesystem.Filesystem/WatchDir",
        envelope(json.dumps({"path": watch_dir}).encode()),
        {"Content-Type": "application/connect+json", "Connect-Protocol-Version": "1"},
    )
    read_envelope(resp)
    open(os.path.join(watch_dir, "created.txt"), "w").close()
    watch_type = None
    for _ in range(40):
        try:
            _, body = read_envelope(resp)
        except EOFError:
            break
        event = json.loads(body).get("filesystem")
        if event and event["name"] == "created.txt":
            watch_type = event["type"]
            break
    conn.close()
    cases["watch_create"] = watch_type

    status, _, body = request(
        "POST",
        "/filesystem.Filesystem/CreateWatcher",
        json.dumps({"path": watch_dir}).encode(),
        {"Content-Type": "application/json"},
    )
    watcher_id = json.loads(body)["watcherId"]
    open(os.path.join(watch_dir, "pull.txt"), "w").close()
    pull_type = None
    for _ in range(40):
        status, _, body = request(
            "POST",
            "/filesystem.Filesystem/GetWatcherEvents",
            json.dumps({"watcherId": watcher_id}).encode(),
            {"Content-Type": "application/json"},
        )
        events = json.loads(body).get("events", [])
        match = next((event for event in events if event["name"] == "pull.txt"), None)
        if match:
            pull_type = match["type"]
            break
        time.sleep(0.05)
    cases["pull_watch_create"] = pull_type
    cases["remove_watcher_status"] = request(
        "POST",
        "/filesystem.Filesystem/RemoveWatcher",
        json.dumps({"watcherId": watcher_id}).encode(),
        {"Content-Type": "application/json"},
    )[0]

    return cases


def main():
    update = "--update" in sys.argv
    server = start_server()
    try:
        current = snapshot()
    finally:
        server.terminate()
        try:
            server.wait(timeout=5)
        except subprocess.TimeoutExpired:
            server.kill()

    current_text = json.dumps(current, indent=2, sort_keys=True) + "\n"
    if update:
        with open(BASELINE, "w", encoding="utf-8") as handle:
            handle.write(current_text)
        print(f"conformance baseline updated: {BASELINE}")
        return 0

    if not os.path.exists(BASELINE):
        print("no baseline; run `python3 scripts/conformance.py --update` first")
        return 1
    with open(BASELINE, "r", encoding="utf-8") as handle:
        baseline_text = handle.read()

    if current_text == baseline_text:
        print(f"CONFORMANCE OK: {len(current)} cases match the baseline")
        return 0

    print("CONFORMANCE MISMATCH:")
    for line in difflib.unified_diff(
        baseline_text.splitlines(),
        current_text.splitlines(),
        fromfile="baseline",
        tofile="current",
        lineterm="",
    ):
        print(line)
    return 1


if __name__ == "__main__":
    sys.exit(main())
