# SPDX-License-Identifier: Apache-2.0
"""Syscall injection and actual FUSE/slow-consumer cleanup through public RPC.

Run beside a fresh daemon with watch_faults.c preloaded in a private container.
Injected IN_Q_OVERFLOW/read EIO are not a claim of real kernel exhaustion.
"""
import concurrent.futures
import os
from pathlib import Path
import tempfile
import time
import struct
from support.filesystem import HeldLookupFilesystem, filesystem_rpc, health
from support.watch import resources, wait_released
from watch_stream import Watch as Stream
from watch_polling import Watch as Polling


def main():
    pid = int(os.environ['WATCH_DAEMON_PID'])
    before = resources(pid)
    with tempfile.TemporaryDirectory() as directory:
        root = Path(directory)
        fuse = root / 'fuse'
        fuse.mkdir()
        backend = HeldLookupFilesystem(fuse)
        try:
            # GETATTR is a genuine kernel request; no production test endpoint.
            with concurrent.futures.ThreadPoolExecutor() as pool:
                creation = pool.submit(filesystem_rpc, 'CreateWatcher', {'path': str(fuse)})
                while not creation.done():
                    import select
                    if select.select([backend.fd], [], [], .05)[0]:
                        opcode, unique, _ = backend.receive()
                        if opcode == 3:
                            backend.getattr(unique)
                        elif opcode == 17:  # FUSE_STATFS for mount-type validation.
                            backend.reply(unique, struct.pack('<5Q10I', *([0] * 5), 4096, 255, 4096, 0, *([0] * 6)))
                        else:
                            import errno
                            backend.reply(unique, error=errno.ENOSYS)
                status, body = creation.result()
                assert status == 400 and body['code'] == 'invalid_argument', (status, body)
            health()
            with concurrent.futures.ThreadPoolExecutor(max_workers=12) as pool:
                pending = [pool.submit(filesystem_rpc, 'Stat', str(fuse / f'job-{i}'))
                           for i in range(12)]
                try:
                    backend.collect(12)
                    health()
                    assert filesystem_rpc('Stat', str(root))[0] == 200
                finally:
                    backend.release()
                for operation in pending:
                    status, body = operation.result(timeout=5)
                    assert status == 404 and body['code'] == 'not_found', (status, body)
        finally:
            backend.close()
        for Watch in (Stream, Polling):
            for marker in ('.watch-enospc', '.watch-eacces', '.fault-kernel', '.fault-backend'):
                watched = root / (Watch.__name__ + marker)
                watched.mkdir()
                # Error markers belong to children, not the initial root path.
                watched.rename(root / 'watched')
                watched = root / 'watched'
                watch = Watch(watched)
                try:
                    watch.start()
                    (watched / 'prefix').write_bytes(b'p')
                    watch.event('prefix', 'CREATE')
                    watch.event('prefix', 'WRITE')
                    (watched / marker).mkdir()
                    watch.terminal('internal')
                    health()
                finally:
                    watch.close()
                wait_released(pid, before)
                (watched / marker).rmdir()
                (watched / 'prefix').unlink()
                watched.rmdir()
        # An unread TCP subscriber cannot stop health or independent operations.
        watched = root / 'slow'
        watched.mkdir()
        slow = Stream(watched, False)
        slow.start()
        try:
            for n in range(4096):
                (watched / (str(n) + 'x' * 180)).touch()
            health()
            status, body = filesystem_rpc('Stat', str(root))
            assert status == 200, body
            other = Stream(root, False)
            try:
                other.start()
                (root / 'independent').mkdir()
                other.event('independent', 'CREATE')
            finally:
                other.close()
        finally:
            slow.close()
        wait_released(pid, before)
        for _ in range(3):
            watchers = [Stream(root, False) for _ in range(16)]
            try:
                for watch in watchers:
                    watch.start()
            finally:
                for watch in watchers:
                    watch.close()
            wait_released(pid, before)
    print('FUSE rejection, backend faults, accepted prefix, slow consumer and cleanup passed')


if __name__ == '__main__':
    main()
