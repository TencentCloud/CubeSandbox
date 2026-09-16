# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

import os
from e2b_code_interpreter import Sandbox
from env_utils import apply_create_time_envs, ensure_dev_sidecar, load_local_dotenv

load_local_dotenv()
ensure_dev_sidecar()

template_id = os.environ["CUBE_TEMPLATE_ID"]
create_envs = {
    "API_TOKEN": "demo-token",
    "SESSION_ID": "user-session-test",
}

with Sandbox.create(template=template_id, envs=create_envs) as sandbox:
    # Best-effort: some cubebox/VNC templates need a post-create /init re-apply.
    apply_create_time_envs(sandbox, create_envs)

    result = sandbox.commands.run('printf "session is %s" "$SESSION_ID"')
    print(result.stdout)
