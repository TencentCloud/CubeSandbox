"""Pydantic AI agent whose `run_python` tool executes code inside a CubeSandbox MicroVM.

Flow: user prompt -> Pydantic AI Agent -> the model calls the `run_python`
function tool -> the tool writes the snippet into a CubeSandbox MicroVM and runs
it with the official `cubesandbox` Python SDK -> stdout / stderr / exit code go
back to the model -> the model continues reasoning -> final answer.

One MicroVM is created per agent run and reused across every tool call; the
`with` context manager tears it down when the run finishes.
"""

from __future__ import annotations

import itertools
import os
import shlex
import sys
from collections.abc import Iterator
from dataclasses import dataclass, field

from cubesandbox import CubeSandboxError, Sandbox
from dotenv import load_dotenv
from pydantic_ai import Agent, RunContext
from pydantic_ai.models.openai import OpenAIChatModel
from pydantic_ai.providers.openai import OpenAIProvider

load_dotenv()

# Working directory inside the MicroVM. The stock `sandbox-code` image does not
# ship a /workspace, so main() creates it once after the sandbox boots.
WORKDIR = "/workspace"
# How long a single `python3 <script>` invocation may run inside the MicroVM.
EXEC_TIMEOUT = 120
# Generous idle-timeout backstop (seconds). The idle clock only resets when the
# sandbox receives a request; each run_python tool call refreshes it, so ordinary
# model latency is fine. Normal teardown is the `with` block's kill() — this
# timeout only bounds an orphaned MicroVM if the agent host dies before that
# runs. Raise it for tasks with long tool-free stretches; NEVER_TIMEOUT would
# remove the backstop and risk an orphan on a host crash.
SANDBOX_TIMEOUT = 1800
# The Fibonacci demo only needs the Python standard library, so the sandbox is
# created with no outbound internet access. Flip to True if your own task needs
# to reach the network from inside the MicroVM.
ALLOW_INTERNET = False


# --------------------------------------------------------------------------- #
# Typed dependencies passed into every tool call via RunContext.
# --------------------------------------------------------------------------- #
@dataclass
class Deps:
    """Per-run dependencies handed to the agent's tools.

    `sandbox` is the single MicroVM shared across the run. `_script_counter`
    hands out a fresh index per tool call so concurrent / repeated calls never
    overwrite each other's script file.
    """

    sandbox: Sandbox
    _script_counter: Iterator[int] = field(default_factory=lambda: itertools.count())


INSTRUCTIONS = (
    "You solve tasks by writing small Python programs and running them with the "
    "run_python tool, which executes inside an isolated CubeSandbox MicroVM.\n"
    "- Only the Python standard library is available; do not rely on third-party "
    "packages or network access.\n"
    "- Every numeric result you report MUST come from stdout that run_python "
    "actually returned. Never guess, estimate, or pre-compute values yourself.\n"
    "- Make your program print the exact values you need, then read them back "
    "from the tool output.\n"
    "- If the tool output contains a stderr section or a non-zero exit code, fix "
    "the program and call run_python again.\n"
    "- If the tool output starts with [cube-sandbox error] or [execution failed], "
    "the sandbox call itself failed (not your code): retry once, and if it fails "
    "again, report the failure instead of guessing an answer."
)

agent = Agent(deps_type=Deps, instructions=INSTRUCTIONS)


@agent.tool
def run_python(ctx: RunContext[Deps], code: str) -> str:
    """Execute a Python 3 snippet inside the CubeSandbox MicroVM and return its output.

    Returns stdout. If the program writes to stderr, that is appended below a
    `--- stderr ---` delimiter; a non-zero exit code is reported on its own line.
    The MicroVM is reused across calls, so files you write in one call are still
    present in later calls of the same run.
    """
    sandbox = ctx.deps.sandbox
    # Unique per-call script path: the index comes from a host-side counter, so
    # the path is fully controlled (no model input is interpolated into it).
    script_path = f"{WORKDIR}/agent_step_{next(ctx.deps._script_counter)}.py"

    try:
        sandbox.files.write(script_path, code)
        result = sandbox.commands.run(
            f"python3 {shlex.quote(script_path)}",
            timeout=EXEC_TIMEOUT,
            cwd=WORKDIR,
        )
    except CubeSandboxError as exc:
        # Surface execution/transport failures to the model as tool output so it
        # can decide to retry, rather than aborting the whole agent run.
        return f"[cube-sandbox error] {type(exc).__name__}: {exc}"
    except Exception as exc:  # noqa: BLE001 - e.g. an envd request timeout
        return f"[execution failed] {type(exc).__name__}: {exc}"

    out = result.stdout or ""
    # Keep stderr delimited so library warnings (with exit_code 0) don't blur the
    # real stdout the model reads.
    if result.stderr:
        out += "\n--- stderr ---\n" + result.stderr
    if result.exit_code != 0:
        out += f"\n[non-zero exit code: {result.exit_code}]"
    return out or "[no output]"


def build_model() -> OpenAIChatModel:
    """Configure any OpenAI-compatible chat endpoint from environment variables."""
    api_key = os.getenv("OPENAI_API_KEY")
    if not api_key:
        print("Missing env: OPENAI_API_KEY", file=sys.stderr)
        sys.exit(1)
    provider = OpenAIProvider(
        base_url=os.getenv("OPENAI_BASE_URL", "https://tokenhub.tencentmaas.com/v1"),
        api_key=api_key,
    )
    # Accept CHAT_MODEL too, for parity with the LangChain example's .env.
    model_name = os.getenv("MODEL_NAME") or os.getenv("CHAT_MODEL") or "deepseek-v3"
    return OpenAIChatModel(model_name, provider=provider)


def main() -> None:
    template_id = os.getenv("CUBE_TEMPLATE_ID")
    if not template_id:
        print("Missing env: CUBE_TEMPLATE_ID", file=sys.stderr)
        sys.exit(1)

    # If the CubeAPI endpoint uses a self-signed CA, export it so both the
    # control-plane (requests -> REQUESTS_CA_BUNDLE) and data-plane (httpx/envd
    # -> SSL_CERT_FILE) clients trust it. This process-global export also affects
    # the LLM client, so the bundle must include public root CAs too.
    cube_ssl = os.getenv("CUBE_SSL_CERT_FILE")
    if cube_ssl and os.path.isfile(cube_ssl):
        os.environ["SSL_CERT_FILE"] = cube_ssl
        os.environ["REQUESTS_CA_BUNDLE"] = cube_ssl

    model = build_model()

    question = sys.argv[1] if len(sys.argv) > 1 else (
        "Generate the first 20 Fibonacci numbers with F(1)=1 and F(2)=1. "
        "Verify that every term satisfies F(n) = F(n-1) + F(n-2), then report "
        "the full sequence, the 20th Fibonacci number, and whether all "
        "recurrence checks passed."
    )

    # One MicroVM for the whole run, reused across every run_python tool call.
    # Sandbox.create() failures propagate here and abort the run cleanly; the
    # context manager tears the sandbox down on exit (success or error).
    with Sandbox.create(
        template=template_id,
        timeout=SANDBOX_TIMEOUT,
        allow_internet_access=ALLOW_INTERNET,
    ) as sandbox:
        print(f"Sandbox {sandbox.sandbox_id} created. Running agent...")
        # The stock sandbox-code image has no /workspace; create it once so the
        # tool's files.write and cwd=WORKDIR succeed on an unmodified template.
        sandbox.commands.run(f"mkdir -p {shlex.quote(WORKDIR)}")
        result = agent.run_sync(question, deps=Deps(sandbox=sandbox), model=model)

    print("\n=== Final answer ===")
    print(result.output)


if __name__ == "__main__":
    main()
