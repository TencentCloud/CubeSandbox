# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

import os
import sys
import time

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from cubesandbox import Sandbox

template_id = os.environ["CUBE_TEMPLATE_ID"]

with Sandbox.create(template=template_id) as sandbox:
    sandbox.pause()
    time.sleep(3)
    sandbox.connect()
    info = sandbox.get_info()
    print("sandbox info %s" % info)
