# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from cubesandbox import Sandbox

# List all running sandboxes
sandboxes = Sandbox.list()
print("total sandboxes (v1): %d" % len(sandboxes))
for sb in sandboxes:
    print("  sandbox_id=%s" % sb.get("sandboxID", ""))

sandboxes_v2 = Sandbox.list_v2()
print("total sandboxes (v2): %d" % len(sandboxes_v2))
for sb in sandboxes_v2:
    print("  sandbox_id=%s" % sb.get("sandboxID", ""))
