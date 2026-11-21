# SPDX-License-Identifier: Apache-2.0
"""Shared real FUSE download object for transfer and compose failure checks."""
import errno
import queue
import select
import stat
import struct
import threading
from support.filesystem import HeldLookupFilesystem

class DownloadFilesystem(HeldLookupFilesystem):
    def attributes(self, nodeid):
        mode = stat.S_IFDIR | 0o755 if nodeid == 1 else stat.S_IFREG | 0o644
        # Identity ServeContent uses the seek size, including on FUSE.
        return struct.pack('<6Q10I', nodeid, len(self.content) if self.content is not None else 65536, 0, 0, 0, 0,
                           0, 0, 0, mode, 2 if nodeid == 1 else 1, 0, 0, 0, 4096, 0)

    def __init__(self, root, content=None, hold_attributes=False):
        self.content = content
        self.hold_attributes = hold_attributes
        self.attributes_pending = queue.Queue()
        super().__init__(root)
        self.reads = queue.Queue()
        self.stopped = threading.Event()
        self.thread = threading.Thread(target=self.serve, daemon=True)
        self.thread.start()

    def until_read(self):
        return self.reads.get(timeout=5)

    def close(self):
        self.stopped.set()
        self.thread.join(timeout=2)
        super().close()

    def serve(self):
        while not self.stopped.is_set():
            if not select.select([self.fd], [], [], .1)[0]:
                continue
            opcode, unique, payload = self.receive()
            if opcode == 1:
                self.reply(unique, struct.pack('<4Q2I', 2, 0, 60, 0, 0, 0) + self.attributes(2))
            elif opcode == 3:
                if self.nodeid == 2 and self.hold_attributes:
                    self.hold_attributes = False
                    self.attributes_pending.put(unique)
                else:
                    self.getattr(unique)
            elif opcode == 14:  # OPEN: direct I/O gives exact read chunk boundaries.
                self.reply(unique, struct.pack('<QII', 1, 1, 0))
            elif opcode == 17:  # STATFS on the bound data FD.
                self.reply(unique, struct.pack('<5Q10I', *([0] * 5), 4096, 255, 4096, 0, *([0] * 6)))
            elif opcode == 15:
                offset, size = struct.unpack_from('<QQI', payload)[1:]
                if self.content is None:
                    self.reads.put((unique, (offset, size)))
                else:
                    self.reply(unique, self.content[offset:offset + size])
            elif opcode in (18, 25):  # RELEASE / FLUSH
                self.reply(unique)
            elif opcode in (2, 42):  # FORGET / BATCH_FORGET has no response.
                continue
            else:
                self.reply(unique, error=errno.ENOSYS)
