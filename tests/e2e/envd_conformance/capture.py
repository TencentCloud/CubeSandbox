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
import threading
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


def connect_client_stream(service_method, payloads, user="user", timeout=30):
    """Send a fragmented Connect request stream and capture its response."""
    headers = {
        "Host": f"{HOST}:{PORT}",
        "Content-Type": "application/connect+json",
        "Connect-Protocol-Version": "1",
        "Authorization": basic_user(user),
        "Connection": "close",
        "Transfer-Encoding": "chunked",
    }
    s = socket.create_connection((HOST, PORT), timeout=timeout)
    result = {"frames": []}
    try:
        request = (f"POST /{service_method} HTTP/1.1\r\n" +
                   "".join(f"{k}: {v}\r\n" for k, v in headers.items()) + "\r\n")
        s.sendall(request.encode())
        for payload in payloads:
            frame = envelope(json.dumps(payload).encode())
            for part in (frame[:2], frame[2:7], frame[7:]):
                s.sendall(f"{len(part):x}\r\n".encode() + part + b"\r\n")
                time.sleep(0.01)
        s.sendall(b"0\r\n\r\n")
        f = s.makefile("rb")
        result["status_line"] = f.readline().decode().strip()
        resp_headers = {}
        while True:
            line = f.readline().decode().strip()
            if not line:
                break
            k, _, v = line.partition(":")
            resp_headers[k.lower()] = v.strip().lower()
        chunked = resp_headers.get("transfer-encoding") == "chunked"
        response_buffer = bytearray()

        def read_response(n):
            if not chunked:
                return f.read(n)
            while len(response_buffer) < n:
                size_line = f.readline().strip()
                if not size_line:
                    break
                size = int(size_line, 16)
                if size == 0:
                    f.readline()
                    break
                data = f.read(size)
                f.read(2)
                response_buffer.extend(data)
            out = bytes(response_buffer[:n])
            del response_buffer[:n]
            return out
        while True:
            hdr = read_response(5)
            if len(hdr) < 5:
                break
            size = struct.unpack(">I", hdr[1:5])[0]
            raw = read_response(size)
            result["frames"].append({"flags": hdr[0], "size": size,
                                     "payload": json.loads(raw)})
            if hdr[0] & 0x02:
                break
    finally:
        s.close()
    return result


# ---------------- A. REST ----------------
# CORS: the comparison keeps only Content-Type / Content-Encoding /
# Cache-Control, so these scenarios prove the *status* contract (a preflight is
# answered 204 here, not 405 by a router with no OPTIONS route); the
# Access-Control-* headers themselves are covered by cargo tests in
# cube-envd/src/cors.rs.
def cap_cors():
    origin = {"Origin": "https://conformance.invalid"}
    record("rest_cors_preflight", http_req(
        "OPTIONS", "/health",
        headers={**origin, "Access-Control-Request-Method": "POST"}))
    record("rest_cors_preflight_request_headers", http_req(
        "OPTIONS", "/health",
        headers={**origin, "Access-Control-Request-Method": "POST",
                 "Access-Control-Request-Headers": "content-type, x-access-token"}))
    # Method outside the configured six, and a preflight without an Origin:
    # both are still answered, just without Access-Control-Allow-Origin.
    record("rest_cors_preflight_bad_method", http_req(
        "OPTIONS", "/health",
        headers={**origin, "Access-Control-Request-Method": "TRACE"}))
    record("rest_cors_preflight_no_origin", http_req(
        "OPTIONS", "/health", headers={"Access-Control-Request-Method": "POST"}))
    # Actual (non-preflight) requests reach the handler and gain the headers.
    record("rest_cors_actual_with_origin", http_req("GET", "/health", headers=origin))
    record("rest_cors_actual_no_origin", http_req("GET", "/health"))
    # OPTIONS without Access-Control-Request-Method is an *actual* request to
    # rs/cors, which always allows OPTIONS as a method (cors.go:490-492): the
    # router answers 405 and the middleware still adds ACAO + Expose-Headers.
    # cube-envd regressed this once (Vary-only); the fixture guards it.
    record("rest_cors_options_actual", http_req("OPTIONS", "/health", headers=origin))


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


# Runs ABSOLUTELY LAST (after compose): the first scenario configures a
# daemon-wide access token, after which every request must carry
# X-Access-Token. Upstream /init is on the authorization whitelist, so the
# token lifecycle is decided by the *body's* accessToken field alone.
def cap_init_token():
    tok = "tok-baseline"
    auth = {"X-Access-Token": tok}
    # Rejected bodies first (they change no state, so they are safe to run
    # before the token is configured): upstream decodes into a typed body and
    # answers any decode failure with a bare 400. An empty accessToken is one
    # of them (*SecureToken.UnmarshalJSON rejects ""), and so is a timestamp
    # that is not RFC3339 — no zone, or a day the calendar does not have.
    record("rest_init_token_empty", http_req(
        "POST", "/init", json.dumps({"accessToken": ""}).encode(),
        {"Content-Type": "application/json"}))
    record("rest_init_timestamp_no_zone", http_req(
        "POST", "/init", json.dumps({"timestamp": "1970-01-01T00:00:00.5"}).encode(),
        {"Content-Type": "application/json"}))
    record("rest_init_timestamp_bad_day", http_req(
        "POST", "/init", json.dumps({"timestamp": "2023-02-31T00:00:00Z"}).encode(),
        {"Content-Type": "application/json"}))
    # Out of the i64-nanosecond range: upstream's UnixNano() wraps and loses
    # the gate comparison, so it answers 204 without applying anything and
    # without moving the gate. cube-envd answers 400 (an out-of-range
    # timestamp is a caller bug) — a DECLARED-DIFF in conformance.py. Either
    # way nothing is applied and the gate does not move, so the follow-up
    # (ordinary timestamp) must still apply; that second scenario passes
    # unchanged on both sides.
    record("rest_init_timestamp_out_of_range", http_req(
        "POST", "/init",
        json.dumps({"timestamp": "9999-01-01T00:00:00Z",
                    "envVars": {"OUT_OF_RANGE": "1"}}).encode(),
        {"Content-Type": "application/json"}))
    record("rest_init_after_out_of_range", http_req(
        "POST", "/init",
        json.dumps({"timestamp": "2030-01-01T00:00:00Z",
                    "envVars": {"AFTER_OUT_OF_RANGE": "1"}}).encode(),
        {"Content-Type": "application/json"}))
    # First-time setup: no token configured yet, the body token is accepted.
    record("rest_init_token_first_set", http_req(
        "POST", "/init",
        json.dumps({"envVars": {"INIT_A": "1"}, "accessToken": tok}).encode(),
        {"Content-Type": "application/json"}))
    # Token set + different body token -> 401 "access token validation failed".
    record("rest_init_token_mismatch", http_req(
        "POST", "/init",
        json.dumps({"envVars": {"INIT_B": "2"}, "accessToken": "tok-other"}).encode(),
        {"Content-Type": "application/json"}))
    # Token set + body omits the token -> 401 "access token reset not authorized".
    record("rest_init_token_absent", http_req(
        "POST", "/init", json.dumps({"envVars": {"INIT_C": "3"}}).encode(),
        {"Content-Type": "application/json"}))
    # Matching body token -> 204 (and the header is not consulted at all).
    record("rest_init_token_match", http_req(
        "POST", "/init",
        json.dumps({"envVars": {"INIT_D": "4"}, "accessToken": tok}).encode(),
        {"Content-Type": "application/json"}))
    # Timestamp gate: a newer timestamp is applied...
    record("rest_init_timestamp_newer", http_req(
        "POST", "/init",
        json.dumps({"timestamp": "2030-01-01T00:00:00Z", "envVars": {"INIT_F": "6"},
                    "accessToken": tok}).encode(),
        {"Content-Type": "application/json"}))
    # ...and an older one is dropped (204 without applying INIT_G).
    record("rest_init_timestamp_stale", http_req(
        "POST", "/init",
        json.dumps({"timestamp": "2001-01-01T00:00:00Z", "envVars": {"INIT_G": "7"},
                    "accessToken": tok}).encode(),
        {"Content-Type": "application/json"}))
    # The gate runs *before* token validation, so a stale request is not 401.
    record("rest_init_timestamp_stale_skips_validation", http_req(
        "POST", "/init",
        json.dumps({"timestamp": "2001-01-01T00:00:00Z", "accessToken": "tok-other"}).encode(),
        {"Content-Type": "application/json"}))
    # Unparseable timestamp -> 400 (upstream fails while decoding the body).
    record("rest_init_timestamp_invalid", http_req(
        "POST", "/init",
        json.dumps({"timestamp": "not-a-timestamp", "accessToken": tok}).encode(),
        {"Content-Type": "application/json"}))
    # No timestamp at all -> always applied.
    record("rest_init_no_timestamp", http_req(
        "POST", "/init", json.dumps({"envVars": {"INIT_H": "8"}, "accessToken": tok}).encode(),
        {"Content-Type": "application/json"}))
    # defaultUser overrides the fallback user for later requests.
    record("rest_init_default_user", http_req(
        "POST", "/init", json.dumps({"defaultUser": "user", "accessToken": tok}).encode(),
        {"Content-Type": "application/json"}))
    # Self-contained fixture: compose above deleted base_a.txt, so upload the
    # probe file here (as user) and fetch it back with no username and no
    # Basic auth — only defaultUser can resolve it under /home/user.
    record("rest_files_default_user_upload", http_req(
        "POST", "/files?path=/home/user/init_user.txt&username=user", b"init-user-file\n",
        {"Content-Type": "application/octet-stream", **auth}))
    record("rest_files_default_user_relative", http_req(
        "GET", "/files?path=init_user.txt", headers=auth))
    record("rest_files_default_user_absent", http_req(
        "GET", "/files?path=/root/init_user.txt", headers=auth))
    # defaultWorkdir substitutes for an empty path (upstream
    # execcontext.ResolveDefaultWorkdir), then is anchored at the home dir.
    record("rest_init_default_workdir", http_req(
        "POST", "/init", json.dumps({"defaultWorkdir": "init_wd", "accessToken": tok}).encode(),
        {"Content-Type": "application/json"}))
    record("rest_files_default_workdir_nopath", http_req("GET", "/files?username=user", headers=auth))
    # Which env vars actually landed (INIT_B/C/G must be absent).
    record("rest_envs_after_init_token", http_req("GET", "/envs", headers=auth))


# ---------------- B. filesystem.Filesystem unary ----------------
def cap_fs():
    # Self-contained fixture set: base_a.txt / base_b.bin are uploaded HERE
    # (cap_rest creates the same files for its download scenarios, but
    # `--which fs` must not depend on a preceding rest run — recording on a
    # bare container used to capture 404s for the Stat/Move golden paths
    # while the comparison still "passed").
    # Sweep first so a rerun on a reused container sees the same /home/user
    # on both sides: residue from compose (base_c.txt) and from this group's
    # own probes (base_b2.bin left by fs_move, zz_empty/zz_link/zz_suid/
    # zz_sticky) would otherwise leak into fs_listdir_*, as would an
    # interrupted cap_fs_legacy run.
    for stale in ("/home/user/base_c.txt", "/home/user/base_b2.bin",
                  "/home/user/zz_empty", "/home/user/zz_link",
                  "/home/user/zz_suid", "/home/user/zz_sticky",
                  "/home/user/zz_legacy_a.txt", "/home/user/zz_legacy_dir",
                  "/home/user/zz_legacy_link", "/home/user/zz_legacy_dir_link",
                  "/home/user/zz_legacy_dangling"):
        try:
            connect_unary("filesystem.Filesystem/Remove", {"path": stale})
        except Exception:
            pass
    # NOTE: cap_rest uploads the SAME base_a.txt content (12 bytes,
    # "hello-octet\n") for its download scenarios. Seeding here is
    # idempotent and makes `--which fs` self-contained; both sides always
    # see the identical 12-byte file, so the fs_stat_file size stays 12
    # regardless of which group ran first.
    http_req("POST", "/files?path=/home/user/base_a.txt&username=user", b"hello-octet\n",
             {"Content-Type": "application/octet-stream"})
    http_req("POST", "/files?path=/home/user/base_b.bin&username=user", bytes(range(256)),
             {"Content-Type": "application/octet-stream"})
    record("fs_stat_file", connect_unary("filesystem.Filesystem/Stat", {"path": "/home/user/base_a.txt"}))
    record("fs_stat_relative", connect_unary("filesystem.Filesystem/Stat", {"path": "base_a.txt"}))
    record("fs_stat_dir", connect_unary("filesystem.Filesystem/Stat", {"path": "/home/user"}))
    record("fs_stat_missing", connect_unary("filesystem.Filesystem/Stat", {"path": "/home/user/nope"}))
    record("fs_makedir", connect_unary("filesystem.Filesystem/MakeDir", {"path": "/home/user/base_dir/sub"}))
    record("fs_makedir_exists", connect_unary("filesystem.Filesystem/MakeDir", {"path": "/home/user/base_dir/sub"}))
    # MakeDir onto an existing FILE: upstream splits on os.Stat isDir
    # (dir.go:73-83) -> InvalidArgument "path already exists but it is not a
    # directory", NOT AlreadyExists.
    record("fs_makedir_on_file", connect_unary("filesystem.Filesystem/MakeDir", {"path": "/home/user/base_a.txt"}))
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
    # Symlink probe: the target zz_probe_gone never exists, so the link is
    # dangling by construction and the fixture does not depend on which
    # earlier scenario happened to delete a file. Both sides answer the same
    # GetEntryInfo shape: UnknownFileType (type and mode omitted as proto3
    # zeros) with the link's own stats.
    connect_stream("process.Process/Start",
                   start_req("ln -sf /home/user/zz_probe_gone /home/user/zz_link"), user="user")
    record("fs_stat_symlink_probe", connect_unary(
        "filesystem.Filesystem/Stat", {"path": "/home/user/zz_link"}))
    # Permissions format probe: Go FileMode.String() (io/fs fs.go:212-232)
    # renders setuid/setgid/sticky as PREFIX characters ('u'/'g'/'t'), not
    # ls's s/S/t in the x slot; the numeric `mode` field is Perm() = m & 0777
    # (setuid stripped). 4755 -> "urwxr-xr-x" / mode 493; 1777 file ->
    # "trwxrwxrwx" / mode 511.
    http_req("POST", "/files?path=/home/user/zz_suid&username=user", b"x",
             {"Content-Type": "application/octet-stream"})
    http_req("POST", "/files?path=/home/user/zz_sticky&username=user", b"x",
             {"Content-Type": "application/octet-stream"})
    connect_stream("process.Process/Start",
                   start_req("chmod 4755 /home/user/zz_suid; chmod 1777 /home/user/zz_sticky"),
                   user="user")
    record("fs_stat_suid", connect_unary(
        "filesystem.Filesystem/Stat", {"path": "/home/user/zz_suid"}))
    record("fs_stat_sticky", connect_unary(
        "filesystem.Filesystem/Stat", {"path": "/home/user/zz_sticky"}))


def cap_fs_legacy():
    # Legacy SDK (User-Agent "connect-python") conformance (item 1.6).
    # Self-contained BY DESIGN: every path it reads is created right here, so
    # `--which fs-legacy` reproduces on its own.
    UA = {"User-Agent": "connect-python"}
    # Fixtures: one plain file (Stat), a directory with two plain files
    # (ListDir `entries`), and three symlink shapes (A-3 family).
    http_req("POST", "/files?path=/home/user/zz_legacy_a.txt&username=user", b"legacy",
             {"Content-Type": "application/octet-stream"})
    for name in ("a.txt", "b.txt"):
        http_req("POST", f"/files?path=/home/user/zz_legacy_dir/{name}&username=user", b"x",
                 {"Content-Type": "application/octet-stream"})
    # Symlink shapes exercised under the legacy UA: link-to-file,
    # link-to-directory, and a dangling link (target zz_legacy_gone never
    # exists). Both sides classify via GetEntryInfo (links followed; a
    # dangling target -> UNSPECIFIED), so the narrowed values stay inside the
    # legacy 3-value FileType enum.
    connect_stream("process.Process/Start", start_req(
        "ln -sf /home/user/zz_legacy_a.txt /home/user/zz_legacy_link"))
    connect_stream("process.Process/Start", start_req(
        "ln -sf /home/user/zz_legacy_dir /home/user/zz_legacy_dir_link"))
    connect_stream("process.Process/Start", start_req(
        "ln -sf /home/user/zz_legacy_gone /home/user/zz_legacy_dangling"))
    # A-1: `entry` narrowed to {name,type,path} + X-E2B-Legacy-SDK: true.
    record("fs_legacy_stat", connect_unary(
        "filesystem.Filesystem/Stat", {"path": "/home/user/zz_legacy_a.txt"}, extra_headers=UA))
    # A-2: `entries` narrowed element-wise — the other half of narrow().
    # Listed in a private directory so the comparison inherits neither the
    # symlink cases nor /home/user's container-state drift.
    record("fs_legacy_listdir", connect_unary(
        "filesystem.Filesystem/ListDir", {"path": "/home/user/zz_legacy_dir"},
        extra_headers=UA))
    # A-3: link-to-file narrows to FILE_TYPE_FILE.
    record("fs_legacy_stat_symlink", connect_unary(
        "filesystem.Filesystem/Stat", {"path": "/home/user/zz_legacy_link"}, extra_headers=UA))
    # A-3b: link-to-directory narrows to FILE_TYPE_DIRECTORY (followed type).
    record("fs_legacy_stat_symlink_dir", connect_unary(
        "filesystem.Filesystem/Stat", {"path": "/home/user/zz_legacy_dir_link"}, extra_headers=UA))
    # A-3c: dangling link narrows to FILE_TYPE_UNSPECIFIED — the proto3 zero,
    # omitted from the JSON, so the narrowed entry is {name, path} only
    # (Stat itself still succeeds: the link exists, only the target is gone).
    record("fs_legacy_stat_symlink_dangling", connect_unary(
        "filesystem.Filesystem/Stat", {"path": "/home/user/zz_legacy_dangling"}, extra_headers=UA))
    # A-4: Remove already answers `{}` — only the header can differ.
    record("fs_legacy_remove", connect_unary(
        "filesystem.Filesystem/Remove", {"path": "/home/user/zz_legacy_a.txt"},
        extra_headers=UA))
    # A-5: errors must NOT be narrowed and must NOT carry X-E2B-Legacy-SDK
    # (upstream WrapUnary returns the error before shouldHideChanges).
    record("fs_legacy_stat_missing", connect_unary(
        "filesystem.Filesystem/Stat", {"path": "/home/user/nope"}, extra_headers=UA))
    # Leave no residue: no later scenario reads these, and cap_fs sweeps them
    # as stale before its ListDir if a run was interrupted in between.
    for stale in ("/home/user/zz_legacy_link", "/home/user/zz_legacy_dir",
                 "/home/user/zz_legacy_dir_link", "/home/user/zz_legacy_dangling"):
        try:
            connect_unary("filesystem.Filesystem/Remove", {"path": stale})
        except Exception:
            pass


# ---------------- C. process.Process ----------------
def start_req(cmd_args, envs=None, cwd=None, tag=None, stdin=False, pty=None,
              include_stdin=True):
    p = {"process": {"cmd": "/bin/bash", "args": ["-l", "-c", cmd_args]}}
    if envs:
        p["process"]["envs"] = envs
    if cwd:
        p["process"]["cwd"] = cwd
    if tag:
        p["tag"] = tag
    if include_stdin:
        p["stdin"] = stdin
    if pty:
        p["pty"] = pty
    return p


def wait_for_tag(tag, timeout=5.0):
    """Poll List until a process with `tag` is registered; return its pid.

    Removes the registration race: a Start is a streaming request whose
    process-table insertion happens server-side on a schedule the client cannot
    observe, so a fixed sleep before a selector RPC is flaky. Polling until the
    tag is visible makes the follow-up deterministic.
    """
    deadline = time.time() + timeout
    while time.time() < deadline:
        lst = connect_unary("process.Process/List", {})
        try:
            body = json.loads(lst["body"]) if lst["status"] == 200 else {}
            for p in body.get("processes", []):
                if p.get("tag") == tag:
                    return p.get("pid")
        except Exception:
            pass
        time.sleep(0.1)
    return None


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
    # stdin omitted: backwards-compatible default is enabled.
    stdin_default = {}
    def run_stdin_default():
        stdin_default["stream"] = connect_stream(
            "process.Process/Start",
            start_req("read line; printf 'default:%s' \"$line\"; sleep 1", tag="baseline-stdin-default",
                      include_stdin=False), timeout=10)
    t_default = threading.Thread(target=run_stdin_default)
    t_default.start()
    default_pid = wait_for_tag("baseline-stdin-default")
    stdin_default["input"] = connect_unary(
        "process.Process/SendInput",
        {"process": {"pid": default_pid}, "input": {"stdin": base64.b64encode(b"ok\n").decode()}})
    stdin_default["close"] = connect_unary(
        "process.Process/CloseStdin", {"process": {"pid": default_pid}})
    t_default.join(timeout=10)
    record("proc_stdin_default", stdin_default)

    # Valid pipe input + CloseStdin EOF.
    pipe_input = {}
    def run_pipe_input():
        pipe_input["stream"] = connect_stream(
            "process.Process/Start",
            start_req("cat", tag="baseline-pipe-input", stdin=True), timeout=10)
    t_pipe = threading.Thread(target=run_pipe_input)
    t_pipe.start()
    pipe_pid = wait_for_tag("baseline-pipe-input")
    pipe_input["input"] = connect_unary(
        "process.Process/SendInput",
        {"process": {"pid": pipe_pid}, "input": {"stdin": base64.b64encode(b"pipe\n").decode()}})
    pipe_input["malformed"] = connect_unary(
        "process.Process/SendInput", {"process": {"pid": pipe_pid}, "input": {}})
    pipe_input["close"] = connect_unary(
        "process.Process/CloseStdin", {"process": {"pid": pipe_pid}})
    t_pipe.join(timeout=10)
    record("proc_pipe_input_close", pipe_input)

    # Fragmented client-streaming input: split envelope headers and payload
    # across several HTTP chunks, then close stdin to deliver EOF.
    stream_input = {}
    def run_stream_input_target():
        stream_input["stream"] = connect_stream(
            "process.Process/Start",
            start_req("cat", tag="baseline-stream-input", stdin=True), timeout=10)
    t_stream = threading.Thread(target=run_stream_input_target)
    t_stream.start()
    stream_pid = wait_for_tag("baseline-stream-input")
    stream_input["input"] = connect_client_stream(
        "process.Process/StreamInput",
        [{"start": {"process": {"pid": stream_pid}}},
         {"data": {"input": {"stdin": base64.b64encode(b"fragmented\n").decode()}}}],
        timeout=10)
    stream_input["close"] = connect_unary(
        "process.Process/CloseStdin", {"process": {"pid": stream_pid}})
    t_stream.join(timeout=10)
    record("proc_stream_input_fragmented", stream_input)

    # The malformed input result is recorded with the valid pipe process above,
    # so the service reaches the input oneof validation path.
    record("proc_sendinput_malformed", pipe_input["malformed"])
    # signal kill: start sleep with tag, then List + SendSignal + observe end event
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

    # PTY start (both implementations emit data.pty frames)
    record("proc_pty_probe", connect_stream(
        "process.Process/Start", start_req("echo pty-test", pty={"size": {"cols": 80, "rows": 24}}),
        timeout=10, read_deadline=5))

    # PTY resize (Update): start a pty that reports its window size, resize it
    # mid-flight, and record both the unary response and the size the process
    # observes afterward. The coalesced pty stream is "24 80\r\n43 132\r\n"
    # when both the Start-time size and the resize take effect.
    upd = {}
    upd_stream = {}

    def run_resize():
        upd_stream["r"] = connect_stream(
            "process.Process/Start",
            start_req("stty size; sleep 2; stty size", tag="baseline-resize",
                      pty={"size": {"cols": 80, "rows": 24}}),
            timeout=15)

    t = threading.Thread(target=run_resize)
    t.start()
    time.sleep(0.6)  # first `stty size` is done; the process is inside `sleep 2`
    upd["update"] = connect_unary(
        "process.Process/Update",
        {"process": {"tag": "baseline-resize"},
         "pty": {"size": {"cols": 132, "rows": 43}}})
    t.join(timeout=15)
    upd["stream"] = upd_stream["r"]
    record("proc_update_resize", upd)

    # Update error/no-op shapes:
    # - unknown selector is not_found (resolution happens before the resize)
    record("proc_update_unknown_process", connect_unary(
        "process.Process/Update",
        {"process": {"pid": 99999}, "pty": {"size": {"cols": 10, "rows": 10}}}))
    # - missing pty on a live process is a silent no-op success
    def run_no_pty():
        connect_stream("process.Process/Start",
                       start_req("sleep 10", tag="baseline-nopty",
                                 pty={"size": {"cols": 80, "rows": 24}}), timeout=20)
    t2 = threading.Thread(target=run_no_pty)
    t2.start()
    no_pty_pid = wait_for_tag("baseline-nopty")
    record("proc_update_missing_pty", connect_unary(
        "process.Process/Update", {"process": {"pid": no_pty_pid}}))
    # - resizing a non-pty (pipe-spawned) process is an internal error
    def run_pipe():
        connect_stream("process.Process/Start",
                       start_req("sleep 10", tag="baseline-pipe"), timeout=20)
    t3 = threading.Thread(target=run_pipe)
    t3.start()
    pipe_pid = wait_for_tag("baseline-pipe")
    record("proc_update_non_pty", connect_unary(
        "process.Process/Update",
        {"process": {"pid": pipe_pid}, "pty": {"size": {"cols": 10, "rows": 10}}}))
    t2.join(timeout=20)
    t3.join(timeout=20)

    # Connect attach: start a tagged process, then attach to it mid-flight and
    # record the attach stream (start → data → end; no history replay).
    def run_attach_target():
        # Keep the Start stream open (read to completion) so the process stays
        # alive and its output bus stays populated while we attach below.
        connect_stream("process.Process/Start",
                       start_req("sleep 2; echo CONNECT_MARK >&2", tag="baseline-connect"),
                       timeout=30)

    t = threading.Thread(target=run_attach_target)
    t.start()
    time.sleep(0.8)  # attach while the target is still running
    record("proc_connect_attach", connect_stream(
        "process.Process/Connect", {"process": {"tag": "baseline-connect"}},
        timeout=30, read_deadline=10))
    t.join(timeout=15)

    # Connect flat-selector not-found: pid/tag wording matches upstream.
    record("proc_connect_flat_missing", connect_stream(
        "process.Process/Connect", {"process": {"pid": 99999}}, timeout=8, read_deadline=5))
    # Nested (non-flat) selector: upstream rejects the shape outright.
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
    if which in ("all", "cors"):
        cap_cors()
    if which in ("all", "rest"):
        cap_rest()
    if which in ("all", "fs"):
        cap_fs()
    if which in ("all", "fs-legacy"):
        cap_fs_legacy()
    if which in ("all", "proc"):
        cap_process()
    if which in ("all", "compose"):
        cap_compose()
    if which in ("all", "init_token"):
        cap_init_token()
    print(f"\n{len(FIXTURES)} fixtures written to {OUTDIR}")
