# SPDX-License-Identifier: Apache-2.0
import os
from pathlib import Path
import time

def resources(pid):
    root = Path('/proc') / str(pid)
    descriptors = list((root / 'fd').iterdir())
    watches = 0
    for fd in descriptors:
        try:
            watches += os.readlink(fd) == 'anon_inode:inotify'
        except FileNotFoundError:
            pass
    return watches, len(descriptors), len(list((root / 'task').iterdir()))


def wait_released(pid, before):
    deadline = time.monotonic() + 10
    while time.monotonic() < deadline:
        current = resources(pid)
        if current[0] == before[0] and current[1] <= before[1] + 3 and current[2] <= before[2] + 1:
            return current
        time.sleep(.02)  # Poll measured resource state; not an event-coverage barrier.
    raise AssertionError(('watch/FD/task leak', before, current))
