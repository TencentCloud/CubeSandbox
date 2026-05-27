# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# Demo 11: Delete a snapshot and verify it is removed from the list.

from cubesandbox import Sandbox
from env import TEMPLATE_ID

sb = Sandbox.create(template=TEMPLATE_ID)
snap = sb.create_snapshot()
snapshot_id = snap.snapshot_id
sb.kill()
print(f"snapshot ready: {snapshot_id}")

Sandbox.delete_snapshot(snapshot_id)
print(f"delete_snapshot({snapshot_id}) -> ok")

# Verify it no longer appears in list_snapshots()
all_ids = []
items, token = Sandbox.list_snapshots()
while True:
    all_ids.extend(s.snapshot_id for s in items)
    if not token:
        break
    items, token = Sandbox.list_snapshots(next_token=token)

assert snapshot_id not in all_ids
print(f"OK: snapshot {snapshot_id} removed from list")
