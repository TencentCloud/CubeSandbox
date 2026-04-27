# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

import os
import json
from e2b_code_interpreter import Sandbox

# os.environ["E2B_API_KEY"] = "dummy"
# os.environ["E2B_API_URL"] = "http://localhost:3000"
# os.environ["SSL_CERT_FILE"] = "/root/.local/share/mkcert/rootCA.pem"

template_id = os.environ["CUBE_TEMPLATE_ID"]

with Sandbox.create(
    template=template_id,
    metadata={
        "host-mount": json.dumps([
            {
                "hostPath":  "/tmp/rw",
                "mountPath": "/mnt/rw",
                "readOnly":  False
            },
            {
                "hostPath":  "/tmp/ro",
                "mountPath": "/mnt/ro",
                "readOnly":  True
            }
        ])
    }
) as sandbox:
    info = sandbox.get_info()
    print("sandbox info %s" % info)
