# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# Demo 04: Verify filesystem state is preserved across snapshot + clone.
#
# Write a marker file in the source sandbox, take a snapshot, spin up a
# clone from that snapshot, and confirm the marker is visible.

from cubesandbox import Sandbox
from env import TEMPLATE_ID

MARKER = "hello from snapshot"

with Sandbox.create(template=TEMPLATE_ID) as src:
    src.run_code(f"open('/tmp/marker.txt','w').write('{MARKER}')")
    snapshot = src.create_snapshot()
    snapshot_id = snapshot.snapshot_id
    print(f"wrote marker, snapshot: {snapshot_id}")

with Sandbox.create(template=snapshot_id) as cloned:
    result = cloned.run_code("print(open('/tmp/marker.txt').read())")
    content = result.logs.stdout[0].strip() if result.logs.stdout else ""
    print(f"cloned sandbox read: {content!r}")
    assert content == MARKER, f"state not preserved: got {content!r}"
    print("OK: filesystem state preserved in cloned sandbox")

Sandbox.delete_snapshot(snapshot_id)
print("snapshot deleted")
