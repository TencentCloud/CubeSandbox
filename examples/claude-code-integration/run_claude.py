#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""
run_claude.py — spin up a CubeSandbox from the Claude Code template, feed it a
prompt over the E2B protocol, and print the transcript.

The sandbox is fully isolated: Claude Code runs *inside* the MicroVM, and every
tool call it makes (bash, file edits, etc.) hits the sandbox rootfs, never the
host. The Anthropic API key is forwarded per-command via `envs=...` so it
never touches the sandbox filesystem or persistent env.

Usage:
    cp env.example .env  # fill in E2B_API_URL, CUBE_TEMPLATE_ID, ANTHROPIC_API_KEY
    pip install -r requirements.txt
    python run_claude.py --prompt "Write hello.py that prints Hello, Cube!"
"""

from __future__ import annotations

import argparse
import json
import os
import shlex
import sys
from pathlib import Path

from e2b import Sandbox

from env_utils import build_agent_env, load_local_dotenv, required


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument(
        "--prompt",
        default="Create hello.py that prints 'Hello from CubeSandbox!' and run it.",
        help="Task the Claude Code agent should perform inside the sandbox.",
    )
    p.add_argument(
        "--workspace",
        default=os.environ.get("CLAUDE_WORKSPACE", "/workspace"),
        help="Working directory inside the sandbox (default: /workspace).",
    )
    p.add_argument(
        "--seed",
        default=None,
        help="Optional path to a local file uploaded into the workspace before "
             "Claude Code runs (e.g. an existing project the agent should edit).",
    )
    p.add_argument(
        "--timeout",
        type=int,
        default=600,
        help="Sandbox lifetime in seconds (default: 600).",
    )
    p.add_argument(
        "--exec-timeout",
        type=int,
        default=300,
        help="Per-command exec timeout in seconds (default: 300).",
    )
    p.add_argument(
        "--allowed-tools",
        default="Bash(npm:*),Bash(node:*),Bash(python3:*),Edit,Write,Read",
        help="Comma-separated whitelist passed to `claude --allowedTools`.",
    )
    p.add_argument(
        "--pause",
        action="store_true",
        help="Pause the sandbox at the end (for later resume) instead of "
             "destroying it. The sandbox_id is printed on exit.",
    )
    p.add_argument(
        "--stream-json",
        action="store_true",
        help="Emit `--output-format stream-json` events (machine-readable, "
             "one JSON object per turn). Implies `claude --verbose`.",
    )
    return p.parse_args()


def preflight_agent(sandbox: Sandbox) -> None:
    """Sanity-check that the template really has Claude Code installed."""
    result = sandbox.commands.run("claude --version", user="root", timeout=30)
    if result.exit_code != 0:
        raise SystemExit(
            "claude CLI is not installed in the template.\n"
            "Rebuild the image from examples/claude-code-integration/Dockerfile "
            "and re-register the template.\n"
            f"stderr: {result.stderr}"
        )
    print(f"[preflight] {result.stdout.strip()}")


def upload_seed(sandbox: Sandbox, workspace: str, seed: str) -> None:
    """Copy a local file into the sandbox workspace so the agent can edit it."""
    seed_path = Path(seed)
    if not seed_path.is_file():
        raise SystemExit(f"seed file not found: {seed_path}")
    dst = f"{workspace.rstrip('/')}/{seed_path.name}"
    sandbox.files.write(dst, seed_path.read_bytes(), user="root")
    print(f"[seed] {seed_path} -> {dst}")


def build_command(prompt: str, allowed_tools: str, stream_json: bool) -> str:
    """
    Compose a headless `claude` invocation. `--print` disables the interactive
    TUI so we can capture stdout linearly. `--output-format stream-json` is
    optional but very useful when the agent takes many tool-call turns.
    """
    parts = [
        "claude",
        "--print",
        "--allowedTools", allowed_tools,
    ]
    if stream_json:
        parts.extend(["--verbose", "--output-format", "stream-json"])
    parts.append("--")
    parts.append(prompt)
    return " ".join(shlex.quote(p) for p in parts)


def run(args: argparse.Namespace) -> int:
    load_local_dotenv()
    template_id = required("CUBE_TEMPLATE_ID")
    required("ANTHROPIC_API_KEY")

    envs = build_agent_env()
    print(f"[cube] creating sandbox from template={template_id}")
    with Sandbox.create(template=template_id, timeout=args.timeout) as sandbox:
        print(f"[cube] sandbox_id={sandbox.sandbox_id}")
        preflight_agent(sandbox)

        sandbox.commands.run(
            f"mkdir -p {shlex.quote(args.workspace)}", user="root", timeout=15,
        )
        if args.seed:
            upload_seed(sandbox, args.workspace, args.seed)

        cmd = build_command(args.prompt, args.allowed_tools, args.stream_json)
        wrapped = f"cd {shlex.quote(args.workspace)} && {cmd}"

        print(f"[claude] $ {cmd}")
        print("─" * 60)

        result = sandbox.commands.run(
            wrapped,
            envs=envs,
            user="root",
            timeout=args.exec_timeout,
            on_stdout=lambda m: sys.stdout.write(m),
            on_stderr=lambda m: sys.stderr.write(m),
        )

        print("─" * 60)
        print(
            f"[claude] exit_code={result.exit_code} "
            f"len(stdout)={len(result.stdout or '')} "
            f"len(stderr)={len(result.stderr or '')}"
        )

        listing = sandbox.commands.run(
            f"ls -la {shlex.quote(args.workspace)}", user="root", timeout=10,
        )
        print("[cube] workspace after run:")
        print(listing.stdout)

        if args.pause:
            info = sandbox.pause()
            print(json.dumps({"paused": True, "sandbox_id": sandbox.sandbox_id, "info": info}, default=str))

        return 0 if result.exit_code == 0 else result.exit_code or 1


if __name__ == "__main__":
    sys.exit(run(parse_args()))
