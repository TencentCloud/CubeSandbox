#!/usr/bin/env python3
"""Run CodeBuddy CLI inside a Cube Sandbox template."""

import argparse
import os
import shlex
from typing import Dict, Iterable, Optional

from env_utils import load_local_dotenv

DEMO_DIR = "/tmp/codebuddy-demo"
SCRIPT_PATH = "/tmp/run-codebuddy.sh"
DEFAULT_CONFIG_DIR = "/workspace/.codebuddy"
DEFAULT_OUTPUT_FORMAT = "text"
DEFAULT_PROMPT = (
    "Inspect /tmp/codebuddy-demo, run python3 hello.py, "
    "and explain what the script does."
)


def require_env(keys: Iterable[str]) -> Dict[str, str]:
    missing = [key for key in keys if not os.environ.get(key)]
    if missing:
        raise SystemExit(
            "Missing required environment variables: " + ", ".join(missing)
        )
    return {key: os.environ[key] for key in keys}


def positive_int(value: Optional[str], default: int) -> int:
    if value is None or value == "":
        return default
    try:
        parsed = int(value)
    except ValueError as exc:
        raise SystemExit(f"Expected an integer, got: {value}") from exc
    if parsed <= 0:
        raise SystemExit(f"Expected a positive integer, got: {value}")
    return parsed


def build_codebuddy_script(
    prompt: str,
    output_format: str,
    config_dir: str,
    allowed_tools: Optional[str] = None,
    permission_mode: Optional[str] = None,
) -> str:
    command = ["codebuddy", "-p", prompt, "--output-format", output_format]
    if allowed_tools:
        command.extend(["--allowedTools", allowed_tools])
    if permission_mode:
        command.extend(["--permission-mode", permission_mode])

    rendered_command = " ".join(shlex.quote(part) for part in command)
    quoted_config_dir = shlex.quote(config_dir)
    quoted_demo_dir = shlex.quote(DEMO_DIR)

    return f"""#!/usr/bin/env bash
set -euo pipefail

export DISABLE_AUTOUPDATER=1
export CODEBUDDY_CONFIG_DIR={quoted_config_dir}
mkdir -p "$CODEBUDDY_CONFIG_DIR"

cd {quoted_demo_dir}

echo "[codebuddy] version"
codebuddy --version

echo "[codebuddy] running prompt"
{rendered_command}
"""


def sandbox_env(config_dir: str) -> Dict[str, str]:
    env = {
        "CODEBUDDY_API_KEY": os.environ["CODEBUDDY_API_KEY"],
        "CODEBUDDY_CONFIG_DIR": config_dir,
        "DISABLE_AUTOUPDATER": "1",
    }
    for key in ("HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY"):
        if os.environ.get(key):
            env[key] = os.environ[key]
    return env


def write_demo_workspace(sandbox) -> None:
    sandbox.commands.run(f"mkdir -p {shlex.quote(DEMO_DIR)}")
    sandbox.files.write(
        f"{DEMO_DIR}/hello.py",
        "print('hello from Cube Sandbox + CodeBuddy')\n",
    )
    sandbox.files.write(
        f"{DEMO_DIR}/README.md",
        "# CodeBuddy Cube Sandbox Demo\n\n"
        "This workspace is created by `run_codebuddy.py` before the CodeBuddy "
        "CLI starts. The demo prompt asks CodeBuddy to inspect this directory "
        "and run `python3 hello.py`.\n",
    )


def print_result(result) -> None:
    stdout = getattr(result, "stdout", "") or ""
    stderr = getattr(result, "stderr", "") or ""
    exit_code = getattr(result, "exit_code", None)

    if stdout:
        print(stdout, end="" if stdout.endswith("\n") else "\n")
    if stderr:
        print(stderr, end="" if stderr.endswith("\n") else "\n")
    if exit_code is None:
        raise SystemExit("Sandbox command did not report an exit code")
    if exit_code != 0:
        raise SystemExit(f"Sandbox command failed with exit code {exit_code}")


def verify_pause_resume(sandbox, config_dir: str) -> None:
    marker_path = f"{config_dir.rstrip('/')}/cube-pause-resume-marker.txt"
    quoted_config_dir = shlex.quote(config_dir)
    quoted_marker = shlex.quote(marker_path)

    print("[pause-resume] writing marker before pause")
    result = sandbox.commands.run(
        "mkdir -p "
        + quoted_config_dir
        + " && printf '%s\\n' codebuddy-state-preserved > "
        + quoted_marker
        + " && cat "
        + quoted_marker
    )
    print_result(result)

    print("[pause-resume] pausing sandbox")
    sandbox.pause()

    print("[pause-resume] reconnecting sandbox")
    sandbox.connect()

    print("[pause-resume] reading marker after resume")
    result = sandbox.commands.run("cat " + quoted_marker)
    print_result(result)
    if "codebuddy-state-preserved" not in (getattr(result, "stdout", "") or ""):
        raise SystemExit("Pause/resume marker was not preserved")


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Run CodeBuddy CLI inside a Cube Sandbox template"
    )
    parser.add_argument(
        "--template",
        default=os.environ.get("CUBE_TEMPLATE_ID"),
        help="Cube template ID. Defaults to CUBE_TEMPLATE_ID.",
    )
    parser.add_argument(
        "--prompt",
        default=os.environ.get("CODEBUDDY_PROMPT", DEFAULT_PROMPT),
        help="Prompt passed to `codebuddy -p`.",
    )
    parser.add_argument(
        "--output-format",
        default=os.environ.get("CODEBUDDY_OUTPUT_FORMAT", DEFAULT_OUTPUT_FORMAT),
        help="CodeBuddy output format, for example text, json, or stream-json.",
    )
    parser.add_argument(
        "--config-dir",
        default=os.environ.get("CODEBUDDY_CONFIG_DIR", DEFAULT_CONFIG_DIR),
        help="Directory used by CodeBuddy for runtime configuration inside the sandbox.",
    )
    parser.add_argument(
        "--allowed-tools",
        default=os.environ.get("CODEBUDDY_ALLOWED_TOOLS", "Bash,Read,Write,Edit"),
        help="Optional CodeBuddy allowed tools list. Set to empty to omit.",
    )
    parser.add_argument(
        "--permission-mode",
        default=os.environ.get("CODEBUDDY_PERMISSION_MODE", "bypassPermissions"),
        help="Optional CodeBuddy permission mode. Set to empty to omit.",
    )
    parser.add_argument(
        "--timeout",
        type=int,
        default=positive_int(os.environ.get("CUBE_SANDBOX_TIMEOUT"), 600),
        help="Sandbox timeout in seconds.",
    )
    parser.add_argument(
        "--pause-resume",
        action="store_true",
        help="After the CodeBuddy run, pause and reconnect the sandbox to verify state.",
    )
    return parser.parse_args()


def main() -> None:
    load_local_dotenv()
    args = parse_args()

    require_env(["E2B_API_URL", "E2B_API_KEY", "CODEBUDDY_API_KEY"])
    if not args.template:
        raise SystemExit("Missing template: set CUBE_TEMPLATE_ID or pass --template")

    from e2b_code_interpreter import Sandbox

    print(f"[cube] template: {args.template}")
    print(f"[cube] api url:  {os.environ['E2B_API_URL']}")
    print(f"[cube] timeout:  {args.timeout}s")
    print(f"[codebuddy] config dir: {args.config_dir}")

    with Sandbox.create(
        template=args.template,
        timeout=args.timeout,
        envs=sandbox_env(args.config_dir),
    ) as sandbox:
        print(f"[cube] sandbox id: {sandbox.sandbox_id}")
        write_demo_workspace(sandbox)

        script = build_codebuddy_script(
            prompt=args.prompt,
            output_format=args.output_format,
            config_dir=args.config_dir,
            allowed_tools=args.allowed_tools,
            permission_mode=args.permission_mode,
        )
        sandbox.files.write(SCRIPT_PATH, script)
        sandbox.commands.run(f"chmod +x {shlex.quote(SCRIPT_PATH)}")

        result = sandbox.commands.run(f"bash {shlex.quote(SCRIPT_PATH)}")
        print_result(result)

        if args.pause_resume:
            verify_pause_resume(sandbox, args.config_dir)


if __name__ == "__main__":
    main()
