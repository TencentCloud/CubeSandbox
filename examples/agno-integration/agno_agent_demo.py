"""Run an Agno custom tool that executes Python only inside CubeSandbox."""

from __future__ import annotations

import argparse
import itertools
import os
import sys

from agno.agent import Agent
from agno.models.openai import OpenAIChat
from cubesandbox import Sandbox
from dotenv import load_dotenv


MAX_CODE_BYTES = 16 * 1024


def require_env(*names: str) -> None:
    """Exit with one actionable message when required configuration is absent."""
    missing = [name for name in names if not os.environ.get(name)]
    if missing:
        raise SystemExit(f"Missing environment variable(s): {', '.join(missing)}")


def format_result(result: object) -> str:
    """Make stdout, stderr, and an exit code unambiguous to the model."""
    stdout = getattr(result, "stdout", "") or ""
    stderr = getattr(result, "stderr", "") or ""
    exit_code = getattr(result, "exit_code", 0)
    output = stdout
    if stderr:
        output += "\n--- stderr ---\n" + stderr
    if exit_code:
        output += f"\n[non-zero exit code: {exit_code}]"
    return output or "[command completed without output]"


def make_run_python(sandbox: Sandbox):
    """Return an Agno-compatible Python function bound to one MicroVM."""
    script_counter = itertools.count()

    def run_python(code: str) -> str:
        """Execute Python in the CubeSandbox MicroVM and return its output.

        The code never runs on the Agent host. Each invocation writes a unique
        script under /workspace and runs it with a 120-second command timeout.
        Use print() to return results to the Agent.
        """
        if not isinstance(code, str) or not code.strip():
            return "Error: code must be a non-empty string."
        if "\x00" in code:
            return "Error: code must not contain NUL bytes."
        if len(code.encode("utf-8")) > MAX_CODE_BYTES:
            return f"Error: code exceeds the {MAX_CODE_BYTES}-byte limit."

        script = f"/workspace/_agno_agent_{next(script_counter)}.py"
        sandbox.files.write(script, code)
        result = sandbox.commands.run(
            f"python3 {script}", timeout=120, cwd="/workspace"
        )
        return format_result(result)

    return run_python


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--sandbox-only",
        action="store_true",
        help="Verify the CubeSandbox execution path without calling an LLM.",
    )
    parser.add_argument(
        "--prompt",
        default="Use run_python to calculate the sum of the squares from 1 to 10. "
        "Explain the result concisely.",
        help="Task for the Agno Agent.",
    )
    args = parser.parse_args()

    load_dotenv()
    require_env("CUBE_TEMPLATE_ID")

    # A single MicroVM is reused for every tool call in this Agent run. Leaving
    # the block destroys it, including when the model or tool call fails.
    with Sandbox.create(template=os.environ["CUBE_TEMPLATE_ID"], timeout=600) as sandbox:
        print(f"Sandbox {sandbox.sandbox_id} created.")
        run_python = make_run_python(sandbox)

        if args.sandbox_only:
            print(run_python("print(sum(i * i for i in range(1, 11)))"))
            return

        require_env("OPENAI_API_KEY")
        model = OpenAIChat(
            id=os.getenv("AGNO_MODEL", "gpt-4o-mini"),
            api_key=os.environ["OPENAI_API_KEY"],
            base_url=os.getenv("OPENAI_BASE_URL") or None,
            timeout=60,
            max_retries=2,
            temperature=0,
        )
        agent = Agent(
            name="CubeSandbox Python Agent",
            model=model,
            tools=[run_python],
            instructions=[
                "Use run_python for every calculation or code-execution task.",
                "The execution environment is /workspace inside an isolated MicroVM.",
                "Never claim a computation succeeded unless run_python returned it.",
            ],
            markdown=True,
        )
        agent.print_response(args.prompt, stream=True)


if __name__ == "__main__":
    main()
