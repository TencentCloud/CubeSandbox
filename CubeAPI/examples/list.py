# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

import os
from e2b_code_interpreter import Sandbox
from e2b.sandbox.sandbox_api import SandboxQuery, SandboxState

# os.environ["E2B_API_KEY"] = "dummy"
# os.environ["E2B_API_URL"] = "http://localhost:3000"
# os.environ["SSL_CERT_FILE"] = "/root/.local/share/mkcert/rootCA.pem"

# List all running sandboxes (paginated)
paginator = Sandbox.list(query=SandboxQuery(state=[SandboxState.RUNNING]))

sandboxes = []
while paginator.has_next:
    sandboxes.extend(paginator.next_items())

print("total running sandboxes: %d" % len(sandboxes))
for sb in sandboxes:
    print("  sandbox_id=%s template=%s started_at=%s" % (sb.sandbox_id, sb.template_id, sb.started_at))
