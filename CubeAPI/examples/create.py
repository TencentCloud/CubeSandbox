# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

import os
from e2b_code_interpreter import Sandbox

# os.environ["E2B_API_KEY"] = "dummy"
# os.environ["E2B_API_URL"] = "http://localhost:3000"
# os.environ["SSL_CERT_FILE"] = "/root/.local/share/mkcert/rootCA.pem"

template_id = os.environ["CUBE_TEMPLATE_ID"]

with Sandbox.create(template=template_id) as sandbox:
    info = sandbox.get_info()
    print("sandbox info %s" % info)
