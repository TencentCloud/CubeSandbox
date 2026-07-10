# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# Demo 01 (E2B-compatible SDK): push the C++ project into a sandbox, build it
# with CMake + Ninja, then run the resulting executable.

import os

from e2b_code_interpreter import Sandbox

from env_utils import load_local_dotenv
from seed import SANDBOX_PROJECT_DIR, push_project

load_local_dotenv()

template_id = os.environ["CUBE_TEMPLATE_ID"]

BUILD_CMD = (
    f"cd {SANDBOX_PROJECT_DIR} "
    "&& cmake -G Ninja -B build -DCMAKE_BUILD_TYPE=Release "
    "&& cmake --build build"
)
RUN_CMD = f"{SANDBOX_PROJECT_DIR}/build/app"

with Sandbox.create(template=template_id) as sandbox:
    written = push_project(sandbox)
    print(f"pushed {written} project files -> {SANDBOX_PROJECT_DIR}\n")

    print("=== cmake configure + build ===")
    sandbox.commands.run(
        BUILD_CMD,
        timeout=300,
        on_stdout=lambda line: print(line, end=""),
        on_stderr=lambda line: print(line, end=""),
    )

    print("\n=== run ./app ===")
    result = sandbox.commands.run(RUN_CMD, timeout=60)
    print(result.stdout.strip())
