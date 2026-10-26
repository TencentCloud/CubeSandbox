# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# Demo 06: Clone N sandboxes from a single source using sb.clone(n=...).
#
# Internally, clone(n) does:
#   1. self.create_snapshot()
#   2. Sandbox.create(template=snapshot_id) × n
#   3. Sandbox.delete_snapshot(snapshot_id)  (best-effort cleanup)
#   4. return list[Sandbox] of length n
#
# After return, the source sandbox keeps running and the ephemeral snapshot
# has already been deleted — list_snapshots() will not show it.

from cubesandbox import Sandbox
from env import TEMPLATE_ID

N = 3

src = Sandbox.create(template=TEMPLATE_ID)
src.run_code("open('/tmp/shared.txt','w').write('shared state')")
print(f"src sandbox: {src.sandbox_id}")

# ★ One-line clone — SDK handles snapshot/create/delete internally
clones = src.clone(n=N)
print(f"cloned {len(clones)} sandboxes")

for i, sb in enumerate(clones):
    result = sb.run_code("print(open('/tmp/shared.txt').read())")
    content = result.logs.stdout[0].strip() if result.logs.stdout else ""
    print(f"  clone {i+1}: {sb.sandbox_id}  file={content!r}")
    assert content == "shared state"

print(f"OK: {N} sandboxes cloned via sb.clone(n={N}), all share initial state")

# Cleanup
src.kill()
for sb in clones:
    sb.kill()
print("all sandboxes killed")
