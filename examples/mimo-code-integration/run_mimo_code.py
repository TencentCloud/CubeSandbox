#!/usr/bin/env python3
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import argparse
import os
import shlex
import sys

from _mimo_common import (
    ensure_success,
    kill_sandbox,
    run_command,
    run_mimo_command,
    sandbox_identifier,
    session_id_from_events,
)
from env_utils import (
    MIMO_API_KEY_ENV,
    build_mimo_env,
    int_env,
    load_local_dotenv,
    mimo_command,
    mimo_workspace,
    positive_int,
    required,
    shell_join,
)
from network_policy import (
    DEFAULT_NODE_CA_BUNDLE,
    PLACEHOLDER_KEY,
    create_sandbox,
    show_secret_boundary,
    verify_ca_bundle,
)

DEFAULT_PROMPT = (
    "Inspect the project, run python3 app.py, and write a concise result to "
    "{workspace}/result.md. The file must contain the exact marker "
    "CUBE_MIMO_RUN_OK."
)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description=(
            "Run a one-shot MiMo Code smoke task inside CubeSandbox with "
            "CubeEgress placeholder credentials."
        )
    )
    parser.add_argument("--template", default=os.environ.get("CUBE_TEMPLATE_ID"))
    parser.add_argument("--workspace", default=mimo_workspace())
    parser.add_argument(
        "--prompt",
        help=(
            "Custom task. It must create result.md with CUBE_MIMO_RUN_OK unless "
            "--skip-result-check is set."
        ),
    )
    parser.add_argument(
        "--agent",
        choices=("build", "compose"),
        default="build",
        help="Use MiMo's normal build agent or its Compose multi-agent mode.",
    )
    parser.add_argument(
        "--sandbox-timeout",
        type=positive_int,
        default=int_env("MIMO_SANDBOX_TIMEOUT", 1800),
    )
    parser.add_argument(
        "--exec-timeout",
        type=positive_int,
        default=int_env("MIMO_AGENT_EXEC_TIMEOUT", 900),
    )
    parser.add_argument("--no-seed", action="store_true")
    parser.add_argument("--skip-result-check", action="store_true")
    parser.add_argument("--raw", action="store_true")
    args = parser.parse_args()
    if args.prompt is None:
        args.prompt = DEFAULT_PROMPT.format(workspace=args.workspace)
    return args


def seed_project(sandbox, workspace: str) -> None:
    directory = shlex.quote(workspace)
    command = f"""mkdir -p {directory}
cat > {directory}/README.md <<'EOF'
# CubeSandbox MiMo Code Smoke Project

This tiny project provides a deterministic task for MiMo Code.
EOF
cat > {directory}/app.py <<'EOF'
def main() -> None:
    print("hello from CubeSandbox + MiMo Code")


if __name__ == "__main__":
    main()
EOF
"""
    result = run_command(sandbox, command, timeout=60)
    ensure_success(result, "seed the workspace")


def verify_result(sandbox, workspace: str) -> None:
    directory = shlex.quote(workspace)
    result_file = shlex.quote(f"{workspace}/result.md")
    command = shell_join(
        f"test -f {result_file}",
        f"grep -Fq CUBE_MIMO_RUN_OK {result_file}",
        f"ls -la {directory}",
        f"printf '\\n--- result.md ---\\n' && cat {result_file}",
    )
    result = run_command(sandbox, command, timeout=60)
    ensure_success(result, "verify MiMo Code's result")
    if getattr(result, "stdout", ""):
        print(result.stdout)


def main() -> int:
    load_local_dotenv()
    args = parse_args()
    if args.raw:
        os.environ["MIMO_STREAM_RAW"] = "1"

    template_id = args.template or required("CUBE_TEMPLATE_ID")
    api_url = required("E2B_API_URL")
    api_key = required("E2B_API_KEY")
    secret = required(MIMO_API_KEY_ENV)

    envs = build_mimo_env(include_secret=False)
    envs[MIMO_API_KEY_ENV] = PLACEHOLDER_KEY
    envs["NODE_EXTRA_CA_CERTS"] = os.environ.get(
        "MIMO_NODE_EXTRA_CA_CERTS", DEFAULT_NODE_CA_BUNDLE
    )

    print(f"Creating CubeEgress sandbox from template: {template_id}")
    sandbox = create_sandbox(
        template_id,
        secret,
        args.sandbox_timeout,
        api_url=api_url,
        api_key=api_key,
    )
    sandbox_id = sandbox_identifier(sandbox)
    try:
        print(f"Sandbox ready: {sandbox_id}")
        verify_ca_bundle(sandbox, envs)
        show_secret_boundary(sandbox, envs)

        version = run_command(sandbox, "mimo --version", timeout=60)
        ensure_success(version, "check the MiMo Code version")
        print(f"MiMo Code version: {getattr(version, 'stdout', '').strip()}")

        if not args.no_seed:
            seed_project(sandbox, args.workspace)
            print(f"Seeded demo project in {args.workspace}")

        command = mimo_command(
            args.prompt,
            workspace=args.workspace,
            agent=args.agent,
        )
        print(f"\nRunning MiMo Code with the {args.agent!r} agent...\n")
        result, events = run_mimo_command(
            sandbox,
            command,
            cwd=args.workspace,
            envs=envs,
            timeout=args.exec_timeout,
        )
        ensure_success(result, "run MiMo Code")
        session_id = session_id_from_events(events)
        print(f"\nMiMo session ID: {session_id}")
        if not args.skip_result_check:
            verify_result(sandbox, args.workspace)
        show_secret_boundary(sandbox, envs)
        return 0
    finally:
        kill_sandbox(
            sandbox,
            sandbox_id,
            run_failed=sys.exc_info()[0] is not None,
        )


if __name__ == "__main__":
    sys.exit(main())
