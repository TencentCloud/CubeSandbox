# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
"""
rollback_demo.py — verify that rollback restores both disk and in-memory state.

Steps:
  1. Create a base snapshot (v0)
  2. Spin up a sandbox from the base snapshot
  3. Write v1 to both /tmp (disk) and /dev/shm (tmpfs/RAM), take a checkpoint
  4. Write v2 to both, confirm it stuck
  5. Rollback to the v1 checkpoint
  6. Verify both /tmp and /dev/shm are back to v1
"""

from cubesandbox import Sandbox
from env import TEMPLATE_ID

# Step 1: create a base snapshot
with Sandbox.create(template=TEMPLATE_ID) as src:
    src.run_code("open('/tmp/v.txt','w').write('v0'); open('/dev/shm/v.txt','w').write('v0')")
    base = src.create_snapshot()
    base_id = base.snapshot_id
    print(f"base snapshot (v0): {base_id}")

# Step 2: spin up a sandbox from the base snapshot
with Sandbox.create(template=base_id) as sb:
    print(f"derived sandbox: {sb.sandbox_id}")

    # Step 3: write v1 to both disk and tmpfs, take a checkpoint
    sb.run_code("open('/tmp/v.txt','w').write('v1'); open('/dev/shm/v.txt','w').write('v1')")
    checkpoint = sb.create_snapshot()
    checkpoint_id = checkpoint.snapshot_id
    print(f"checkpoint (v1): {checkpoint_id}")

    # Step 4: write v2, verify it stuck on both
    sb.run_code("open('/tmp/v.txt','w').write('v2'); open('/dev/shm/v.txt','w').write('v2')")
    r = sb.run_code("print(open('/tmp/v.txt').read(), open('/dev/shm/v.txt').read())")
    before = r.logs.stdout[0].strip() if r.logs.stdout else ""
    print(f"before rollback: {before!r}")
    assert before == "v2 v2"

    # Step 5: rollback to the v1 checkpoint
    sb.rollback(checkpoint_id)
    print(f"rolled back to: {checkpoint_id}")

    # Step 6: verify both disk and tmpfs restored to v1
    r = sb.run_code("print(open('/tmp/v.txt').read(), open('/dev/shm/v.txt').read())")
    after = r.logs.stdout[0].strip() if r.logs.stdout else ""
    print(f"after rollback:  {after!r}")
    assert after == "v1 v1", f"expected 'v1 v1', got {after!r}"
    print("OK: both disk and memory rolled back to v1")

# Cleanup snapshots
Sandbox.delete_snapshot(checkpoint_id)
Sandbox.delete_snapshot(base_id)
print("snapshots deleted")
