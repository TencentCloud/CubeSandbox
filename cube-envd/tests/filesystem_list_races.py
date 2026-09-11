# SPDX-License-Identifier: Apache-2.0
"""Real mounted-FUSE ListDir races; run beside a disposable privileged daemon.

Use ENVD_SMOKE_URL and the same isolated mount namespace as filesystem_pressure.
All fixture mutations stay below a new temporary directory.
"""
import concurrent.futures
import errno
from pathlib import Path
import select
import struct
import tempfile
import time

from support.filesystem import (
    HeldLookupFilesystem, filesystem_rpc, finish_metadata, health,
)


class DisappearingDirectory(HeldLookupFilesystem):
    """List names, then return an actual FUSE lookup failure for one child."""
    def __init__(self, root, lookup_error):
        self.lookup_error = lookup_error
        self.listed = False
        self.failed_lookup = False
        super().__init__(root)

    @staticmethod
    def dirent(nodeid, offset, name):
        name = name.encode()
        data = struct.pack('<QQII', nodeid, offset, len(name), 8) + name
        return data + b'\0' * (-len(data) % 8)

    def serve(self, future):
        limit = time.monotonic() + 5
        while not future.done() and time.monotonic() < limit:
            if not select.select([self.fd], [], [], .02)[0]:
                continue
            opcode, unique, payload = self.receive()
            if opcode == 3:  # GETATTR
                self.getattr(unique)
            elif opcode == 52:  # STATX: ask kernel to fall back to GETATTR.
                self.reply(unique, error=errno.ENOSYS)
            elif opcode == 27:  # OPENDIR
                self.reply(unique, struct.pack('<QII', 1, 0, 0))
            elif opcode == 28:  # READDIR
                offset = struct.unpack_from('<Q', payload, 8)[0]
                if offset == 0:
                    self.listed = True
                    self.reply(unique, self.dirent(2, 1, 'a-kept')
                               + self.dirent(3, 2, 'b-disappeared'))
                else:
                    self.reply(unique)
            elif opcode == 29:  # RELEASEDIR
                self.reply(unique)
            elif opcode == 1:  # LOOKUP
                name = payload.rstrip(b'\0')
                assert self.listed, 'child lookup must follow actual directory enumeration'
                if name == b'b-disappeared':
                    self.failed_lookup = True
                    self.reply(unique, error=self.lookup_error)
                else:
                    assert name == b'a-kept', name
                    self.reply(unique, struct.pack('<4Q2I', 2, 0, 60, 60, 0, 0)
                               + self.attributes(2))
            elif opcode in (2, 42):  # FORGET/BATCH_FORGET have no reply.
                continue
            else:
                raise AssertionError(f'unexpected FUSE operation {opcode}')
        result = future.result(timeout=.1)
        assert self.listed and self.failed_lookup
        return result


def disappearing_entry(root, pool):
    for number, expected in ((errno.ENOENT, None), (errno.EIO, 'internal')):
        mount = root / f'disappear-{number}'
        mount.mkdir()
        fixture = DisappearingDirectory(mount, number)
        try:
            future = pool.submit(filesystem_rpc, 'ListDir', mount)
            status, body = fixture.serve(future)
            if expected is None:
                assert status == 200, (status, body)
                assert [entry['name'] for entry in body['entries']] == ['a-kept'], body
            else:
                assert status != 200 and body['code'] == expected, (status, body)
                assert 'entries' not in body, 'backend failure returned partial successful list'
        finally:
            fixture.close()
        print(f'actual FUSE child lookup errno={number} after READDIR: '
              f'{"disappearance skipped" if expected is None else "whole RPC failed Internal"}',
              flush=True)


def bound_parent_survives_replacement(root, pool):
    mount = root / 'held-target'
    mount.mkdir()
    fixture = HeldLookupFilesystem(mount)
    try:
        original = root / 'original'
        original.mkdir()
        (original / 'a-held').symlink_to(mount / 'job-parent-rename')
        (original / 'z-original').write_text('original directory')
        future = pool.submit(filesystem_rpc, 'ListDir', original)
        fixture.collect(1)  # List has enumerated/opened original and reached first target.
        renamed = root / 'renamed'
        original.rename(renamed)
        original.mkdir()
        (original / 'replacement-only').write_text('replacement directory')
        fixture.return_file()
        status, body = finish_metadata(fixture, future)
        assert status == 200, (status, body)
        assert [entry['name'] for entry in body['entries']] == ['a-held', 'z-original'], body
        assert all(entry['path'].startswith(str(original) + '/') for entry in body['entries'])
        assert (renamed / 'z-original').read_text() == 'original directory'
        status, current = filesystem_rpc('ListDir', original)
        assert status == 200, (status, current)
        assert [entry['name'] for entry in current['entries']] == ['replacement-only'], current
        print('held ListDir resumed on opened original after parent rename/replacement; '
              'next request saw replacement', flush=True)
    finally:
        fixture.close()


def main():
    with tempfile.TemporaryDirectory(prefix='envd-list-races-') as directory:
        with concurrent.futures.ThreadPoolExecutor(max_workers=1) as pool:
            root = Path(directory)
            disappearing_entry(root, pool)
            bound_parent_survives_replacement(root, pool)
    health()
    print('ListDir disappearance, backend failure, bound-parent races and final health passed',
          flush=True)


if __name__ == '__main__':
    main()
