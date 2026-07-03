# Copyright (c) 2024 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

import os
from pathlib import Path

from dotenv import load_dotenv
from e2b import Sandbox


def load_local_dotenv() -> None:
    for candidate in (Path(__file__).with_name(".env"), Path.cwd() / ".env"):
        if candidate.is_file():
            load_dotenv(dotenv_path=candidate, override=False)
            return


def run(sandbox: Sandbox, command: str) -> None:
    print(f"$ {command}")
    result = sandbox.commands.run(command)
    if result.stdout:
        print(result.stdout, end="" if result.stdout.endswith("\n") else "\n")
    if result.stderr:
        print(result.stderr, end="" if result.stderr.endswith("\n") else "\n")
    exit_code = result.exit_code
    if exit_code:
        raise SystemExit(f"command failed with exit code {exit_code}: {command}")


load_local_dotenv()

template_id = os.environ["CUBE_TEMPLATE_ID"]

with Sandbox.create(template=template_id) as sandbox:
    run(sandbox, "go version")
    run(sandbox, "cd /workspace/hello-go && go test ./...")
    run(sandbox, "cd /workspace/hello-go && go run .")
    run(
        sandbox,
        "mkdir -p /workspace/runtime-smoke "
        "&& printf '%s\\n' go-runtime-ok > /workspace/runtime-smoke/marker.txt",
    )
    run(
        sandbox,
        "cat /workspace/runtime-smoke/marker.txt "
        "&& test \"$(cat /workspace/runtime-smoke/marker.txt)\" = go-runtime-ok",
    )
