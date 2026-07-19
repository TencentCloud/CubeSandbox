# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import argparse
import os

from common import (
    SUMMARY_PATH,
    build_workbench_notebook,
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
    parser.add_argument(
        "--jupyter-port",
        type=int,
        default=8888,
        help="JupyterLab port exposed by the template.",
    )
    return parser.parse_args()


def main() -> None:
    args = parse_args()
    load_env()

    template_id = args.template or os.environ["CUBE_TEMPLATE_ID"]

    with create_sandbox(template=template_id, timeout=args.timeout) as sandbox:
        prepare_workspace(sandbox)
        write_text(sandbox, "/workspace/notebooks/jupyter_ml_workbench.ipynb", build_workbench_notebook())

        print(f"Sandbox: {sandbox.get_info().sandbox_id}")
        print(f"JupyterLab: {sandbox_url(sandbox, args.jupyter_port)}/lab")

        run = sandbox.commands.run(
            "jupyter nbconvert "
            "--to notebook "
            "--execute /workspace/notebooks/jupyter_ml_workbench.ipynb "
            "--output jupyter_ml_workbench.executed.ipynb "
            "--output-dir /workspace/artifacts "
            "--ExecutePreprocessor.timeout=300 "
            "--ExecutePreprocessor.kernel_name=python3",
            cwd="/workspace",
            timeout=600,
            user="root",
        )
        ensure_success(run, "execute the Jupyter notebook")
        print(run.stdout)

        artifacts = sandbox.commands.run("find /workspace/artifacts -maxdepth 1 -type f | sort")
        ensure_success(artifacts, "list notebook artifacts")
        print("Artifacts:")
        print(artifacts.stdout.strip())

        summary = sandbox.files.read(SUMMARY_PATH)
        print("Summary:")
        print(summary)


if __name__ == "__main__":
    main()

