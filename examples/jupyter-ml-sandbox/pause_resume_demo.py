# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import argparse
import json
import os
import time

from common import (
    create_sandbox,
    ensure_success,
    load_env,
    prepare_workspace,
    sandbox_url,
    write_text,
)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--template", default=None, help="Cube template ID.")
    parser.add_argument("--timeout", type=int, default=900, help="Sandbox timeout in seconds.")
    parser.add_argument("--jupyter-port", type=int, default=8888, help="JupyterLab port.")
    return parser.parse_args()


def main() -> None:
    args = parse_args()
    load_env()

    template_id = args.template or os.environ["CUBE_TEMPLATE_ID"]

    with create_sandbox(template=template_id, timeout=args.timeout) as sandbox:
        prepare_workspace(sandbox)
        write_text(
            sandbox,
            "/workspace/artifacts/pause_resume_checkpoint.json",
            json.dumps({"step": 1, "message": "checkpoint before pause"}, indent=2),
        )

        before = sandbox.files.read("/workspace/artifacts/pause_resume_checkpoint.json")
        print(f"Sandbox: {sandbox.get_info().sandbox_id}")
        print(f"JupyterLab: {sandbox_url(sandbox, args.jupyter_port)}/lab")
        print("Before pause:")
        print(before)

        sandbox.pause()
        time.sleep(1)
        sandbox.connect()

        after = sandbox.files.read("/workspace/artifacts/pause_resume_checkpoint.json")
        print("After resume:")
        print(after)

        if before != after:
            raise SystemExit("pause/resume state check failed")

        run = sandbox.commands.run(
            "python3 - <<'PY'\n"
            "from pathlib import Path\n"
            "path = Path('/workspace/artifacts/pause_resume_checkpoint.json')\n"
            "print(path.read_text())\n"
            "PY",
            cwd="/workspace",
            timeout=60,
            user="root",
        )
        ensure_success(run, "re-read checkpoint after resume")
        print(run.stdout)


if __name__ == "__main__":
    main()
