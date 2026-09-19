# SPDX-License-Identifier: Apache-2.0
"""Measure real slow upload memory, admission and cleanup, plus inode metadata."""
import os
from pathlib import Path
import pwd
import socket
import tempfile
import time
import zlib
from urllib.parse import urlencode, urlsplit

from support.files import memory, status, post, resources
from support.connect import BASE


def main():
    pid = int(os.environ['ENVD_SMOKE_PID'])
    address = urlsplit(BASE)
    with tempfile.TemporaryDirectory() as directory:
        root = Path(directory)
        # Daemon is started with umask 077 by the runner. New inode attributes
        # remain deterministic and an existing inode keeps its ordinary mode/xattr.
        target = root / 'new/parents/file'
        assert post(target, query={'username': 'nobody'})[0] == 200
        user = pwd.getpwnam('nobody')
        for path, mode in [(target, 0o644), (target.parent, 0o755), (target.parent.parent, 0o755)]:
            info = path.stat()
            assert (stat_mode := info.st_mode & 0o7777) == mode, (path, stat_mode)
            assert (info.st_uid, info.st_gid) == (user.pw_uid, user.pw_gid)
        inode = target.stat().st_ino
        target.chmod(0o6751)
        os.setxattr(target, 'user.upload-test', b'preserve')
        assert post(target, query={'username': 'nobody'})[0] == 200
        assert target.stat().st_ino == inode and target.stat().st_mode & 0o777 == 0o751
        assert os.getxattr(target, 'user.upload-test') == b'preserve'
        print('umask 077: new 0644/0755 selected ownership; existing inode/mode/xattr passed', flush=True)
        # Warm allocator/socket state before comparing steady-state resources.
        assert status('/files?' + urlencode({'path': str(target)})) == 200
        time.sleep(.1)
        base_memory, base_resources = memory(pid), resources(pid)
        for encoding in ('identity', 'gzip'):
            for cycle in range(3):
                peers = []
                paths = []
                try:
                    for index in range(8):
                        path = root / f'{encoding}-{cycle}-{index}'
                        paths.append(path)
                        sock = socket.create_connection((address.hostname, address.port), timeout=5)
                        peers.append(sock)
                        content = b'x' * 65536
                        if encoding == 'gzip':
                            compressor = zlib.compressobj(wbits=31)
                            content = compressor.compress(content) + compressor.flush(zlib.Z_SYNC_FLUSH)
                        headers = (f'POST /files?{urlencode({"path": str(path)})} HTTP/1.1\r\n'
                                   'Host: localhost\r\nContent-Type: application/octet-stream\r\n'
                                   f'Content-Encoding: {encoding}\r\nTransfer-Encoding: chunked\r\n\r\n')
                        sock.sendall(headers.encode() + f'{len(content):x}\r\n'.encode() + content + b'\r\n')
                        deadline = time.monotonic() + 5
                        while not path.exists() or path.stat().st_size != 65536:
                            assert time.monotonic() < deadline
                            time.sleep(.01)
                    assert status('/files?' + urlencode({'path': str(target)})) == 200
                    assert status('/health') == 204
                    current = memory(pid)
                    assert current - base_memory < 64 * 1024, (base_memory, current)
                    print(encoding, cycle, 'RSS KiB', base_memory, current, 'resources', resources(pid), flush=True)
                finally:
                    for peer in peers:
                        peer.close()
                deadline = time.monotonic() + 5
                while resources(pid)[1] > base_resources[1] or status('/files?' + urlencode({'path': str(target)})) != 200:
                    assert time.monotonic() < deadline, (base_resources, resources(pid))
                    time.sleep(.02)
                time.sleep(.1)
                after = resources(pid)
                assert after[0] <= base_resources[0] + 1 and after[1] <= base_resources[1], (base_resources, after)
                assert all(path.read_bytes() == b'x' * 65536 for path in paths)
        print('six cycles of eight stalled uploads: bounded RSS, health, prefixes, FD/thread cleanup passed', flush=True)


if __name__ == '__main__':
    main()
