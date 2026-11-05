# SPDX-License-Identifier: Apache-2.0
"""Real FUSE syscall pressure against a disposable daemon in this mount namespace.

Run as root with /dev/fuse and CAP_SYS_ADMIN (isolated privileged container).
ENVD_SMOKE_URL selects the daemon. No libfuse, production hooks, or mock queue.
This runner validates daemon control progress; it does not validate a supervisor.
"""
import ctypes
import errno
import json
import os
import select
import stat
import struct
import time
from urllib.error import HTTPError
from urllib.request import Request

from support.connect import BASE, HTTP


class HeldLookupFilesystem:
    """Only root attributes and deliberately pending child LOOKUPs are needed."""
    def __init__(self, root):
        self.root = root
        self.libc = ctypes.CDLL(None, use_errno=True)
        self.fd = os.open('/dev/fuse', os.O_RDWR)
        self.pending = []
        options = f'fd={self.fd},rootmode=40000,user_id=0,group_id=0'.encode()
        if self.libc.mount(b'envd-pressure', os.fsencode(root), b'fuse', 0, options):
            os.close(self.fd)
            raise OSError(ctypes.get_errno(), 'mount FUSE fixture')
        try:
            opcode, unique, _ = self.receive()
            assert opcode == 26, opcode  # FUSE_INIT
            # FUSE_PARALLEL_DIROPS permits distinct child lookups concurrently.
            payload = struct.pack('<IIIIHHIIHH8I', 7, 31, 0, 1 << 18, 16, 12,
                                  65536, 1, 0, 0, *([0] * 8))
            self.reply(unique, payload)
        except BaseException:
            self.close()
            raise

    def receive(self):
        assert select.select([self.fd], [], [], 5)[0], 'no FUSE syscall within 5 s'
        # Linux requires space for negotiated max_write plus protocol overhead.
        data = os.read(self.fd, 131072)
        _, opcode, unique, self.nodeid = struct.unpack_from('<IIQQ', data)
        return opcode, unique, data[40:]

    def reply(self, unique, payload=b'', error=0):
        os.write(self.fd, struct.pack('<IiQ', 16 + len(payload), -error, unique) + payload)

    def collect(self, count):
        names = set()
        while len(self.pending) < count:
            opcode, unique, payload = self.receive()
            if opcode == 1:  # FUSE_LOOKUP: distinct child names avoid lookup coalescing.
                name = payload.rstrip(b'\0')
                assert name.startswith(b'job-') and name not in names, name
                names.add(name)
                self.pending.append(unique)
            elif opcode == 3:  # Never block shared-root path traversal.
                self.getattr(unique)
            elif opcode == 52:  # Optional FUSE_STATX: use the GETATTR fallback.
                self.reply(unique, error=errno.ENOSYS)
            else:
                raise AssertionError(f'unexpected FUSE operation {opcode}')

    @staticmethod
    def attributes(nodeid):
        mode = stat.S_IFDIR | 0o755 if nodeid in (1, 3) else stat.S_IFREG | 0o644
        return struct.pack('<6Q10I', nodeid, 0, 0, 0, 0, 0,
                           0, 0, 0, mode, 2 if nodeid == 1 else 1, 0, 0, 0, 4096, 0)

    def getattr(self, unique):
        self.reply(unique, struct.pack('<QII', 60, 0, 0) + self.attributes(self.nodeid))

    def return_file(self, nodeid=2):
        assert len(self.pending) == 1
        self.reply(self.pending.pop(), struct.pack('<4Q2I', nodeid, 0, 60, 60, 0, 0)
                   + self.attributes(nodeid))

    def release(self):
        for unique in self.pending:
            self.reply(unique, error=errno.ENOENT)
        self.pending.clear()

    def close(self):
        # Closing the connection also wakes outstanding calls if an assertion failed.
        self.libc.umount2(os.fsencode(self.root), 2)  # MNT_DETACH, private fixture only.
        os.close(self.fd)


def filesystem_rpc(method, path, deadline=None):
    headers = {'Content-Type': 'application/json', 'Connect-Protocol-Version': '1'}
    if deadline is not None:
        headers['Connect-Timeout-Ms'] = str(deadline)
    req = Request(BASE + '/filesystem.Filesystem/' + method,
                  json.dumps(path if isinstance(path, dict) else {'path': str(path)}).encode(), headers)
    try:
        with HTTP.open(req, timeout=10) as response:
            return response.status, json.loads(response.read())
    except HTTPError as error:
        return error.code, json.loads(error.read())


def filesystem_stat(path, deadline=None):
    return filesystem_rpc('Stat', path, deadline)


def health():
    with HTTP.open(BASE + '/health', timeout=2) as response:
        assert response.status == 204


def finish_metadata(fixture, future):
    limit = time.monotonic() + 3
    while not future.done() and time.monotonic() < limit:
        if select.select([fixture.fd], [], [], .05)[0]:
            opcode, unique, _ = fixture.receive()
            if opcode == 52:  # FUSE_STATX is optional; request GETATTR fallback.
                fixture.reply(unique, error=errno.ENOSYS)
            else:
                assert opcode == 3, opcode
                fixture.getattr(unique)
    return future.result(timeout=.1)
