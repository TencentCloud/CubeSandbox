# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# Demo 09: Rollback a sandbox to a previous snapshot.
#
# sb.rollback(snapshot_id) restores the sandbox in place — its memory and
# filesystem revert to the captured state. The sandbox keeps the same
# sandbox_id, so you can keep using the same `sb` object after rollback.
#
# Internally, rollback also resets the SDK's pooled HTTP connections, so
# the next run_code() on the same sandbox object Just Works without any
# manual reconnect.

from cubesandbox import Sandbox
from env import TEMPLATE_ID

# Step 1: create a base snapshot at v0
with Sandbox.create(template=TEMPLATE_ID) as src:
    src.run_code("open('/tmp/v.txt','w').write('v0')")
    base = src.create_snapshot()
    base_id = base.snapshot_id
    print(f"base snapshot (v0): {base_id}")

# Step 2: spin up a sandbox from the base snapshot
sb = Sandbox.create(template=base_id)
print(f"derived sandbox: {sb.sandbox_id}")

# Step 3: write v1, take a checkpoint
sb.run_code("open('/tmp/v.txt','w').write('v1')")
checkpoint = sb.create_snapshot()
checkpoint_id = checkpoint.snapshot_id
print(f"checkpoint (v1): {checkpoint_id}")

# Step 4: write v2, confirm it stuck
sb.run_code("open('/tmp/v.txt','w').write('v2')")
before = sb.run_code("print(open('/tmp/v.txt').read())").logs.stdout
before = before[0].strip() if before else ""
print(f"before rollback: {before!r}")
assert before == "v2"

# Step 5: rollback to the v1 checkpoint
sb.rollback(checkpoint_id)
print(f"rolled back to checkpoint {checkpoint_id}")

# Step 6: verify state is v1
after = sb.run_code("print(open('/tmp/v.txt').read())").logs.stdout
after = after[0].strip() if after else ""
print(f"after rollback:  {after!r}")
assert after == "v1", f"expected 'v1', got {after!r}"
print("OK: rollback restored state to checkpoint (v1)")

# Cleanup
sb.kill()
Sandbox.delete_snapshot(checkpoint_id)
Sandbox.delete_snapshot(base_id)
print("snapshots deleted")
