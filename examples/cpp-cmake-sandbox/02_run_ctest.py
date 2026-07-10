# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# Demo 02 (E2B-compatible SDK): build the project and run its CTest suite
# inside the sandbox. A non-zero ctest exit code surfaces as a failed command.

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
TEST_CMD = f"cd {SANDBOX_PROJECT_DIR}/build && ctest --output-on-failure"

with Sandbox.create(template=template_id) as sandbox:
    written = push_project(sandbox)
    print(f"pushed {written} project files -> {SANDBOX_PROJECT_DIR}\n")

    print("=== build ===")
    sandbox.commands.run(
        BUILD_CMD,
        timeout=300,
        on_stdout=lambda line: print(line, end=""),
        on_stderr=lambda line: print(line, end=""),
    )

    print("\n=== ctest ===")
    sandbox.commands.run(
        TEST_CMD,
        timeout=120,
        on_stdout=lambda line: print(line, end=""),
        on_stderr=lambda line: print(line, end=""),
    )
