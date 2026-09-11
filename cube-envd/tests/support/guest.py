# SPDX-License-Identifier: Apache-2.0

import http.client
import http.server
import json
import os
import pathlib
import signal
import socket
import socketserver
import subprocess
import threading
import time
import urllib.parse


BINARY = os.environ.get("ENVD_TEST_BINARY")
CGROUP_MOUNT = pathlib.Path(os.environ.get("ENVD_TEST_CGROUP_ROOT", "/sys/fs/cgroup"))


def free_port():
    sock = socket.socket()
    sock.bind(("127.0.0.1", 0))
    port = sock.getsockname()[1]
    sock.close()
    return port


class State:
    lock = threading.Lock()
    token = "mmds-session-token"
    metadata = {}
    mmds_requests = []
    collector_attempts = 0
    collected = []
    mmds_failures = 0
    mmds_hold = None
    collector_hold = None
    collector_release = None


class QuietHandler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_args):
        pass


class MMDSHandler(QuietHandler):
    def do_PUT(self):
        with State.lock:
            State.mmds_requests.append(("PUT", self.path, dict(self.headers)))
            fail = State.mmds_failures > 0
            State.mmds_failures = max(0, State.mmds_failures - 1)
        if fail:
            self.connection.shutdown(socket.SHUT_RDWR)
            self.connection.close()
            return
        if State.mmds_hold is not None:
            State.mmds_hold.set()
            time.sleep(2)
            return
        self.send_response(200)
        self.end_headers()
        self.wfile.write(State.token.encode())

    def do_GET(self):
        if self.headers.get('X-metadata-token') != State.token:
            self.send_error(401)
            return
        with State.lock:
            State.mmds_requests.append(("GET", self.path, dict(self.headers)))
            body = json.dumps(State.metadata).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


class CollectorHandler(QuietHandler):
    def do_POST(self):
        size = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(size)
        with State.lock:
            State.collector_attempts += 1
            first = State.collector_attempts == 1
        if State.collector_hold is not None:
            State.collector_hold.set()
            State.collector_release.wait(timeout=3)
        if first:
            self.connection.shutdown(socket.SHUT_RDWR)
            self.connection.close()
            return
        event = json.loads(body)
        with State.lock:
            State.collected.append((dict(self.headers), event))
        self.send_response(204)
        self.end_headers()


class Server:
    def __init__(self, address, handler):
        self.server = http.server.ThreadingHTTPServer(address, handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)

    def __enter__(self):
        self.thread.start()
        return self

    def __exit__(self, *_args):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=2)


class EchoHandler(socketserver.BaseRequestHandler):
    def handle(self):
        while data := self.request.recv(4096):
            self.request.sendall(data)


class EchoServer:
    def __init__(self, family=socket.AF_INET):
        class TCPServer(socketserver.ThreadingTCPServer):
            address_family = family
        address = "::1" if family == socket.AF_INET6 else "127.0.0.1"
        self.server = TCPServer((address, 0), EchoHandler)
        self.server.daemon_threads = True
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)

    @property
    def port(self):
        return self.server.server_address[1]

    def start(self):
        self.thread.start()

    def stop(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=2)


def request(port, method, path, body=None, headers=None):
    connection = http.client.HTTPConnection("127.0.0.1", port, timeout=2)
    connection.request(method, path, body=body, headers=headers or {})
    response = connection.getresponse()
    result = response.status, dict(response.getheaders()), response.read()
    connection.close()
    return result


def wait_for(callback, timeout=15):
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        try:
            value = callback()
            if value:
                return value
        except (ConnectionError, OSError, http.client.HTTPException) as error:
            last = error
        time.sleep(0.02)
    raise AssertionError(f"condition not met before timeout: {last}")


def cleanup_cgroup(root):
    remaining = set()
    for name in ("ptys", "socats", "user"):
        path = root / name / "cgroup.procs"
        if path.exists():
            remaining.update(path.read_text().split())
    if remaining:
        raise AssertionError(f"daemon left cgroup processes behind: {sorted(remaining)}")
    for name in ("ptys", "socats", "user"):
        path = root / name
        if path.exists():
            path.rmdir()
    root.rmdir()


def isolate_network():
    """Create a new network namespace before installing link-local fixtures."""
    import ctypes
    import fcntl
    import struct
    with open('/proc/self/status') as status:
        caps = dict(line.split(':', 1) for line in status if line.startswith('Cap'))
    for name in ('CapEff', 'CapBnd', 'CapInh', 'CapAmb'):
        assert not int(caps[name], 16) & (1 << 25), name + ' contains SYS_TIME'
    libc = ctypes.CDLL(None, use_errno=True)
    if libc.unshare(0x40020000) != 0:  # CLONE_NEWNET | CLONE_NEWNS
        raise OSError(ctypes.get_errno(), 'private network namespace required')
    subprocess.run(['mount', '--make-rprivate', '/'], check=True)
    with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as control:
        fcntl.ioctl(control, 0x8914, struct.pack('16sH22x', b'lo', 0x49))
        for name, address in [(b'lo', '127.0.0.1'), (b'lo:mmds', '169.254.169.254'), (b'lo:forward', '169.254.0.21')]:
            request = struct.pack('16sH2x4s8x', name, socket.AF_INET, socket.inet_aton(address))
            fcntl.ioctl(control, 0x8916, request)

class GuestDaemon:
    """Own a real daemon and a dedicated subtree of the private cgroup mount."""
    def __init__(self, firecracker=False, environment=None):
        import tempfile
        self.root = pathlib.Path(tempfile.mkdtemp(prefix='guest-', dir=CGROUP_MOUNT))
        (self.root / 'cgroup.subtree_control').write_text('+cpu +memory')
        self.port = free_port()
        self.output = tempfile.TemporaryFile()
        args = [BINARY, '-port', str(self.port), '-cgroup-root', str(self.root)]
        if not firecracker:
            args.append('-isnotfc')
        self.child = subprocess.Popen(args, stdout=self.output, stderr=self.output,
                                      env=environment)

    def __enter__(self):
        try:
            wait_for(lambda: request(self.port, 'GET', '/health')[0] == 204)
        except BaseException:
            self.__exit__(None, None, None)
            raise
        return self

    def logs(self):
        return os.pread(self.output.fileno(), 1024 * 1024, 0).decode()

    def stop(self, sig=signal.SIGTERM):
        if self.child.poll() is None:
            self.child.send_signal(sig)
        self.child.wait(timeout=3)
        assert self.child.returncode == 0, self.logs()

    def __exit__(self, *_args):
        try:
            self.stop()
            cleanup_cgroup(self.root)
        finally:
            self.output.close()

    def helpers(self):
        path = self.root / 'socats/cgroup.procs'
        return {int(pid) for pid in path.read_text().split()} if path.exists() else set()
