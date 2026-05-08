# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from cubesandbox import Sandbox

template_id = os.environ["CUBE_TEMPLATE_ID"]

python_code = """
print("hello cube")
"""

with Sandbox.create(template=template_id) as sandbox:
    print(sandbox.run_code(python_code, on_stdout=lambda data: print(data)))
