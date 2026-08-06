#!/usr/bin/env python3
"""Capture Go envd (2026.16 / v0.5.13) behavior baseline as golden fixtures.

Targets a running envd at BASE (default http://127.0.0.1:49983).
Writes one JSON fixture per scenario into OUTDIR, plus a summary.

Only stdlib is used (urllib/http.client/socket) so raw frames can be recorded.
"""
import base64
import json
import os
import socket
import struct
import sys
import time
import urllib.error
import urllib.request

BASE = os.environ.get("ENVD_BASE", "http://127.0.0.1:49983")
HOST = BASE.split("//", 1)[1].split(":")[0]
PORT = int(BASE.rsplit(":", 1)[1])
OUTDIR = os.environ.get("OUTDIR", "fixtures")
os.makedirs(OUTDIR, exist_ok=True)

FIXTURES = {}


def record(name, data):
    FIXTURES[name] = data
    with open(os.path.join(OUTDIR, f"{name}.json"), "w") as f:
        json.dump(data, f, indent=2, ensure_ascii=False)
    print(f"[ok] {name}")


def http_req(method, path, body=None, headers=None, timeout=10):
    req = urllib.request.Request(BASE + path, data=body, method=method)
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return {
                "status": resp.status,
                "headers": dict(resp.headers),
                "body": resp.read().decode("utf-8", "replace"),
            }
    except urllib.error.HTTPError as e:
        return {
            "status": e.code,
            "headers": dict(e.headers),
            "body": e.read().decode("utf-8", "replace"),
        }


def basic_user(user):
    return "Basic " + base64.b64encode(f"{user}:".encode()).decode()


def connect_unary(service_method, payload, user="user", timeout=10, extra_headers=None):
    headers = {"Content-Type": "application/json", "Authorization": basic_user(user)}
    headers.update(extra_headers or {})
    return http_req("POST", f"/{service_method}", json.dumps(payload).encode(), headers, timeout)


def envelope(payload_bytes, flags=0):
    return bytes([flags]) + struct.pack(">I", len(payload_bytes)) + payload_bytes


def connect_stream(service_method, payload, user="user", timeout=30,
                   extra_headers=None, close_after_frames=None, read_deadline=None):
    """Raw HTTP/1.1 streaming client recording every Connect frame.

    close_after_frames: abruptly close the socket after N frames (disconnect test).
    read_deadline: stop reading after this many seconds even if stream is open.
    """
    body = envelope(json.dumps(payload).encode())
    headers = {
        "Host": f"{HOST}:{PORT}",
        "Content-Type": "application/connect+json",
        "Connect-Protocol-Version": "1",
        "Authorization": basic_user(user),
        "Content-Length": str(len(body)),
        "Connection": "close",
    }
    headers.update(extra_headers or {})
    head = f"POST /{service_method} HTTP/1.1\r\n" + "".join(
        f"{k}: {v}\r\n" for k, v in headers.items()) + "\r\n"

    s = socket.create_connection((HOST, PORT), timeout=timeout)
    result = {"frames": [], "closed_early": False}
    try:
        s.sendall(head.encode() + body)
        f = s.makefile("rb")
        status_line = f.readline().decode().strip()
        result["status_line"] = status_line
        resp_headers = {}
        while True:
            line = f.readline().decode().strip()
            if not line:
                break
            k, _, v = line.partition(":")
            resp_headers[k.strip()] = v.strip()
        result["headers"] = resp_headers
        chunked = any(k.lower() == "transfer-encoding" and v.lower() == "chunked"
                      for k, v in resp_headers.items())

        def read_n(n):
            buf = b""
            while len(buf) < n:
                if chunked:
                    chunk = read_chunked(n - len(buf))
                else:
                    chunk = f.read(n - len(buf))
                if not chunk:
                    return buf
                buf += chunk
            return buf

        chunk_rest = [b""]

        def read_chunked(want):
            if chunk_rest[0]:
                out, chunk_rest[0] = chunk_rest[0][:want], chunk_rest[0][want:]
                return out
            size_line = f.readline().strip()
            if not size_line:
                return b""
            size = int(size_line, 16)
            if size == 0:
                f.readline()
                return b""
            data = f.read(size)
            f.read(2)  # CRLF
            out, chunk_rest[0] = data[:want], data[want:]
            return out

        start = time.time()
        while True:
            if read_deadline and time.time() - start > read_deadline:
                result["stopped_by_deadline"] = True
                break
            hdr = read_n(5)
            if len(hdr) < 5:
                break
            flags = hdr[0]
            size = struct.unpack(">I", hdr[1:5])[0]
            payload_b = read_n(size)
            try:
                payload_j = json.loads(payload_b)
            except Exception:
                payload_j = {"_raw_b64": base64.b64encode(payload_b).decode()}
            result["frames"].append({"flags": flags, "size": size, "payload": payload_j})
            if close_after_frames and len(result["frames"]) >= close_after_frames:
                result["closed_early"] = True
                break
            if flags & 0x02:
                break
    except socket.timeout:
        result["socket_timeout"] = True
    finally:
        try:
            s.close()
        except Exception:
            pass
    return result


# ---------------- A. REST ----------------
def cap_rest():
    record("rest_health", http_req("GET", "/health"))
    record("rest_init_envvars", http_req(
        "POST", "/init", json.dumps({"envVars": {"BASELINE_FOO": "bar42"}}).encode(),
        {"Content-Type": "application/json"}))
    record("rest_envs", http_req("GET", "/envs"))
    record("rest_metrics", http_req("GET", "/metrics"))
    # files upload: octet-stream with path query
    record("rest_files_upload_octet", http_req(
        "POST", "/files?path=/home/user/base_a.txt&username=user",
        b"hello-octet\n", {"Content-Type": "application/octet-stream"}))
    # files upload: multipart
    boundary = "----baselineB0undary"
    part = (f"--{boundary}\r\n"
            'Content-Disposition: form-data; name="file"; filename="/home/user/base_b.bin"\r\n'
            "Content-Type: application/octet-stream\r\n\r\n").encode() + bytes(range(256)) + \
        f"\r\n--{boundary}--\r\n".encode()
    record("rest_files_upload_multipart", http_req(
        "POST", "/files?username=user", part,
        {"Content-Type": f"multipart/form-data; boundary={boundary}"}))
    # download ok / relative path
    record("rest_files_download", http_req("GET", "/files?path=/home/user/base_a.txt&username=user"))
    record("rest_files_download_relative", http_req("GET", "/files?path=base_a.txt&username=user"))
    # gzip response encoding: upstream compresses, cube-envd serves identity
    # (declared difference — see conformance.py DECLARED_DIFFERENT).
    record("rest_files_gzip_accept", http_req(
        "GET", "/files?path=/home/user/base_a.txt&username=user",
        headers={"Accept-Encoding": "gzip"}))
    # download errors
    record("rest_files_err_missing", http_req("GET", "/files?path=/home/user/nope.txt&username=user"))
    record("rest_files_err_directory", http_req("GET", "/files?path=/home/user&username=user"))
    record("rest_files_err_baduser", http_req("GET", "/files?path=/etc/hostname&username=ghost9"))
    record("rest_files_err_nopath", http_req("GET", "/files?username=user"))
    # upload as root to protected path; and missing content-type
    record("rest_files_upload_root", http_req(
        "POST", "/files?path=/root/base_root.txt&username=root",
        b"root-file\n", {"Content-Type": "application/octet-stream"}))


# Runs LAST: compose deletes its source files on the Go implementation,
# which would fork the container filesystem state between implementations
# for every scenario that follows it.
def cap_compose():
    record("rest_files_compose_probe", http_req(
        "POST", "/files/compose",
        json.dumps({"source_paths": ["/home/user/base_a.txt"], "destination": "/home/user/base_c.txt",
                    "username": "user"}).encode(),
        {"Content-Type": "application/json"}))


# ---------------- B. filesystem.Filesystem unary ----------------
def cap_fs():
    record("fs_stat_file", connect_unary("filesystem.Filesystem/Stat", {"path": "/home/user/base_a.txt"}))
    record("fs_stat_relative", connect_unary("filesystem.Filesystem/Stat", {"path": "base_a.txt"}))
    record("fs_stat_dir", connect_unary("filesystem.Filesystem/Stat", {"path": "/home/user"}))
    record("fs_stat_missing", connect_unary("filesystem.Filesystem/Stat", {"path": "/home/user/nope"}))
    record("fs_makedir", connect_unary("filesystem.Filesystem/MakeDir", {"path": "/home/user/base_dir/sub"}))
    record("fs_makedir_exists", connect_unary("filesystem.Filesystem/MakeDir", {"path": "/home/user/base_dir/sub"}))
    record("fs_listdir_depth1", connect_unary("filesystem.Filesystem/ListDir", {"path": "/home/user"}))
    record("fs_listdir_depth2", connect_unary("filesystem.Filesystem/ListDir", {"path": "/home/user", "depth": 2}))
    record("fs_listdir_missing", connect_unary("filesystem.Filesystem/ListDir", {"path": "/home/user/nodir"}))
    record("fs_move", connect_unary("filesystem.Filesystem/Move",
                                    {"source": "/home/user/base_b.bin", "destination": "/home/user/base_b2.bin"}))
    record("fs_move_missing", connect_unary("filesystem.Filesystem/Move",
                                            {"source": "/home/user/nope", "destination": "/home/user/x"}))
    record("fs_remove", connect_unary("filesystem.Filesystem/Remove", {"path": "/home/user/base_dir"}))
    record("fs_remove_missing", connect_unary("filesystem.Filesystem/Remove", {"path": "/home/user/base_dir"}))
    record("fs_stat_baduser", connect_unary("filesystem.Filesystem/Stat", {"path": "/tmp"}, user="ghost9"))
    record("fs_watch_unary_probe", connect_unary("filesystem.Filesystem/CreateWatcher", {"path": "/home/user"}))
    record("fs_bad_json", http_req(
        "POST", "/filesystem.Filesystem/Stat", b"{not json",
        {"Content-Type": "application/json", "Authorization": basic_user("user")}))
    # proto3 zero-value omission (F1): empty file → no `size` key; mode-000
    # file → no `mode` key. Seed the files first via the /files + a command.
    http_req("POST", "/files?path=/home/user/zz_empty&username=user", b"",
             {"Content-Type": "application/octet-stream"})
    record("fs_stat_empty_file", connect_unary(
        "filesystem.Filesystem/Stat", {"path": "/home/user/zz_empty"}))
    connect_stream("process.Process/Start",
                   start_req("chmod 000 /home/user/zz_empty"), user="user")
    record("fs_stat_mode000", connect_unary(
        "filesystem.Filesystem/Stat", {"path": "/home/user/zz_empty"}))
    # symlink semantics (F2, declared different): cube-envd lstat's (type
    # SYMLINK, lowercase-l perms, real target); upstream follows the link.
    connect_stream("process.Process/Start",
                   start_req("ln -sf /home/user/base_a.txt /home/user/zz_link"), user="user")
    record("fs_stat_symlink_probe", connect_unary(
        "filesystem.Filesystem/Stat", {"path": "/home/user/zz_link"}))


# ---------------- C. process.Process ----------------
def start_req(cmd_args, envs=None, cwd=None, tag=None, stdin=False, pty=None):
    p = {"process": {"cmd": "/bin/bash", "args": ["-l", "-c", cmd_args]}}
    if envs:
        p["process"]["envs"] = envs
    if cwd:
        p["process"]["cwd"] = cwd
    if tag:
        p["tag"] = tag
    p["stdin"] = stdin
    if pty:
        p["pty"] = pty
    return p


def cap_process():
    record("proc_echo", connect_stream("process.Process/Start", start_req("echo hello-baseline")))
    record("proc_stderr_exit3", connect_stream(
        "process.Process/Start", start_req("echo out1; echo err1 >&2; exit 3")))
    record("proc_env_merge", connect_stream(
        "process.Process/Start",
        start_req("echo FOO=$BASELINE_FOO REQ=$REQ_VAR USER=$USER HOME=$HOME PWD=$(pwd)",
                  envs={"REQ_VAR": "req42"})))
    record("proc_cwd", connect_stream("process.Process/Start", start_req("pwd", cwd="/tmp")))
    # invalid cwd (F1/cwd fix): a missing directory must be rejected with
    # invalid_argument, NOT silently run in `/`. Both should error now.
    record("proc_cwd_missing", connect_stream(
        "process.Process/Start", start_req("pwd", cwd="/no/such/dir/xyz")))
    record("proc_root_user", connect_stream("process.Process/Start", start_req("id -u; whoami"), user="root"))
    record("proc_bad_user", connect_stream("process.Process/Start", start_req("id"), user="ghost9"))
    record("proc_missing_cmd", connect_stream(
        "process.Process/Start", {"process": {"cmd": "/no/such/bin", "args": []}, "stdin": False}))
    # signal kill: start sleep with tag, then List + SendSignal + observe end event
    import threading
    sig_result = {}

    def run_sleeper():
        sig_result["stream"] = connect_stream(
            "process.Process/Start", start_req("sleep 20", tag="baseline-sleeper"), timeout=30)

    t = threading.Thread(target=run_sleeper)
    t.start()
    time.sleep(1.0)
    sig_result["list"] = connect_unary("process.Process/List", {})
    sig_result["sendsignal"] = connect_unary(
        "process.Process/SendSignal",
        {"process": {"tag": "baseline-sleeper"}, "signal": "SIGNAL_SIGKILL"})
    t.join(timeout=15)
    record("proc_signal_kill", sig_result)
    # Nested (non-flat) selector shape: upstream rejects with an input-type
    # error; cube-envd is lenient and reports not_found for the unknown tag.
    record("proc_sendsignal_nested_probe", connect_unary(
        "process.Process/SendSignal",
        {"process": {"selector": {"tag": "nonexistent"}}, "signal": "SIGNAL_SIGKILL"}))

    # timeout: Connect-Timeout-Ms=1500 on sleep 10
    record("proc_timeout_connect_ms", connect_stream(
        "process.Process/Start", start_req("sleep 10"),
        extra_headers={"Connect-Timeout-Ms": "1500"}, timeout=8, read_deadline=6))

    # disconnect: close socket right after start event, then check List after 1s
    disc = {}
    disc["stream"] = connect_stream(
        "process.Process/Start", start_req("sleep 8; echo done > /tmp/disc_done", tag="baseline-disc"),
        close_after_frames=1)
    time.sleep(1.0)
    disc["list_after_disconnect"] = connect_unary("process.Process/List", {})
    time.sleep(8)
    disc["file_written"] = connect_unary("filesystem.Filesystem/Stat", {"path": "/tmp/disc_done"}, user="root")
    record("proc_disconnect_keeps_running", disc)

    # PTY start (Go implements it — recorded for divergence documentation)
    record("proc_pty_probe", connect_stream(
        "process.Process/Start", start_req("echo pty-test", pty={"size": {"cols": 80, "rows": 24}}),
        timeout=10, read_deadline=5))
    # Connect RPC to nonexistent pid
    record("proc_connect_missing", connect_stream(
        "process.Process/Connect", {"process": {"selector": {"pid": 99999}}}, timeout=8, read_deadline=5))
    # SendInput to non-stdin process (error shape)
    record("proc_sendinput_probe", connect_unary(
        "process.Process/SendInput",
        {"process": {"selector": {"pid": 99999}}, "input": {"stdin": base64.b64encode(b"x").decode()}}))
    # long output: 2MiB — record only sizes
    big = connect_stream("process.Process/Start",
                         start_req("head -c 2097152 /dev/zero | tr '\\0' 'a'"), timeout=60)
    import base64 as _b64
    total = sum(len(_b64.b64decode(fr["payload"].get("event", {}).get("data", {}).get("stdout", "")))
                for fr in big["frames"] if isinstance(fr.get("payload"), dict))
    record("proc_large_output_summary", {
        "status_line": big.get("status_line"),
        "stdout_bytes_total": total,
        "first_frame": big["frames"][0] if big["frames"] else None,
        "last_frames": big["frames"][-2:] if len(big["frames"]) >= 2 else big["frames"],
        "max_frame_size": max((fr["size"] for fr in big["frames"]), default=0),
    })
    # version endpoints via docker exec are captured outside this script


if __name__ == "__main__":
    which = sys.argv[1] if len(sys.argv) > 1 else "all"
    if which in ("all", "rest"):
        cap_rest()
    if which in ("all", "fs"):
        cap_fs()
    if which in ("all", "proc"):
        cap_process()
    if which in ("all", "compose"):
        cap_compose()
    print(f"\n{len(FIXTURES)} fixtures written to {OUTDIR}")
