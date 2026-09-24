"""Run real cgroup v2 termination tests in an isolated Linux/QEMU guest.

Run as root: python3 termination_e2e.py /path/to/cube-envd
Requires a writable cgroup v2 root with memory/cpu enabled, Python 3, and
the Python SDK's httpx/requests dependencies on PYTHONPATH for SDK checks.
Only the daemon, cgroup subtree and temporary files created here are removed.
"""

import base64
import hashlib
import http.client
import json
import os
from pathlib import Path
import platform
import shlex
import socket
import struct
import subprocess
import sys
import tempfile
import time
from types import SimpleNamespace


AUTH = {"Authorization": "Basic cm9vdDo="}
LIMIT = 64 * 1024 * 1024
ALLOCATE = "blocks=[]\nwhile True: blocks.append(bytearray(1024*1024))"
DESCENDANT_OOM = (
    "import os, signal, time\n"
    "child=os.fork()\n"
    "if child == 0:\n"
    " open('/proc/self/oom_score_adj','w').write('1000')\n"
    " blocks=[]\n"
    " while True: blocks.append(bytearray(1024*1024))\n"
    "_,status=os.waitpid(child,0)\n"
    "assert os.WIFSIGNALED(status) and os.WTERMSIG(status)==signal.SIGKILL\n"
    "print('descendant-oom',flush=True)\n"
)


def wait_for(predicate, message, timeout=10):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if predicate():
            return
        time.sleep(0.05)
    raise AssertionError(message)


class ProcessStream:
    def __init__(self, port, command, args, timeout=None, pty=False):
        self.connection = http.client.HTTPConnection("127.0.0.1", port, timeout=20)
        payload = {"process": {"cmd": command, "args": args}, "stdin": False}
        if pty:
            payload["pty"] = {"size": {"cols": 80, "rows": 24}}
        raw = json.dumps(payload).encode()
        headers = {**AUTH, "Content-Type": "application/connect+json",
                   "Connect-Protocol-Version": "1"}
        if timeout is not None:
            headers["Connect-Timeout-Ms"] = str(timeout)
        self.connection.request("POST", "/process.Process/Start",
                                b"\0" + struct.pack(">I", len(raw)) + raw, headers)
        self.response = self.connection.getresponse()
        assert self.response.status == 200
        self.end = None
        self.trailer = None
        self.output = b""
        self.pid = None

    def read(self):
        header = self.response.read(5)
        assert len(header) == 5, "stream ended without a complete envelope"
        flags, length = struct.unpack(">BI", header)
        body = self.response.read(length)
        assert len(body) == length
        payload = json.loads(body)
        if flags == 2:
            self.trailer = payload
        else:
            assert flags == 0
            event = payload.get("event", {})
            if "start" in event:
                self.pid = event["start"]["pid"]
            if "end" in event:
                self.end = event["end"]
            for chunk in event.get("data", {}).values():
                self.output += base64.b64decode(chunk)
        return payload

    def collect(self):
        try:
            while self.trailer is None:
                self.read()
            return self
        finally:
            self.connection.close()


def unary(port, method, payload):
    connection = http.client.HTTPConnection("127.0.0.1", port, timeout=10)
    try:
        connection.request("POST", "/process.Process/" + method,
                           json.dumps(payload), {**AUTH, "Content-Type": "application/json"})
        response = connection.getresponse()
        return response.status, json.loads(response.read())
    finally:
        connection.close()


def run(binary):
    assert os.geteuid() == 0, "requires root in an isolated test guest"
    binary = Path(binary).resolve()
    root = Path(f"/sys/fs/cgroup/cube-envd-e2e-{os.getpid()}")
    root.mkdir()
    daemon = None
    passed = 0

    def check(name, condition):
        nonlocal passed
        assert condition, name
        passed += 1
        print(f"PASS {name}", flush=True)

    try:
        root.joinpath("memory.max").write_text(str(256 * 1024 * 1024))
        root.joinpath("memory.swap.max").write_text("0")
        with tempfile.TemporaryDirectory(prefix="cube-envd-e2e-") as directory:
            with socket.socket() as listener:
                listener.bind(("127.0.0.1", 0))
                port = listener.getsockname()[1]
            log = Path(directory, "envd.log")
            with log.open("w") as output:
                daemon = subprocess.Popen(
                    [str(binary), "-port", str(port)], stdout=output, stderr=subprocess.STDOUT,
                    env={**os.environ, "CUBE_ENVD_CGROUP_ROOT": str(root),
                         "CUBE_ENVD_CGROUP_MEMORY_MAX_BYTES": str(LIMIT)},
                )
            try:
                def healthy():
                    try:
                        connection = http.client.HTTPConnection("127.0.0.1", port, timeout=1)
                        connection.request("GET", "/health")
                        status = connection.getresponse().status
                        connection.close()
                        return status == 204
                    except OSError:
                        return False

                wait_for(healthy, "daemon did not become healthy")
                print(f"kernel={platform.release()} binary_sha256={hashlib.sha256(binary.read_bytes()).hexdigest()}", flush=True)
                check("cgroup memory limit active", root.joinpath("user/memory.max").read_text().strip() == str(LIMIT))

                def start(code, timeout=None, pty=False):
                    return ProcessStream(port, sys.executable, ["-c", code], timeout, pty)

                def clean():
                    return not list(root.glob("*/process-*"))

                normal = start("print('normal')").collect()
                check("normal exit omits termination metadata", normal.end.get("exited") is True and
                      not any(key in normal.end for key in ("signal", "oomKilled", "killedBy")))
                signaled = start("import os,signal; os.kill(os.getpid(),signal.SIGTERM)").collect()
                check("self SIGTERM metadata", signaled.end.get("signal") == 15 and "killedBy" not in signaled.end)

                for pty in (False, True):
                    stream = start("import time; time.sleep(30)", pty=pty)
                    stream.read()
                    membership = Path(f"/proc/{stream.pid}/cgroup").read_text()
                    check(f"{'PTY' if pty else 'pipe'} placed in dedicated leaf", str(root).removeprefix("/sys/fs/cgroup") +
                          ("/ptys/process-" if pty else "/user/process-") in membership)
                    status, _ = unary(port, "SendSignal", {"process": {"pid": stream.pid}, "signal": "SIGNAL_SIGKILL"})
                    stream.collect()
                    check(f"{'PTY' if pty else 'pipe'} user SIGKILL metadata", status == 200 and
                          stream.end.get("signal") == 9 and stream.end.get("killedBy") == "user")

                timeout = start("import time; time.sleep(30)", timeout=200).collect()
                check("timeout EndEvent precedes deadline trailer", timeout.end.get("signal") == 9 and
                      timeout.end.get("killedBy") == "timeout" and timeout.trailer.get("error", {}).get("code") == "deadline_exceeded")
                oom = start(ALLOCATE).collect()
                check("real main-process OOM", oom.end.get("signal") == 9 and
                      oom.end.get("oomKilled") is True and oom.end.get("killedBy") == "oom")
                descendant = start(DESCENDANT_OOM).collect()
                check("descendant OOM does not mislabel successful parent", b"descendant-oom" in descendant.output and
                      descendant.end.get("exited") is True and not descendant.end.get("oomKilled", False) and
                      "killedBy" not in descendant.end)
                collision = start(DESCENDANT_OOM + "time.sleep(30)\n", timeout=3000).collect()
                check("timeout wins over descendant OOM", b"descendant-oom" in collision.output and
                      collision.end.get("killedBy") == "timeout" and not collision.end.get("oomKilled", False))
                collision = start(DESCENDANT_OOM + "time.sleep(30)\n")
                while b"descendant-oom" not in collision.output:
                    collision.read()
                status, _ = unary(port, "SendSignal", {"process": {"pid": collision.pid}, "signal": "SIGNAL_SIGKILL"})
                collision.collect()
                check("user kill wins over descendant OOM", status == 200 and collision.end.get("killedBy") == "user" and
                      not collision.end.get("oomKilled", False))

                escaped = start("import os,time\nchild=os.fork()\nif child == 0:\n os.setsid()\n time.sleep(30)\nelse:\n print(child,flush=True)\n").collect()
                child = int(escaped.output.strip())
                def dead():
                    try:
                        return Path(f"/proc/{child}/stat").read_text().split(") ", 1)[1].startswith("Z")
                    except FileNotFoundError:
                        return True
                wait_for(dead, "setsid descendant survived command cleanup")
                wait_for(clean, "process cgroup leaves leaked")
                check("setsid descendant reaped from leaf", True)

                allocation_limit = root.joinpath("user/cgroup.max.descendants")
                allocation_limit.write_text("0")
                marker = Path(directory, "must-not-execute")
                for attempt in range(2):
                    rejected = start(f"open({str(marker)!r},'w').write('escaped')").collect()
                    check(f"leaf allocation failure rejects request {attempt + 1}", rejected.pid is None and
                          rejected.trailer.get("error", {}).get("code") == "resource_exhausted" and not marker.exists())
                allocation_limit.write_text("max")
                recovered = start("print(open('/proc/self/cgroup').read())").collect()
                check("allocation recovers without restart or bypass", b"/user/process-" in recovered.output and
                      recovered.end.get("exited") is True and not marker.exists())

                status, body = unary(port, "SendSignal", {"process": {}, "signal": "SIGNAL_SIGKILL"})
                check("empty selector matches upstream unimplemented", status == 501 and body.get("code") == "unimplemented")

                from cubesandbox._commands import Commands
                from cubesandbox._pty import Pty, PtySize
                import httpx

                with httpx.Client(trust_env=False) as client:
                    sandbox = SimpleNamespace(_client=client, _data={}, get_host=lambda envd_port: f"127.0.0.1:{port}")
                    commands = Commands(sandbox)
                    result = commands.run("echo sdk-live")
                    check("Python SDK command roundtrip", result.stdout == "sdk-live\n" and result.exit_code == 0)
                    result = commands.run("kill -TERM $$")
                    check("Python SDK reads real signal", result.signal == 15)
                    result = commands.run(shlex.join([sys.executable, "-c", ALLOCATE]))
                    check("Python SDK reads real OOM fields", result.signal == 9 and result.oom_killed and result.killed_by == "oom")
                    handle = Pty(sandbox).create(PtySize(rows=24, cols=80))
                    handle.kill()
                    try:
                        handle.wait()
                    except Exception:
                        if handle.killed_by != "user":
                            raise
                    check("Python PTY SDK reads real user-kill fields", handle.signal == 9 and handle.killed_by == "user")
                wait_for(clean, "process cgroup leaves leaked after SDK tests")
                check("all command leaves cleaned up", True)
                print(f"RESULT {passed} passed, 0 failed", flush=True)
            except BaseException:
                print(log.read_text(), file=sys.stderr)
                raise
    finally:
        if daemon is not None:
            daemon.terminate()
            try:
                daemon.wait(timeout=5)
            except subprocess.TimeoutExpired:
                daemon.kill()
                daemon.wait()
        root.joinpath("cgroup.kill").write_text("1")
        def remove_groups():
            try:
                for path in sorted(root.rglob("*"), key=lambda item: len(item.parts), reverse=True):
                    if path.is_dir():
                        path.rmdir()
                root.rmdir()
                return True
            except OSError:
                return False
        wait_for(remove_groups, f"could not clean test subtree {root}")


if __name__ == "__main__":
    run(sys.argv[1])
