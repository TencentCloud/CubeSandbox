# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""
clone_demo.py — verify that clone inherits both in-memory and on-disk state.

Writes a marker to /dev/shm (RAM/tmpfs) and /tmp (disk) in the source sandbox,
then clones N copies and asserts each clone can read both files intact.
"""

import os
from cubesandbox import Sandbox
from env import TEMPLATE_ID

N = int(os.environ.get("FORK_N", "3"))

src = Sandbox.create(template=TEMPLATE_ID)

# write to tmpfs (RAM only, no disk flush)
src.run_code("open('/dev/shm/marker', 'w').write('hello from cube')")
# write to disk
src.run_code("open('/tmp/marker', 'w').write('hello from cube')")
print(f"src: {src.sandbox_id}")

clones = src.clone(n=N, concurrency=N)

# verify every clone inherited both in-memory and on-disk content
ok = 0
for i, sb in enumerate(clones):
    r = sb.run_code("""
mem = open('/dev/shm/marker').read()
disk = open('/tmp/marker').read()
assert mem == 'hello from cube', f'mem got {mem!r}'
assert disk == 'hello from cube', f'disk got {disk!r}'
print(f'mem={mem!r} disk={disk!r}')
""")
    val = r.logs.stdout[0].strip() if r.logs.stdout else ""
    ok += 1
    print(f"clone[{i}]: {val}  OK")

print(f"\n{ok}/{N} clones verified")

src.kill()
for sb in clones:
    sb.kill()
