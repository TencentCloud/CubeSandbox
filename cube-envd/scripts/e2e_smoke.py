#!/usr/bin/env python3
"""Router-level end-to-end smoke test for cube-envd.

Boots the release binary and drives the real HTTP + Connect surface, covering
the six surfaces the SDKs depend on plus concurrency:

  * /health, /status
  * process.Process Start: exit code, stdout/stderr, env, cwd, signal, timeout
  * /files: raw write/read, Range 206, unsatisfiable 416, conditional 304
  * filesystem.Filesystem: MakeDir, Stat, ListDir, Move, Remove
  * filesystem.Filesystem WatchDir: inotify create event
  * process.Process PTY: Start output, Connect, SendInput, Update, SendSignal
  * 24 concurrent command + file operations

Usage (from the cube-envd directory, after `cargo build --release`):

    python3 scripts/e2e_smoke.py

Environment overrides:
    CUBE_ENVD_BIN        path to the cube-envd binary
    CUBE_ENVD_E2E_PORT   listen port (default 45984)
"""
import base64
import http.client
import json
import os
import struct
import subprocess
import sys
import tempfile
import threading
import time

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(HERE)
HOST = "127.0.0.1"
PORT = int(os.environ.get("CUBE_ENVD_E2E_PORT", "45984"))
BIN = os.environ.get("CUBE_ENVD_BIN", os.path.join(REPO, "target", "release", "cube-envd"))
LOG = os.path.join(tempfile.gettempdir(), "cube-envd-e2e.log")

FAILURES = []


def check(name, condition, detail=""):
    if condition:
        print(f"  PASS {name}")
    else:
        print(f"  FAIL {name}: {detail}")
        FAILURES.append(name)


def envelope(payload: bytes, flags: int = 0) -> bytes:
    return bytes([flags]) + struct.pack(">I", len(payload)) + payload


def request(method, path, body=b"", headers=None, port=PORT):
    conn = http.client.HTTPConnection(HOST, port, timeout=15)
    conn.request(method, path, body=body, headers=headers or {})
    resp = conn.getresponse()
    data = resp.read()
    result = (resp.status, {k.lower(): v for k, v in resp.getheaders()}, data)
    conn.close()
    return result


def stream_request(method, path, body=b"", headers=None, port=PORT):
    conn = http.client.HTTPConnection(HOST, port, timeout=15)
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


def run_command(args, timeout_ms=None, envs=None, cwd=None):
    payload = {"process": {"cmd": args[0], "args": args[1:]}}
    if envs:
        payload["process"]["envs"] = envs
    if cwd:
        payload["process"]["cwd"] = cwd
    headers = {
        "Content-Type": "application/connect+json",
        "Connect-Protocol-Version": "1",
    }
    if timeout_ms is not None:
        headers["Connect-Timeout-Ms"] = str(timeout_ms)
    conn, resp = stream_request(
        "POST",
        "/process.Process/Start",
        envelope(json.dumps(payload).encode()),
        headers,
    )
    return collect_process_stream(conn, resp)


def process_output(frames):
    stdout = b""
    stderr = b""
    end = None
    for frame in frames:
        event = frame["event"]
        if "data" in event:
            if "stdout" in event["data"]:
                stdout += base64.b64decode(event["data"]["stdout"])
            if "stderr" in event["data"]:
                stderr += base64.b64decode(event["data"]["stderr"])
        if "end" in event:
            end = event["end"]
    return stdout, stderr, end


def rpc(method, body):
    return request(
        "POST",
        f"/filesystem.Filesystem/{method}",
        json.dumps(body).encode(),
        {"Content-Type": "application/json"},
    )


def start_server(port=PORT, extra_args=None):
    if not os.path.exists(BIN):
        raise RuntimeError(f"binary not found: {BIN} (run `cargo build --release`)")
    process = subprocess.Popen(
        [BIN, "--port", str(port)] + (extra_args or []),
        stdout=open(LOG, "ab"),
        stderr=subprocess.STDOUT,
    )
    for _ in range(50):
        try:
            status, _, _ = request("GET", "/health", port=port)
            if status == 204:
                return process
        except OSError:
            pass
        time.sleep(0.1)
    process.terminate()
    raise RuntimeError("cube-envd did not become ready")


def run_tests():
    print("[health/status]")
    check("health 204", request("GET", "/health")[0] == 204)
    status, _, body = request("GET", "/status")
    payload = json.loads(body)
    check(
        "status contract",
        status == 200
        and payload["service"] == "cube-envd"
        and payload["ready"] is True
        and isinstance(payload["port"], int),
        body,
    )

    status, _, _ = request(
        "POST",
        "/init",
        json.dumps({"envVars": {"CUBE_ENVD_E2E": "on"}}).encode(),
        {"Content-Type": "application/json"},
    )
    check("init 204", status == 204)
    status, _, body = request("GET", "/envs")
    check(
        "envs echoes init",
        status == 200 and json.loads(body).get("CUBE_ENVD_E2E") == "on",
        body,
    )

    status, _, body = request("GET", "/metrics")
    metrics = json.loads(body)
    check(
        "metrics contract",
        status == 200
        and isinstance(metrics.get("cpu_count"), int)
        and metrics.get("mem_total", 0) > 0
        and "disk_total" in metrics
        and "cpu_used_pct" in metrics,
        body,
    )

    print("[process]")
    stdout, stderr, end = process_output(
        run_command(["/bin/bash", "-c", "echo -n out; echo -n err >&2; exit 3"])
    )
    check("stdout captured", stdout == b"out", stdout)
    check("stderr captured", stderr == b"err", stderr)
    check("exit code", end.get("exitCode") == 3 and end["exited"] is True, end)

    stdout, _, _ = process_output(
        run_command(["/bin/sh", "-c", 'printf %s "$CUBE_ENVD_E2E"'])
    )
    check("init env visible to command", stdout == b"on", stdout)

    stdout, _, end = process_output(
        run_command(["/bin/sh", "-c", 'printf %s "$FOO"'], envs={"FOO": "bar"})
    )
    check("request env passed", stdout == b"bar", stdout)

    tmp = tempfile.mkdtemp(prefix="cube-envd-e2e-")
    stdout, _, _ = process_output(run_command(["/bin/pwd"], cwd=tmp))
    check("request cwd applied", stdout.strip() == os.path.realpath(tmp).encode(), stdout)

    _, _, end = process_output(run_command(["/bin/sh", "-c", "kill -9 $$"]))
    check(
        "signal termination",
        end["termination"]["reason"] == "signal"
        and end["termination"]["signal"] == 9
        and end["exited"] is False,
        end,
    )

    _, _, end = process_output(run_command(["/bin/bash", "-c", "sleep 5"], timeout_ms=100))
    check(
        "timeout termination",
        end["termination"]["reason"] == "timeout" and end["error"] == "process timed out",
        end,
    )

    print("[files]")
    target = os.path.join(tmp, "nested", "file.txt")
    status, _, _ = request(
        "POST",
        f"/files?path={target}",
        b"hello world",
        {"Content-Type": "application/octet-stream"},
    )
    check("raw write", status == 200, status)
    status, headers, body = request("GET", f"/files?path={target}")
    check("file read", status == 200 and body == b"hello world", body)
    check("accept-ranges", headers.get("accept-ranges") == "bytes", headers)
    status, headers, body = request(
        "GET", f"/files?path={target}", headers={"Range": "bytes=0-4"}
    )
    check(
        "range 206",
        status == 206 and body == b"hello" and headers.get("content-range") == "bytes 0-4/11",
        (status, body),
    )
    status, _, _ = request("GET", f"/files?path={target}", headers={"Range": "bytes=99-"})
    check("range 416", status == 416, status)
    last_modified = request("GET", f"/files?path={target}")[1]["last-modified"]
    status, _, _ = request(
        "GET", f"/files?path={target}", headers={"If-Modified-Since": last_modified}
    )
    check("conditional 304", status == 304, status)

    boundary = "cubee2e"
    multipart = (
        f"--{boundary}\r\n"
        'Content-Disposition: form-data; name="file"; filename="mp.txt"\r\n'
        "Content-Type: application/octet-stream\r\n\r\n"
        "multipart-body\r\n"
        f"--{boundary}--\r\n"
    ).encode()
    mp_target = os.path.join(tmp, "mp.txt")
    status, _, _ = request(
        "POST",
        f"/files?path={mp_target}",
        multipart,
        {"Content-Type": f"multipart/form-data; boundary={boundary}"},
    )
    check(
        "multipart write",
        status == 200 and open(mp_target, "rb").read() == b"multipart-body",
        status,
    )

    print("[filesystem]")
    dir_path = os.path.join(tmp, "fsdir")
    status, _, body = rpc("MakeDir", {"path": dir_path})
    check("MakeDir", status == 200 and json.loads(body)["entry"]["type"] == "FILE_TYPE_DIRECTORY")
    status, _, body = rpc("Stat", {"path": dir_path})
    check("Stat dir", status == 200 and json.loads(body)["entry"]["name"] == "fsdir", body)
    status, _, body = rpc("ListDir", {"path": tmp})
    names = [entry["name"] for entry in json.loads(body)["entries"]]
    check("ListDir", status == 200 and {"nested", "fsdir", "mp.txt"} <= set(names), names)
    status, _, _ = rpc(
        "Move",
        {"source": mp_target, "destination": os.path.join(tmp, "moved.txt")},
    )
    check("Move", status == 200 and os.path.exists(os.path.join(tmp, "moved.txt")))
    status, _, _ = rpc("Remove", {"path": dir_path})
    check("Remove", status == 200 and not os.path.isdir(dir_path))
    status, _, _ = rpc("Stat", {"path": os.path.join(tmp, "does-not-exist")})
    check("Stat missing -> 404", status == 404, status)

    print("[watch]")
    watch_dir = os.path.join(tmp, "watched")
    os.makedirs(watch_dir)
    payload = envelope(json.dumps({"path": watch_dir}).encode())
    conn, resp = stream_request(
        "POST",
        "/filesystem.Filesystem/WatchDir",
        payload,
        {"Content-Type": "application/connect+json", "Connect-Protocol-Version": "1"},
    )
    read_envelope(resp)
    open(os.path.join(watch_dir, "created.txt"), "w").close()
    saw_create = False
    for _ in range(40):
        try:
            _, body = read_envelope(resp)
        except EOFError:
            break
        event = json.loads(body)
        if event.get("filesystem", {}).get("name") == "created.txt":
            saw_create = True
            break
    conn.close()
    check("WatchDir create event", saw_create)

    status, _, body = request(
        "POST",
        "/filesystem.Filesystem/CreateWatcher",
        json.dumps({"path": watch_dir}).encode(),
        {"Content-Type": "application/json"},
    )
    check("CreateWatcher 200", status == 200, status)
    watcher_id = json.loads(body)["watcherId"]
    open(os.path.join(watch_dir, "pull.txt"), "w").close()
    saw_pull = False
    for _ in range(40):
        status, _, body = request(
            "POST",
            "/filesystem.Filesystem/GetWatcherEvents",
            json.dumps({"watcherId": watcher_id}).encode(),
            {"Content-Type": "application/json"},
        )
        if any(
            event.get("name") == "pull.txt"
            for event in json.loads(body).get("events", [])
        ):
            saw_pull = True
            break
        time.sleep(0.05)
    check("GetWatcherEvents reports event", saw_pull)
    status, _, _ = request(
        "POST",
        "/filesystem.Filesystem/RemoveWatcher",
        json.dumps({"watcherId": watcher_id}).encode(),
        {"Content-Type": "application/json"},
    )
    check("RemoveWatcher 200", status == 200, status)
    status, _, _ = request(
        "POST",
        "/filesystem.Filesystem/GetWatcherEvents",
        json.dumps({"watcherId": "w-missing"}).encode(),
        {"Content-Type": "application/json"},
    )
    check("GetWatcherEvents missing -> 404", status == 404, status)

    print("[pty]")
    pty_payload = {
        "process": {"cmd": "/bin/bash", "args": ["-c", "printf ptyhello"]},
        "pty": {"size": {"rows": 24, "cols": 80}},
    }
    conn, resp = stream_request(
        "POST",
        "/process.Process/Start",
        envelope(json.dumps(pty_payload).encode()),
        {"Content-Type": "application/connect+json", "Connect-Protocol-Version": "1"},
    )
    pty_out = b""
    pty_end = None
    while True:
        flags, body = read_envelope(resp)
        if flags & 0x02:
            break
        event = json.loads(body)["event"]
        if "data" in event:
            pty_out += base64.b64decode(event["data"]["pty"])
        if "end" in event:
            pty_end = event["end"]
    conn.close()
    check("pty output", b"ptyhello" in pty_out, pty_out)
    check("pty end event", isinstance(pty_end, dict))

    long_payload = {
        "process": {"cmd": "/bin/bash", "args": ["-c", "sleep 30"]},
        "pty": {"size": {"rows": 24, "cols": 80}},
    }
    conn, resp = stream_request(
        "POST",
        "/process.Process/Start",
        envelope(json.dumps(long_payload).encode()),
        {"Content-Type": "application/connect+json", "Connect-Protocol-Version": "1"},
    )
    _, body = read_envelope(resp)
    pid = json.loads(body)["event"]["start"]["pid"]

    status, _, body = request(
        "POST",
        "/process.Process/List",
        b"{}",
        {"Content-Type": "application/json"},
    )
    listed = [entry["pid"] for entry in json.loads(body)["processes"]]
    check("List includes running pid", status == 200 and pid in listed, listed)

    status, _, _ = request(
        "POST",
        "/process.Process/SendInput",
        json.dumps(
            {"process": {"pid": pid}, "input": {"pty": base64.b64encode(b"x").decode()}}
        ).encode(),
        {"Content-Type": "application/json"},
    )
    check("SendInput 200", status == 200, status)
    status, _, _ = request(
        "POST",
        "/process.Process/Update",
        json.dumps(
            {"process": {"pid": pid}, "pty": {"size": {"rows": 40, "cols": 120}}}
        ).encode(),
        {"Content-Type": "application/json"},
    )
    check("Update 200", status == 200, status)

    # Connect to the same PID via a second stream.
    cconn, cresp = stream_request(
        "POST",
        "/process.Process/Connect",
        envelope(json.dumps({"process": {"pid": pid}}).encode()),
        {"Content-Type": "application/connect+json", "Connect-Protocol-Version": "1"},
    )
    _, body = read_envelope(cresp)
    check("Connect reuses pid", json.loads(body)["event"]["start"]["pid"] == pid)
    cconn.close()

    status, _, _ = request(
        "POST",
        "/process.Process/SendSignal",
        json.dumps({"process": {"pid": pid}, "signal": "SIGNAL_SIGKILL"}).encode(),
        {"Content-Type": "application/json"},
    )
    check("SendSignal 200", status == 200, status)
    killed = None
    while True:
        flags, body = read_envelope(resp)
        if flags & 0x02:
            break
        event = json.loads(body)["event"]
        if "end" in event:
            killed = event["end"]
    conn.close()
    check(
        "pty kill termination",
        killed is not None and killed["termination"]["signal"] == 9,
        killed,
    )

    print("[concurrency]")
    errors = []
    lock = threading.Lock()

    def worker(index):
        try:
            stdout, _, end = process_output(
                run_command(["/bin/bash", "-c", f"printf c{index}"])
            )
            if stdout != f"c{index}".encode() or end.get("exitCode") != 0:
                with lock:
                    errors.append(index)
            path = os.path.join(tmp, f"concurrent-{index}.txt")
            status, _, _ = request(
                "POST",
                f"/files?path={path}",
                f"body{index}".encode(),
                {"Content-Type": "application/octet-stream"},
            )
            body = request("GET", f"/files?path={path}")[2]
            if status != 200 or body != f"body{index}".encode():
                with lock:
                    errors.append(index)
        except Exception as exc:  # noqa: BLE001
            with lock:
                errors.append((index, repr(exc)))

    threads = [threading.Thread(target=worker, args=(i,)) for i in range(24)]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join()
    check("24 concurrent commands + files", not errors, errors)


def run_auth_test():
    print("[auth]")
    port = PORT + 1
    server = start_server(port)
    try:
        status, _, _ = request(
            "POST",
            "/init",
            json.dumps({"accessToken": "s3cr3t"}).encode(),
            {"Content-Type": "application/json"},
            port=port,
        )
        check("init with token 204", status == 204)
        status, _, _ = request("GET", "/status", port=port)
        check("missing token -> 401", status == 401, status)
        status, _, _ = request(
            "GET", "/status", headers={"X-Access-Token": "s3cr3t"}, port=port
        )
        check("valid token -> 200", status == 200, status)
        status, _, _ = request("GET", "/health", port=port)
        check("health stays exempt -> 204", status == 204, status)
    finally:
        server.terminate()
        try:
            server.wait(timeout=5)
        except subprocess.TimeoutExpired:
            server.kill()


def run_cli_test():
    print("[cli]")
    port = PORT + 2
    server = start_server(port, extra_args=["--unknown-flag", "value"])
    try:
        status, _, _ = request("GET", "/health", port=port)
        check("unknown flag is tolerated", status == 204, status)
    finally:
        server.terminate()
        try:
            server.wait(timeout=5)
        except subprocess.TimeoutExpired:
            server.kill()


def main():
    server = start_server()
    try:
        run_tests()
        run_auth_test()
        run_cli_test()
    finally:
        server.terminate()
        try:
            server.wait(timeout=5)
        except subprocess.TimeoutExpired:
            server.kill()

    print()
    if FAILURES:
        print(f"E2E FAILED: {len(FAILURES)} check(s): {', '.join(FAILURES)}")
        return 1
    print("E2E PASSED: all checks green")
    return 0


if __name__ == "__main__":
    sys.exit(main())
