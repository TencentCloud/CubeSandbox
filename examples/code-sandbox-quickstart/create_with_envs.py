# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

import os
from e2b_code_interpreter import Sandbox
from env_utils import load_local_dotenv

load_local_dotenv()

template_id = os.environ["CUBE_TEMPLATE_ID"]

with Sandbox.create(
    template=template_id,
    envs={
        "API_TOKEN": "demo-token",
        "SESSION_ID": "user-session-test",
    },
) as sandbox:
    result = sandbox.commands.run("echo $SESSION_ID")
    print(result.stdout)
