# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

import json
import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from cubesandbox import Sandbox

template_id = os.environ["CUBE_TEMPLATE_ID"]

with Sandbox.create(
    template=template_id,
    metadata={
        "host-mount": json.dumps([
            {
                "hostPath":  "/tmp/rw",
                "mountPath": "/mnt/rw",
                "readOnly":  False,
            },
            {
                "hostPath":  "/tmp/ro",
                "mountPath": "/mnt/ro",
                "readOnly":  True,
            },
        ])
    },
) as sandbox:
    info = sandbox.get_info()
    print("sandbox info %s" % info)
