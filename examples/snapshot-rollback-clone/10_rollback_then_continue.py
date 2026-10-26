# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# Demo 10: Rollback and continue — write new state on the rolled-back branch,
#           then take another snapshot from that branch.
#
# Timeline:
#   v1 (checkpoint) -> v2 -> rollback to v1 -> write v3 -> snapshot

from cubesandbox import Sandbox
from env import TEMPLATE_ID

sb = Sandbox.create(template=TEMPLATE_ID)

# Write v1 and take a checkpoint
sb.run_code("open('/tmp/v.txt','w').write('v1')")
checkpoint = sb.create_snapshot()
print(f"checkpoint (v1): {checkpoint.snapshot_id}")

# Advance to v2, then roll back to v1
sb.run_code("open('/tmp/v.txt','w').write('v2')")
sb.rollback(checkpoint.snapshot_id)
got = sb.run_code("print(open('/tmp/v.txt').read())").logs.stdout
got = got[0].strip() if got else ""
print(f"after rollback: {got!r}")
assert got == "v1"

# Continue on the rolled-back branch: write v3
sb.run_code("open('/tmp/v.txt','w').write('v3')")
got = sb.run_code("print(open('/tmp/v.txt').read())").logs.stdout
got = got[0].strip() if got else ""
print(f"after writing v3: {got!r}")
assert got == "v3"

# Take a new snapshot from this branch and verify it via a clone
new_snap = sb.create_snapshot()
print(f"new snapshot: {new_snap.snapshot_id}")

with Sandbox.create(template=new_snap.snapshot_id) as forked:
    got = forked.run_code("print(open('/tmp/v.txt').read())").logs.stdout
    got = got[0].strip() if got else ""
    print(f"clone of new snapshot reads: {got!r}")
    assert got == "v3"

print("OK: rollback + continue + re-snapshot all consistent")

# Cleanup
sb.kill()
for sid in [checkpoint.snapshot_id, new_snap.snapshot_id]:
    try:
        Sandbox.delete_snapshot(sid)
    except Exception as e:
        print(f"  warn: delete {sid}: {e}")
print("cleanup done")
