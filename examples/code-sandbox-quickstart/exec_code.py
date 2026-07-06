# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

import os
from e2b_code_interpreter import Sandbox
from env_utils import load_local_dotenv

load_local_dotenv()

template_id = os.environ["CUBE_TEMPLATE_ID"]

python_code = """
print("hello cube")
"""

with Sandbox.create(template=template_id) as sandbox:
    print(sandbox.run_code(python_code, on_stdout=lambda data: print(data)))
