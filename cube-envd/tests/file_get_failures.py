# SPDX-License-Identifier: Apache-2.0
"""Real FUSE GET faults and cancellation in a disposable privileged container.

Requires /dev/fuse and CAP_SYS_ADMIN. Uses public HTTP and kernel FUSE messages;
no daemon hooks. Run next to the daemon in its mount namespace.
"""
import concurrent.futures
import errno
import http.client
import gzip
import json
import os
from pathlib import Path
import select
import queue
import socket
import stat
import struct
import tempfile
import time
from urllib.parse import urlencode, urlsplit
from urllib.request import Request
from urllib.error import HTTPError

from support.filesystem import HeldLookupFilesystem
from support.connect import BASE, HTTP


from support.download import DownloadFilesystem


def get(path, encoding='identity'):
    try:
        return HTTP.open(Request(BASE + '/files?' + urlencode({'path': str(path)}),
                                 headers={'Accept-Encoding': encoding}), timeout=10)
    except HTTPError as error:
        return error


def health():
    with HTTP.open(BASE + '/health', timeout=5) as response:
        assert response.status == 204


def failures(root, pool):
    for encoding in ('identity', 'gzip'):
        for phase in ('before', 'after'):
            mount = root / f'{encoding}-{phase}'
            mount.mkdir()
            fs = DownloadFilesystem(mount)
            payload = b'' if phase == 'before' else os.urandom(8192)
            def fetch():
                with get(mount / 'file', encoding) as response:
                    try:
                        body, incomplete = response.read(), False
                    except http.client.IncompleteRead as error:
                        body, incomplete = error.partial, True
                    return response.status, response.headers, body, incomplete
            try:
                pending = pool.submit(fetch)
                deadline = time.monotonic() + 10
                faults = 0
                while not pending.done():
                    assert time.monotonic() < deadline, (encoding, phase)
                    try:
                        unique, (offset, size) = fs.reads.get(timeout=.02)
                    except queue.Empty:
                        continue
                    if offset < len(payload):
                        fs.reply(unique, payload[offset:offset + size])
                    else:
                        faults += 1
                        fs.reply(unique, error=errno.EIO)
                status, headers, body, incomplete = pending.result()
                assert faults > 0
                assert status == 200, (encoding, phase, status)
                if encoding == 'gzip':
                    assert not incomplete
                    assert gzip.decompress(body) == payload
                else:
                    assert headers['Content-Length'] == '65536'
                    assert incomplete and body == payload
                health()
                print(encoding, phase, 'upstream read-fault response matched', flush=True)
            finally:
                fs.close()


def cancel_blocked_lookups(root):
    mount = root / 'cancel'
    mount.mkdir()
    fs = HeldLookupFilesystem(mount)
    peers = []
    try:
        url = urlsplit(BASE)
        for number in range(8):
            peer = socket.create_connection((url.hostname, url.port), timeout=5)
            peers.append(peer)
            path = '/files?' + urlencode({'path': str(mount / f'job-{number}')})
            peer.sendall(f'GET {path} HTTP/1.1\r\nHost: envd\r\n\r\n'.encode())
        fs.collect(8)
        with get(root / 'probe') as response:
            assert response.status == 200 and response.read()
        # Independent paths continue while these kernel lookups are blocked.
        assert not select.select([fs.fd], [], [], 0.1)[0]
        health()
        for peer in peers:
            peer.setsockopt(socket.SOL_SOCKET, socket.SO_LINGER, struct.pack('ii', 1, 0))
            peer.close()
        fs.release()
        deadline = time.monotonic() + 5
        while True:
            with get(root / 'probe') as response:
                if response.status == 200:
                    assert response.read()
                    break
            assert time.monotonic() < deadline, 'cancelled file slots leaked'
            time.sleep(.02)
        print('eight blocked lookups, independent progress and cancellation passed', flush=True)
    finally:
        for peer in peers:
            peer.close()
        fs.close()


def binding_before_open(root, pool):
    mount = root / 'binding'
    mount.mkdir()
    payload = b'original bound object'
    fs = DownloadFilesystem(mount, content=payload, hold_attributes=True)
    link = root / 'link'
    link.symlink_to(mount / 'file')
    try:
        pending = pool.submit(get, link)
        unique = fs.attributes_pending.get(timeout=5)
        # O_PATH already selected inode 2. Replace the public path before its
        # type check returns and before any data open can occur.
        link.unlink()
        link.symlink_to('/dev/zero')
        fs.reply(unique, struct.pack('<QII', 60, 0, 0) + fs.attributes(2))
        with pending.result(5) as response:
            assert response.status == 200
            assert response.read() == payload
        print('symlink retarget between O_PATH binding/type check and data open passed', flush=True)
    finally:
        fs.close()


def special_devices(root):
    for name, mode, device in [('block', stat.S_IFBLK, os.makedev(0, 0)),
                               ('char', stat.S_IFCHR, os.makedev(1, 5))]:
        path = root / name
        os.mknod(path, mode | 0o600, device)
        with get(path) as response:
            assert response.status == (500 if name == "block" else 200), (name, response.status)
            if name == "char":
                assert response.read() == b""
            else:
                assert json.load(response) == {
                    'code': 500,
                    'message': f"error opening file '{path}': open {path}: no such device or address",
                }
    print('device downloads follow kernel open and seek behavior', flush=True)


def cancelled_read(root, pool):
    mount = root / 'cancel-read'
    mount.mkdir()
    fs = DownloadFilesystem(mount)
    try:
        pending = pool.submit(get, mount / 'file')
        unique, _ = fs.until_read()
        fs.reply(unique, b'x' * 512)
        response = pending.result(5)
        assert response.status == 200
        unique, _ = fs.until_read()
        response.close()
        # Deliver a successful kernel read after the peer disconnects. The
        # canceled download must not begin another read from this object.
        time.sleep(.1)
        fs.reply(unique, b'y' * 512)
        try:
            fs.reads.get(timeout=.3)
        except queue.Empty:
            pass
        else:
            raise AssertionError('new read started after observed disconnect')
        with get(root / 'probe') as fresh:
            assert fresh.status == 200 and fresh.read()
        print('disconnect during a kernel read stops subsequent reads', flush=True)
    finally:
        fs.close()


def main():
    with tempfile.TemporaryDirectory(prefix='file-get-fuse-') as directory:
        root = Path(directory)
        (root / 'probe').write_bytes(b'ready')
        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
            failures(root, pool)
            binding_before_open(root, pool)
            cancelled_read(root, pool)
        cancel_blocked_lookups(root)
        special_devices(root)
    print('real FUSE GET failure and cancellation assertions passed', flush=True)


if __name__ == '__main__':
    main()
