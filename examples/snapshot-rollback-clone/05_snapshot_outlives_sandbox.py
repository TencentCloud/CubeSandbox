# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# Demo 05: Snapshot lifecycle is independent of sandbox lifecycle.
# After killing the source sandbox, its snapshot is still usable.

from cubesandbox import Sandbox
from env import TEMPLATE_ID

sb = Sandbox.create(template=TEMPLATE_ID)
sandbox_id = sb.sandbox_id
snap = sb.create_snapshot()
snapshot_id = snap.snapshot_id
print(f"sandbox:  {sandbox_id}")
print(f"snapshot: {snapshot_id}")

sb.kill()
print(f"sandbox killed: {sandbox_id}")

# Snapshot must still be present
all_ids = []
items, token = Sandbox.list_snapshots()
while True:
    all_ids.extend(s.snapshot_id for s in items)
    if not token:
        break
    items, token = Sandbox.list_snapshots(next_token=token)

if snapshot_id in all_ids:
    print(f"OK: snapshot {snapshot_id} still exists after sandbox kill")
else:
    print(f"FAIL: snapshot {snapshot_id} disappeared after sandbox kill")

Sandbox.delete_snapshot(snapshot_id)
print("snapshot deleted")
