# SPDX-License-Identifier: Apache-2.0
"""Real recursive Remove replacement race against a disposable same-namespace daemon.

ENVD_SMOKE_URL selects the daemon. Uses only public RPC and ordinary files;
renames only its own temporary fixtures after observing actual deletion progress.
"""
import concurrent.futures
import errno
import os
import select
from pathlib import Path
import tempfile
import struct
import time

from support.filesystem import filesystem_rpc as rpc
from support.filesystem import HeldLookupFilesystem, filesystem_rpc, finish_metadata
from support.connect import request


def replacement_during_remove(root):
    original = root / 'original'
    moved = root / 'moved-original'
    original.mkdir()
    for number in range(8000):
        (original / f'f{number:04d}').touch()
    # The replacement already exists, keeping the pathname swap short.
    replacement = root / 'replacement'
    replacement.mkdir()
    sentinel = replacement / 'sentinel'
    sentinel.write_text('replacement must survive')
    sentinel_inode = sentinel.stat().st_ino
    with concurrent.futures.ThreadPoolExecutor(max_workers=1) as pool:
        operation = pool.submit(rpc, 'Remove', {'path': str(original)})
        deadline = time.monotonic() + 5
        first = original / 'f0000'
        while first.exists() and not operation.done() and time.monotonic() < deadline:
            time.sleep(.0005)
        assert not first.exists(), 'no actual deletion observed before race'
        assert not operation.done(), 'Remove completed before replacement could be installed'
        original.rename(moved)
        replacement.rename(original)
        status, body = operation.result(timeout=10)
        assert status != 200 and body.get('code') == 'failed_precondition', (status, body)
        assert (original / 'sentinel').read_text() == 'replacement must survive'
        assert (original / 'sentinel').stat().st_ino == sentinel_inode
        assert set(os.listdir(original)) == {'sentinel'}, 'replacement subtree was traversed'
        assert moved.is_dir() and not any(moved.iterdir()), 'bound original recursion did not finish'
        print('Remove traversed bound original after rename, refused replacement name, '
              'and preserved replacement sentinel', flush=True)



def retargeted_move_parent(root):
    mount = root / 'fuse'
    mount.mkdir()
    backing = root / 'backing'
    backing.mkdir()
    source = backing / 'job-source'
    source.touch()
    inode = source.stat().st_ino
    destination = backing / 'job-destination'
    replacement = root / 'different-parent'
    replacement.mkdir()
    (replacement / source.name).write_text('replacement must survive')
    parent_link = root / 'source-parent'
    parent_link.symlink_to(mount, target_is_directory=True)
    fixture = HeldLookupFilesystem(mount)
    pool = concurrent.futures.ThreadPoolExecutor(max_workers=1)
    try:
        operation = pool.submit(filesystem_rpc, 'Move', {
            'source': str(parent_link / source.name),
            'destination': str(mount / destination.name)})
        fixture.collect(1)
        parent_link.unlink()
        parent_link.symlink_to(replacement, target_is_directory=True)
        fixture.return_file(nodeid=4)
        while True:
            opcode, unique, payload = fixture.receive()
            if opcode == 1:
                assert payload.rstrip(b'\0') == destination.name.encode(), payload
                fixture.reply(unique, error=errno.ENOENT)
            elif opcode == 3:
                fixture.getattr(unique)
            elif opcode == 52:
                fixture.reply(unique, error=errno.ENOSYS)
            else:
                assert opcode == 12 and fixture.nodeid == 1, (opcode, fixture.nodeid)
                assert struct.unpack_from('<Q', payload)[0] == 1
                assert payload[8:].split(b'\0') == [source.name.encode(), destination.name.encode(), b'']
                break
        os.rename(source, destination)
        fixture.reply(unique)
        status, body = finish_metadata(fixture, operation)
        assert status == 200 and body['entry']['path'] == str(mount / destination.name), (status, body)
        assert destination.stat().st_ino == inode and not source.exists()
        assert (replacement / source.name).read_text() == 'replacement must survive'
        print('Move retained opened source-parent identity after symlink retarget; '
              'replacement source survived', flush=True)
    finally:
        fixture.close()
        pool.shutdown(wait=True, cancel_futures=True)



def replaced_move_source(root):
    source = root / 'replace-source'
    old = root / 'saved-original-source'
    source.write_text('original')
    mount = root / 'replacement-fuse'
    mount.mkdir()
    fixture = HeldLookupFilesystem(mount)
    pool = concurrent.futures.ThreadPoolExecutor(max_workers=1)
    try:
        operation = pool.submit(filesystem_rpc, 'Move', {
            'source': str(source),
            'destination': str(mount / 'job-destination-parent' / 'target')})
        fixture.collect(1)  # Source is already bound; destination parent is in kernel.
        source.rename(old)
        source.write_text('replacement')
        fixture.return_file(nodeid=3)
        limit = time.monotonic() + 3
        while not operation.done() and time.monotonic() < limit:
            if select.select([fixture.fd], [], [], .05)[0]:
                opcode, unique, payload = fixture.receive()
                if opcode == 1:
                    assert payload.rstrip(b'\0') == b'target', payload
                    fixture.reply(unique, error=errno.ENOENT)
                elif opcode == 3:
                    fixture.getattr(unique)
                elif opcode == 52:
                    fixture.reply(unique, error=errno.ENOSYS)
                else:
                    raise AssertionError(f'unexpected FUSE operation {opcode}')
        status, body = operation.result(timeout=.1)
        assert source.read_text() == 'replacement' and old.read_text() == 'original'
        assert status != 200 and body.get('code') == 'failed_precondition', (status, body)
        print('Move rejected already-visible source replacement before rename; '
              'original and replacement source survived', flush=True)
    finally:
        fixture.close()
        pool.shutdown(wait=True, cancel_futures=True)


def main():
    with tempfile.TemporaryDirectory(prefix='envd-mutation-race-') as directory:
        root = Path(directory)
        request('/init', {'defaultUser': 'root', 'defaultWorkdir': str(root)})
        replacement_during_remove(root)
        retargeted_move_parent(root)
        replaced_move_source(root)


if __name__ == '__main__':
    main()
