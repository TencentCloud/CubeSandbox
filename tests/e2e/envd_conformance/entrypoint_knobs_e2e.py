"""End-to-end check that a deployment can tune envd through the entrypoint.

Run as a normal user: python3 entrypoint_knobs_e2e.py /path/to/cube-envd

This is the deployment-path counterpart of the cgroup checks in
`termination_e2e.py` (which export `CUBE_ENVD_CGROUP_*` when they launch the
daemon): the knobs added for the `/files` pipeline are reachable from the
environment *and* from flags, and the only user-facing way to pass flags is
`ENVD_EXTRA_ARGS` in `docker/cube-entrypoint.sh`. Nothing else in the suite runs
the entrypoint, so without this test "the deployment can set the knobs" was an
untested claim.

What it asserts, through the entrypoint and the real binary:

  1. the entrypoint starts envd with `ENVD_EXTRA_ARGS` and `/health` answers;
  2. envd's startup line reports the values the flags asked for — with the
     environment deliberately set to *different* values, which also pins that
     the flag wins;
  3. the cap is enforced: with `-download-max-bodies` at the floor, `CAP + 1`
     concurrent large downloads give `CAP` bodies and one `503`, while a body
     that fits in one chunk stays exempt (`200`) even with every slot held.

Only the daemon, one temporary directory and its log file are created here.
"""

import http.client
import os
from pathlib import Path
import re
import socket
import subprocess
import sys
import tempfile
import time

# `tracing-subscriber` colorises its output when built with its `ansi` feature
# (the deployed 0.1.0 build is; this crate's does not), which puts escape
# sequences *inside* the field names — `blocking_threads\x1b[0m\x1b[2m=\x1b[0m8`.
# Matching the raw text then fails for a perfectly healthy daemon, so strip CSI
# sequences and collapse whitespace before looking for the values.
ANSI = re.compile(r"\x1b\[[0-9;]*[A-Za-z]")


def plain(text):
    return " ".join(ANSI.sub("", text).split())

TOKEN = "localtok"
BIG_BODY = 8 * 1024 * 1024  # far above DOWNLOAD_CHUNK, so it takes a global slot
SMALL_BODY = 4096  # fits in one chunk: exempt from the cap
POOL_FLAG = 8  # -blocking-threads
POOL_ENV = 32  # CUBE_ENVD_BLOCKING_THREADS: must lose to the flag
# A pool of 8 needs blocking 2 + buffered 4 = 6, so this is the floor: a smaller
# value would be raised to it and the test would prove nothing about the cap.
CAP = 6
ENTRYPOINT = Path(__file__).resolve().parents[3] / "docker" / "cube-entrypoint.sh"

passed = []
failed = []


def check(name, ok, detail=""):
    (passed if ok else failed).append(name)
    print(f"{'PASS' if ok else 'FAIL'}  {name}{('  — ' + detail) if detail else ''}", flush=True)


def free_port():
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        return listener.getsockname()[1]


def status_of(port, path, timeout=5.0):
    connection = http.client.HTTPConnection("127.0.0.1", port, timeout=timeout)
    try:
        connection.request("GET", path, headers={"X-Access-Token": TOKEN})
        response = connection.getresponse()
        response.read()
        return response.status
    finally:
        connection.close()


def selftest():
    """The matcher must survive a colorised log line (see ANSI/plain)."""
    colorised = (
        "2026-09-13T11:15:18Z  INFO runtime limits "
        "\x1b[3mblocking_threads\x1b[0m\x1b[2m=\x1b[0m\x1b[2m8\x1b[0m "
        "\x1b[3mdownload_blocking_producers\x1b[0m\x1b[2m=\x1b[0m\x1b[2m2\x1b[0m "
        "\x1b[3mdownload_buffered_bodies\x1b[0m\x1b[2m=\x1b[0m\x1b[2m4\x1b[0m "
        "\x1b[3mdownload_max_bodies\x1b[0m\x1b[2m=\x1b[0m\x1b[2m6\x1b[0m "
        "\x1b[3mdownload_pool_bytes\x1b[0m\x1b[2m=\x1b[0m\x1b[2m4194304\x1b[0m"
    )
    wanted = (
        f"blocking_threads={POOL_FLAG} download_blocking_producers=2 "
        f"download_buffered_bodies=4 download_max_bodies={CAP} "
        # The recycled-buffer budget follows the same knob: 4 buffered bodies
        # x 1 MiB, not the default 32 MiB.
        f"download_pool_bytes={4 * 1024 * 1024}"
    )
    assert wanted not in colorised, "the raw match must fail on colour, or this test is pointless"
    assert wanted in plain(colorised), "the stripped match must succeed"
    print("selftest: ANSI stripping works")


def main():
    if len(sys.argv) > 1 and sys.argv[1] == "--selftest":
        selftest()
        return
    binary = Path(
        sys.argv[1]
        if len(sys.argv) > 1
        else "target/x86_64-unknown-linux-musl/release/cube-envd"
    )
    if not binary.exists():
        raise SystemExit(f"cube-envd binary not found: {binary}")
    if not ENTRYPOINT.exists():
        raise SystemExit(f"entrypoint not found: {ENTRYPOINT}")

    port = free_port()
    with tempfile.TemporaryDirectory(prefix="cube-envd-entrypoint-") as directory:
        directory = Path(directory)
        log = directory / "envd.log"
        stderr_path = directory / "entrypoint.err"
        (directory / "payload.bin").write_bytes(b"x" * BIG_BODY)
        (directory / "tiny.bin").write_bytes(b"y" * SMALL_BODY)
        stderr = stderr_path.open("w")
        environment = {
            **os.environ,
            "ENVD_BIN": str(binary),
            "ENVD_PORT": str(port),
            "ENVD_LOG_FILE": str(log),
            "ENVD_ACCESS_TOKEN": TOKEN,
            "ENVD_EXTRA_ARGS": f"-blocking-threads {POOL_FLAG} -download-max-bodies {CAP}",
            # Deliberately different from the flags: the flag must win.
            "CUBE_ENVD_BLOCKING_THREADS": str(POOL_ENV),
        }
        entrypoint = subprocess.Popen(
            ["/bin/sh", str(ENTRYPOINT)], env=environment, stdout=stderr, stderr=stderr
        )
        daemon_pid = None
        held = []
        try:
            deadline = time.monotonic() + 20
            while time.monotonic() < deadline:
                if entrypoint.poll() is not None:
                    raise SystemExit(
                        f"entrypoint exited early with {entrypoint.returncode}; log: {log}"
                    )
                try:
                    if status_of(port, "/health", timeout=1.0) == 204:
                        break
                except OSError:
                    time.sleep(0.2)
            else:
                raise SystemExit(f"envd never answered /health on {port}; log: {log}")
            check("entrypoint starts envd and /health answers", True, f"port {port}")

            stderr.flush()
            started = re.search(r"started envd \(pid=(\d+)\)", stderr_path.read_text())
            daemon_pid = int(started.group(1)) if started else None
            check("entrypoint reports the envd pid", daemon_pid is not None)

            limits = ""
            deadline = time.monotonic() + 10
            while time.monotonic() < deadline:
                limits = log.read_text() if log.exists() else ""
                if "runtime limits" in limits:
                    break
                time.sleep(0.1)
            wanted = (
                f"blocking_threads={POOL_FLAG} download_blocking_producers=2 "
                f"download_buffered_bodies=4 download_max_bodies={CAP} "
                f"download_pool_bytes={4 * 1024 * 1024}"
            )
            text = plain(limits)
            line = next((ln for ln in limits.splitlines() if "runtime limits" in plain(ln)), limits[-200:])
            check(
                "ENVD_EXTRA_ARGS reaches envd, and the flag beats the environment",
                wanted in text,
                plain(line),
            )

            # The cap, behaviourally: CAP bodies in flight, the next one refused.
            for _ in range(CAP + 1):
                connection = socket.create_connection(("127.0.0.1", port), timeout=10)
                connection.sendall(
                    (
                        "GET /files?path=%s&username=root HTTP/1.1\r\n"
                        "Host: 127.0.0.1\r\nX-Access-Token: %s\r\n\r\n"
                        % (directory / "payload.bin", TOKEN)
                    ).encode()
                )
                held.append(connection)  # never read the body: the slots stay held
            time.sleep(2)
            statuses = []
            for connection in held:
                connection.settimeout(3)
                head = connection.recv(64).split(b"\r\n", 1)[0].decode(errors="replace")
                statuses.append(head.split()[1] if len(head.split()) > 1 else head)
            check(
                f"the configured cap is enforced ({CAP} served, one 503)",
                statuses.count("200") == CAP and statuses.count("503") == 1,
                f"{statuses}",
            )
            check(
                "a body that fits in one chunk stays exempt while the cap is full",
                status_of(port, "/files?path=%s&username=root" % (directory / "tiny.bin")) == 200,
                "tiny.bin",
            )
        finally:
            for connection in held:
                connection.close()
            if daemon_pid:
                try:
                    os.kill(daemon_pid, 15)
                except ProcessLookupError:
                    pass
            entrypoint.terminate()
            try:
                entrypoint.wait(timeout=5)
            except subprocess.TimeoutExpired:
                entrypoint.kill()
            if daemon_pid:
                for _ in range(50):
                    try:
                        os.kill(daemon_pid, 0)
                    except ProcessLookupError:
                        break
                    time.sleep(0.1)
                else:
                    os.kill(daemon_pid, 9)

    print(f"\n{len(passed)} passed, {len(failed)} failed")
    if failed:
        raise SystemExit(1)


main()
