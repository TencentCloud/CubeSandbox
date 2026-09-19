#!/usr/bin/env python3
"""Acceptance scenarios for cube-envd (实战任务二).

Runs the three scenarios the task requires and prints a report with the raw
results, so the evidence can be pasted into the PR:

  1. 健康检查 / health check
  2. 命令执行 / command execution
  3. 文件读写 / file read + write

    python3 scripts/scenarios.py                 # run and print the report
    python3 scripts/scenarios.py --report FILE   # also write the report to FILE
"""
import base64
import datetime
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
HOST = "127.0.0.1"
PORT = int(os.environ.get("CUBE_ENVD_SCENARIOS_PORT", "46010"))
BIN = os.environ.get("CUBE_ENVD_BIN", os.path.join(REPO, "target", "release", "cube-envd"))
LOG = os.path.join(tempfile.gettempdir(), "cube-envd-scenarios.log")

RESULTS = []


def record(scenario, detail, ok):
    RESULTS.append((scenario, detail, ok))
    print(f"  [{'PASS' if ok else 'FAIL'}] {scenario}: {detail}")


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


def run_command(args):
    payload = {"process": {"cmd": args[0], "args": args[1:]}}
    conn, resp = stream_request(
        "POST",
        "/process.Process/Start",
        envelope(json.dumps(payload).encode()),
        {"Content-Type": "application/connect+json", "Connect-Protocol-Version": "1"},
    )
    stdout, stderr, end = b"", b"", None
    while True:
        flags, body = read_envelope(resp)
        if flags & 0x02:
            break
        event = json.loads(body)["event"]
        if "data" in event:
            if "stdout" in event["data"]:
                stdout += base64.b64decode(event["data"]["stdout"])
            if "stderr" in event["data"]:
                stderr += base64.b64decode(event["data"]["stderr"])
        if "end" in event:
            end = event["end"]
    conn.close()
    return stdout.decode(), stderr.decode(), end


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


def scenario_health():
    print("Scenario 1: 健康检查 / health check")
    status, _, _ = request("GET", "/health")
    record("health", f"GET /health -> {status}", status == 204)
    status, _, body = request("GET", "/status")
    payload = json.loads(body)
    record(
        "identity",
        f"GET /status -> service={payload['service']} ready={payload['ready']}",
        status == 200 and payload["service"] == "cube-envd",
    )


def scenario_command():
    print("Scenario 2: 命令执行 / command execution")
    stdout, stderr, end = run_command(
        ["/bin/bash", "-c", 'echo -n "hello from cube-envd"; echo -n "warn" >&2']
    )
    record(
        "command.output",
        f"stdout={stdout!r} stderr={stderr!r} exitCode={end.get('exitCode')} status={end['status']!r}",
        stdout == "hello from cube-envd" and stderr == "warn" and end["exited"],
    )
    _, _, end = run_command(["/bin/sh", "-c", "exit 7"])
    record(
        "command.exit_code",
        f"exit 7 -> exitCode={end.get('exitCode')} error={end.get('error')!r}",
        end.get("exitCode") == 7 and end.get("error") == "exit status 7",
    )


def scenario_files():
    print("Scenario 3: 文件读写 / file read + write")
    directory = tempfile.mkdtemp(prefix="cube-envd-scenario-")
    path = os.path.join(directory, "note.txt")
    content = "cube-envd file scenario\n"
    status, _, _ = request(
        "POST",
        f"/files?path={path}",
        content.encode(),
        {"Content-Type": "application/octet-stream"},
    )
    record("files.write", f"POST /files -> {status}", status == 200 and os.path.exists(path))
    status, _, body = request("GET", f"/files?path={path}")
    record(
        "files.read",
        f"GET /files -> {status} body={body.decode()!r}",
        status == 200 and body.decode() == content,
    )
    status, _, body = request("GET", f"/files?path={path}", headers={"Range": "bytes=0-3"})
    record(
        "files.range",
        f"GET /files Range 0-3 -> {status} body={body.decode()!r}",
        status == 206 and body == b"cube",
    )


def main():
    report = None
    if "--report" in sys.argv:
        report = sys.argv[sys.argv.index("--report") + 1]

    server = start_server()
    try:
        scenario_health()
        scenario_command()
        scenario_files()
    finally:
        server.terminate()
        try:
            server.wait(timeout=5)
        except subprocess.TimeoutExpired:
            server.kill()

    ok = all(result for _, _, result in RESULTS)
    print()
    print(f"SCENARIOS {'PASSED' if ok else 'FAILED'}: {len(RESULTS)} checks")

    if report:
        with open(report, "w", encoding="utf-8") as handle:
            handle.write("# cube-envd acceptance scenario results\n\n")
            handle.write(f"Generated: {datetime.datetime.utcnow():%Y-%m-%dT%H:%M:%SZ}\n\n")
            handle.write("| scenario | result | detail |\n|---|---|---|\n")
            for scenario, detail, passed in RESULTS:
                handle.write(f"| {scenario} | {'PASS' if passed else 'FAIL'} | {detail} |\n")
            handle.write("\n")
            handle.write(f"cube-envd log: `{LOG}`\n")
        print(f"report written: {report}")

    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
