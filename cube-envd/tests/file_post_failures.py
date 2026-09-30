# SPDX-License-Identifier: Apache-2.0
"""Real kernel FUSE upload faults, binding races and partial writes.

Run in a disposable privileged mount namespace beside the actual daemon binary.
"""
import concurrent.futures
import errno
import json
import os
from pathlib import Path
import queue
import select
import socket
import stat
import struct
import subprocess
import tempfile
import threading
import time
from urllib.parse import urlencode, urlsplit

from support.filesystem import HeldLookupFilesystem
from support.files import post
from support.connect import BASE, HTTP


class UploadFilesystem(HeldLookupFilesystem):
    def __init__(self, root, failure=None, hold=None):
        self.data = bytearray(b'original contents')
        self.failure = failure
        self.hold = hold
        self.events = []
        self.pending_operation = queue.Queue()
        self.stopped = threading.Event()
        super().__init__(root)
        self.thread = threading.Thread(target=self.serve, daemon=True)
        self.thread.start()

    def attributes(self, nodeid):
        mode = stat.S_IFDIR | 0o755 if nodeid == 1 else stat.S_IFREG | 0o644
        return struct.pack('<6Q10I', nodeid, len(self.data), 0, 0, 0, 0,
                           0, 0, 0, mode, 2 if nodeid == 1 else 1, 0, 0, 0, 4096, 0)

    def perform(self, operation, unique, payload):
        self.events.append(operation)
        if self.hold == operation:
            self.hold = None
            self.pending_operation.put((operation, unique, payload))
            return
        if self.failure and self.failure[0] == operation:
            self.reply(unique, error=self.failure[1])
            return
        if operation == 'attributes':
            self.reply(unique, struct.pack('<QII', 0, 0, 0) + self.attributes(2))
        elif operation == 'open':
            self.reply(unique, struct.pack('<QII', 1, 1, 0))  # direct I/O
        elif operation in ('chown', 'truncate'):
            if operation == 'truncate':
                size = struct.unpack_from('<Q', payload, 16)[0]
                del self.data[size:]
            self.reply(unique, struct.pack('<QII', 0, 0, 0) + self.attributes(2))
        elif operation == 'write':
            offset, size = struct.unpack_from('<QI', payload, 8)
            content = payload[40:40 + size]
            if offset > len(self.data):
                self.data.extend(b'\0' * (offset - len(self.data)))
            self.data[offset:offset + size] = content
            self.reply(unique, struct.pack('<II', size, 0))

    def serve(self):
        while not self.stopped.is_set():
            if not select.select([self.fd], [], [], .1)[0]:
                continue
            opcode, unique, payload = self.receive()
            if opcode == 1:
                self.reply(unique, struct.pack('<4Q2I', 2, 0, 0, 0, 0, 0) + self.attributes(2))
            elif opcode == 3:
                if self.nodeid == 1:
                    self.getattr(unique)
                else:
                    self.perform('attributes', unique, payload)
            elif opcode == 4:
                valid = struct.unpack_from('<I', payload)[0]
                self.perform('truncate' if valid & 8 else 'chown', unique, payload)
            elif opcode == 14:
                self.perform('open', unique, payload)
            elif opcode == 16:
                self.perform('write', unique, payload)
            elif opcode == 15:
                offset, size = struct.unpack_from('<QI', payload, 8)
                self.reply(unique, self.data[offset:offset + size])
            elif opcode == 17:
                self.reply(unique, struct.pack('<5Q10I', *([0] * 5), 4096, 255, 4096, 0, *([0] * 6)))
            elif opcode in (18, 25):
                self.reply(unique)
            elif opcode not in (2, 42):
                self.reply(unique, error=errno.ENOSYS)

    def close(self):
        self.stopped.set()
        self.thread.join(timeout=2)
        super().close()




def get(path):
    with HTTP.open(BASE + '/files?' + urlencode({'path': str(path)}), timeout=5) as response:
        return response.read()


def main():
    with tempfile.TemporaryDirectory() as directory:
        root = Path(directory)
        mount = root / 'mount'
        mount.mkdir()
        held = HeldLookupFilesystem(mount)
        peers = []
        try:
            address = urlsplit(BASE)
            for index in range(8):
                peer = socket.create_connection((address.hostname, address.port), timeout=5)
                peers.append(peer)
                header = (f'POST /files?{urlencode({"path": str(mount / f"job-{index}")})} HTTP/1.1\r\n'
                          'Host: localhost\r\nContent-Type: application/octet-stream\r\n'
                          'Transfer-Encoding: chunked\r\n\r\n3\r\nabc\r\n')
                peer.sendall(header.encode())
            held.collect(8)
            assert post(root / 'ninth')[0] == 200
            with HTTP.open(BASE + '/health', timeout=5) as response:
                assert response.status == 204
            for peer in peers:
                peer.close()
            time.sleep(.1)
            held.release()
            time.sleep(.1)
            print('eight blocked POST lookups: independent upload, health and pre-mutation cancellation passed', flush=True)
        finally:
            for peer in peers:
                peer.close()
            held.close()
        for operation, error, status, unchanged in [
            ('open', errno.EACCES, 403, True), ('chown', errno.EPERM, 403, True),
            ('truncate', errno.EROFS, 403, True), ('write', errno.ENOSPC, 507, False),
            ('write', errno.EDQUOT, 507, False), ('write', errno.EIO, 500, False),
            ('open', errno.EMFILE, 503, True),
        ]:
            fs = UploadFilesystem(mount, failure=(operation, error))
            try:
                code, _ = post(mount / 'file')
                assert code == status, (operation, error, code, fs.events)
                assert bytes(fs.data) == (b'original contents' if unchanged else b'')
                if operation == 'chown':
                    assert 'truncate' not in fs.events and 'write' not in fs.events
                print('FUSE fault passed:', operation, error, status, fs.events, flush=True)
            finally:
                fs.close()
        # Hold the bound O_PATH object's metadata before its data open; retarget link.
        fs = UploadFilesystem(mount, hold='attributes')
        link = root / 'link'
        link.symlink_to(mount / 'file')
        try:
            with concurrent.futures.ThreadPoolExecutor() as pool:
                result = pool.submit(post, link)
                operation, unique, payload = fs.pending_operation.get(timeout=5)
                link.unlink()
                replacement = root / 'replacement'
                replacement.write_bytes(b'untouched')
                link.symlink_to(replacement)
                fs.perform(operation, unique, payload)
                assert result.result(timeout=10)[0] == 200
                assert fs.data == b'new payload' and replacement.read_bytes() == b'untouched'
                assert fs.events.index('chown') < fs.events.index('truncate') < fs.events.index('write')
                assert get(mount / 'file') == b'new payload'
                print('O_PATH binding before data open and chown-before-truncate passed', flush=True)
        finally:
            fs.close()
        # Actual first kernel write completes; a later EDQUOT leaves that exact prefix.
        fs = UploadFilesystem(mount, hold='write')
        try:
            with concurrent.futures.ThreadPoolExecutor() as pool:
                result = pool.submit(post, mount / 'file', b'x' * 100000)
                operation, unique, payload = fs.pending_operation.get(timeout=5)
                # Install the next-write fault before releasing this kernel write.
                offset, size = struct.unpack_from('<QI', payload, 8)
                fs.data[offset:offset + size] = payload[40:40 + size]
                fs.failure = ('write', errno.EDQUOT)
                fs.reply(unique, struct.pack('<II', size, 0))
                assert result.result(timeout=10)[0] == 507
                assert 0 < len(fs.data) < 100000 and set(fs.data) == {120}
                assert get(mount / 'file') == bytes(fs.data)
                print('partial kernel write / EDQUOT / GET reconciliation passed:', len(fs.data), flush=True)
        finally:
            fs.close()
        # A kernel-blocked chown can finish, but disconnect prevents later truncate/write.
        fs = UploadFilesystem(mount, hold='chown')
        try:
            address = urlsplit(BASE)
            sock = socket.create_connection((address.hostname, address.port), timeout=5)
            request = (f'POST /files?{urlencode({"path": str(mount / "file")})} HTTP/1.1\r\n'
                       'Host: localhost\r\nContent-Type: application/octet-stream\r\n'
                       'Transfer-Encoding: chunked\r\n\r\n3\r\nabc\r\n')
            sock.sendall(request.encode())
            operation, unique, payload = fs.pending_operation.get(timeout=5)
            sock.close()
            time.sleep(.1)
            fs.perform(operation, unique, payload)
            time.sleep(.1)
            assert 'truncate' not in fs.events and 'write' not in fs.events, fs.events
            assert fs.data == b'original contents'
            print('cancel during blocked chown stops truncate/write passed', flush=True)
        finally:
            fs.close()
        # Real tmpfs exhaustion, rather than opening a special device for fake ENOSPC.
        subprocess.run(['mount', '-t', 'tmpfs', '-o', 'size=1m', 'upload-space', str(mount)], check=True)
        try:
            assert post(mount / 'full', b'x' * (2 * 1024 * 1024))[0] == 507
            size = (mount / 'full').stat().st_size
            assert 0 < size <= 1024 * 1024
            print('actual tmpfs ENOSPC prefix passed:', size, flush=True)
        finally:
            subprocess.run(['umount', str(mount)], check=True)
        for name, kind, device in [('char', stat.S_IFCHR, os.makedev(1, 3)),
                                   ('block', stat.S_IFBLK, os.makedev(0, 0))]:
            path = root / name
            os.mknod(path, kind | 0o600, device)
            code, body = post(path)
            assert code == (200 if name == 'char' else 500)
            if name == 'block':
                assert json.loads(body) == {
                    'code': 500,
                    'message': f'error opening file: open {path}: no such device or address',
                }
        print('character device upload and unavailable block device passed', flush=True)
        fifo = root / 'fifo'
        os.mkfifo(fifo, 0o600)
        reader = os.open(fifo, os.O_RDONLY | os.O_NONBLOCK)
        try:
            assert post(fifo)[0] == 200
            assert os.read(reader, 4096) == b'new payload'
        finally:
            os.close(reader)
        print('FIFO upload reaches the existing reader', flush=True)


if __name__ == '__main__':
    main()
