#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Run a one-shot CodeBuddy CI task in a disposable CubeSandbox MicroVM."""

from __future__ import annotations

import argparse
import os
import shlex
import sys
from pathlib import Path

from e2b import Sandbox

from config import (
    codebuddy_command,
    codebuddy_env,
    load_local_dotenv,
    positive_int,
    required,
    shell_join,
    workspace,
)

DEFAULT_PROMPT = (
    "Inspect the repository, run its smallest relevant test command, and write "
    "a concise CI report to /workspace/codebuddy-ci-report.md. Do not commit "
    "or push changes."
)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--template", default=os.environ.get("CUBE_TEMPLATE_ID"))
    parser.add_argument("--prompt", default=DEFAULT_PROMPT)
    parser.add_argument(
        "--source-tar",
        help="Optional .tar archive to upload and extract into the workspace.",
    )
    parser.add_argument("--session-id", default=os.environ.get("CODEBUDDY_SESSION_ID", "cube-ci"))
    parser.add_argument(
        "--pause",
        action="store_true",
        help="Pause instead of killing the sandbox so resume_codebuddy_ci.py can continue it.",
    )
    return parser.parse_args()


def ensure_success(result, action: str) -> None:
    if getattr(result, "exit_code", None) not in (None, 0):
        raise SystemExit(
            f"Failed to {action} (exit {getattr(result, 'exit_code', 'unknown')}).\n"
            f"STDOUT:\n{getattr(result, 'stdout', '')}\n"
            f"STDERR:\n{getattr(result, 'stderr', '')}"
        )


def upload_source_tar(sandbox: Sandbox, source_tar: str, destination: str) -> None:
    """Upload a regular tar archive, avoiding host-path handling in the VM."""
    source = Path(source_tar).resolve()
    if not source.is_file() or source.suffix != ".tar":
        raise SystemExit(f"--source-tar must be an existing .tar file: {source}")
    if source.stat().st_size > 100 * 1024 * 1024:
        raise SystemExit("--source-tar must be no larger than 100 MiB")
    sandbox.files.write(destination, source.read_bytes())


def main() -> int:
    load_local_dotenv()
    args = parse_args()
    template = args.template or required("CUBE_TEMPLATE_ID")
    required("E2B_API_URL")
    required("E2B_API_KEY")
    command_env = codebuddy_env()
    target_workspace = workspace()
    sandbox_timeout = positive_int("CODEBUDDY_SANDBOX_TIMEOUT", 1800)
    exec_timeout = positive_int("CODEBUDDY_EXEC_TIMEOUT", 900)
    max_turns = positive_int("CODEBUDDY_MAX_TURNS", 3)

    command = shell_join(
        f"mkdir -p {shlex.quote(target_workspace)}",
        f"cd {shlex.quote(target_workspace)}",
        codebuddy_command(args.prompt, session_id=args.session_id, max_turns=max_turns),
    )
    print(f"Creating CodeBuddy CI sandbox from template: {template}")
    # Do not use a context manager: its cleanup kills a paused sandbox and
    # would make the documented snapshot/resume flow impossible.
    sandbox = Sandbox.create(template=template, timeout=sandbox_timeout)
    sandbox_id = getattr(sandbox, "sandbox_id", getattr(sandbox, "id", "unknown"))
    should_kill = True
    try:
        print(f"Sandbox ready: {sandbox_id}")
        preflight = sandbox.commands.run("codebuddy --version", timeout=60, user="root")
        ensure_success(preflight, "check CodeBuddy version")
        print(f"CodeBuddy version: {getattr(preflight, 'stdout', '').strip()}")

        if args.source_tar:
            archive_path = f"{target_workspace}/source.tar"
            upload_source_tar(sandbox, args.source_tar, archive_path)
            extract = sandbox.commands.run(
                f"tar -xf {shlex.quote(archive_path)} -C {shlex.quote(target_workspace)} && rm {shlex.quote(archive_path)}",
                timeout=120,
                user="root",
            )
            ensure_success(extract, "extract source archive")
            print(f"Uploaded and extracted {Path(args.source_tar).resolve()} to {target_workspace}")

        result = sandbox.commands.run(
            command,
            cwd=target_workspace,
            envs=command_env,
            timeout=exec_timeout,
            user="root",
        )
        ensure_success(result, "run CodeBuddy CI task")
        print(getattr(result, "stdout", ""))
        report = sandbox.commands.run(
            f"test ! -f {shlex.quote(target_workspace)}/codebuddy-ci-report.md || cat {shlex.quote(target_workspace)}/codebuddy-ci-report.md",
            timeout=60,
            user="root",
        )
        ensure_success(report, "collect CI report")
        if getattr(report, "stdout", ""):
            print("\n--- codebuddy-ci-report.md ---")
            print(report.stdout)
        if args.pause:
            paused_id = sandbox.pause()
            if isinstance(paused_id, str) and paused_id:
                sandbox_id = paused_id
            should_kill = False
            print(f"Paused. Resume handle: {sandbox_id}")
    finally:
        if should_kill:
            try:
                sandbox.kill()
                print(f"Sandbox {sandbox_id} killed.")
            except Exception as exc:  # noqa: BLE001 - cleanup must not mask real failures
                print(f"Warning: failed to kill sandbox {sandbox_id}: {exc}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
