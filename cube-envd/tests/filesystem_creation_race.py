# SPDX-License-Identifier: Apache-2.0
"""Directory replacement before first binding does not receive metadata changes.

The server creates using actual FUSE request credentials and mode, then replaces
the pathname before the next lookup can bind it.
Run in the same isolated privileged mount namespace as the disposable daemon.
"""
import concurrent.futures
import errno
import os
from pathlib import Path
import select
import stat
import struct
import tempfile
import time

from support.filesystem import HeldLookupFilesystem, filesystem_rpc
from support.connect import request


class CreationFilesystem(HeldLookupFilesystem):
    def __init__(self, mount, backing):
        self.backing = backing
        self.objects = {1: backing}
        self.created = False
        self.replaced = False
        self.metadata_nodes = []
        super().__init__(mount)

    def receive(self):
        assert select.select([self.fd], [], [], 5)[0], 'no FUSE syscall within 5 s'
        data = os.read(self.fd, 131072)
        _, opcode, unique, self.nodeid, self.request_uid, self.request_gid = struct.unpack_from('<IIQQII', data)
        return opcode, unique, data[40:]

    def attributes(self, nodeid):
        value = self.objects[nodeid].stat()
        return struct.pack('<6Q10I', nodeid, value.st_size, value.st_blocks,
                           int(value.st_atime), int(value.st_mtime), int(value.st_ctime),
                           0, 0, 0, value.st_mode, value.st_nlink,
                           value.st_uid, value.st_gid, 0, 4096, 0)

    def entry(self, unique, nodeid):
        self.reply(unique, struct.pack('<4Q2I', nodeid, 0, 0, 0, 0, 0)
                   + self.attributes(nodeid))

    def serve(self, opcode, unique, payload):
        if opcode == 1:  # LOOKUP
            assert payload.rstrip(b'\0') == b'created', payload
            if self.created:
                if not self.replaced:
                    original = self.backing / 'original'
                    self.objects[3].rename(original)
                    self.objects[3] = original
                    replacement = self.backing / 'created'
                    replacement.mkdir(mode=0o711)
                    (replacement / 'sentinel').write_text('external replacement')
                    self.objects[4] = replacement
                    self.replaced = True
                self.entry(unique, 4)
            else:
                self.reply(unique, error=errno.ENOENT)
        elif opcode == 9:  # MKDIR: create a real backing directory.
            assert self.nodeid == 1 and payload[8:].rstrip(b'\0') == b'created'
            assert not self.created
            target = self.backing / 'created'
            mode, umask = struct.unpack_from('<II', payload)
            target.mkdir(mode=stat.S_IMODE(mode) & ~umask)
            os.chown(target, self.request_uid, self.request_gid)
            self.objects[3] = target
            self.created = True
            self.entry(unique, 3)
        elif opcode == 4:  # SETATTR proves which bound inode receives metadata.
            self.metadata_nodes.append(self.nodeid)
            valid = struct.unpack_from('<I', payload)[0]
            mode, uid, gid = (struct.unpack_from('<I', payload, offset)[0]
                              for offset in (68, 76, 80))
            target = self.objects[self.nodeid]
            if valid & 6:  # FATTR_UID / FATTR_GID
                os.chown(target, uid if valid & 2 else -1, gid if valid & 4 else -1)
            if valid & 1:  # FATTR_MODE
                os.chmod(target, stat.S_IMODE(mode))
            self.getattr(unique)
        elif opcode == 3:
            self.getattr(unique)
        elif opcode == 52:
            self.reply(unique, error=errno.ENOSYS)
        elif opcode in (2, 42):  # FORGET and BATCH_FORGET have no reply.
            pass
        else:
            raise AssertionError(('unexpected FUSE opcode', opcode, self.nodeid))


def main():
    with tempfile.TemporaryDirectory(prefix='envd-creation-race-') as directory:
        root = Path(directory)
        mount = root / 'fuse'
        backing = root / 'backing'
        mount.mkdir()
        backing.mkdir()
        request('/init', {'defaultUser': 'nobody'})
        fixture = CreationFilesystem(mount, backing)
        pool = concurrent.futures.ThreadPoolExecutor(max_workers=1)
        try:
            operation = pool.submit(filesystem_rpc, 'MakeDir', mount / 'created')
            deadline = time.monotonic() + 8
            while not operation.done() and time.monotonic() < deadline:
                if select.select([fixture.fd], [], [], .05)[0]:
                    fixture.serve(*fixture.receive())
            status, body = operation.result(timeout=.1)
            assert fixture.replaced, 'no replacement before the binding lookup'
            original = (backing / 'original').stat()
            replacement = (backing / 'created').stat()
            assert (stat.S_IMODE(replacement.st_mode), replacement.st_uid, replacement.st_gid) == (0o711, 0, 0), ('replacement metadata modified', oct(replacement.st_mode), replacement.st_uid, replacement.st_gid, fixture.metadata_nodes)
            assert (backing / 'created' / 'sentinel').read_text() == 'external replacement'
            assert (stat.S_IMODE(original.st_mode), original.st_uid, original.st_gid) == (0o755, 65534, 65534), ('creation credentials/mode', original)
            assert status != 200 and body.get('code') == 'failed_precondition', (status, body)
            assert fixture.metadata_nodes == [], fixture.metadata_nodes
            print('MakeDir: actual FUSE creation credentials produced 0755/65534:65534; '
                  'replacement before first binding stayed 0711/0:0 with sentinel; '
                  'FailedPrecondition without SETATTR', flush=True)
        finally:
            fixture.close()
            pool.shutdown(wait=True, cancel_futures=True)


if __name__ == '__main__':
    main()
