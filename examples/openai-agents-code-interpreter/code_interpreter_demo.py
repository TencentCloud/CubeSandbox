"""Cube-sandbox code-interpreter demo: sales data analysis with matplotlib.

Ports the E2B code-interpreter example to Cube Sandbox (E2B-compatible API).

What it shows
-------------
- Uses a Cube template with pandas / numpy / matplotlib / scikit-learn preinstalled.
- Exposes two custom capabilities:
    * `WorkspaceShell` — a thin wrapper around `session.exec` for `ls` / `cat`.
    * `PythonRunner`   — writes the model-generated snippet to disk and runs it
                         via `python -I -B script.py`, then reports the new files
                         created under `output/`.
- Seeds the workspace via a Manifest (`sales.csv` + `README.md`).
- Asks the model to compute monthly revenue, draw a line chart, and produce a
  top-3 product table.

Usage
-----
    cp .env.example .env   # fill in real values
    pip install -r requirements.txt
    python code_interpreter_demo.py

Key env vars (see `.env.example`):
    TOKENHUB_API_KEY   — LLM key (OpenAI-compatible via TokenHub)
    E2B_API_URL        — CubeAPI endpoint, e.g. http://<ip>:3000
    E2B_API_KEY        — CubeAPI key
    CUBE_TEMPLATE_ID   — template ID with Python data-science stack preinstalled
    CUBE_SSL_CERT_FILE — optional path to CA bundle for cube HTTPS
"""

from __future__ import annotations

import argparse
import asyncio
import contextvars
import functools
import io
import json
import os
import sys
import uuid
from pathlib import Path
from textwrap import dedent
from typing import Any

os.environ.setdefault("OPENAI_AGENTS_DISABLE_TRACING", "1")

from dotenv import load_dotenv

# ---------------------------------------------------------------------------
# cube-sandbox runtime patches:
# 1. envd only serves the "root" user; the E2B SDK defaults to "user".
#    Force all FS ops to user="root" by wrapping Filesystem methods.
# 2. Inject on_stdout / on_stderr callbacks into commands.run so tool output
#    streams to the local terminal in real time (prefixed per tool label).
# ---------------------------------------------------------------------------
import inspect as _inspect

import e2b.envd.rpc as _e2b_rpc
from e2b.sandbox_async.commands.command import Commands as _AsyncCommands
from e2b.sandbox_async.filesystem.filesystem import Filesystem as _AsyncFS

_e2b_rpc.default_username = "root"

for _name in (
    "read", "write", "write_files", "list", "exists",
    "get_info", "remove", "rename", "make_dir", "watch_dir",
):
    _orig = getattr(_AsyncFS, _name, None)
    if _orig is None:
        continue
    _params = list(_inspect.signature(_orig).parameters.keys())
    _user_pos = _params.index("user") - 1 if "user" in _params else None  # -1 for self

    def _make(fn, user_pos=_user_pos):
        @functools.wraps(fn)
        async def _wrapper(self, *a, **kw):
            if user_pos is not None and len(a) > user_pos:
                a = list(a)
                if a[user_pos] is None:
                    a[user_pos] = "root"
                a = tuple(a)
            else:
                kw.setdefault("user", "root")
            return await fn(self, *a, **kw)
        return _wrapper
    setattr(_AsyncFS, _name, _make(_orig))

# Live-streaming context: tools set _stream_label.set("shell" | "python") and
# every commands.run call inside that context gets on_stdout/on_stderr hooks
# that mirror output to the local terminal with a `[<label>]` prefix.
_stream_label: contextvars.ContextVar[str | None] = contextvars.ContextVar(
    "_stream_label", default=None
)


def _make_stream_handler(label: str, stream):
    def _handler(data):
        text = data if isinstance(data, str) else data.decode("utf-8", errors="replace")
        for line in text.splitlines():
            print(f"[{label}] {line}", file=stream, flush=True)
    return _handler


_orig_commands_run = _AsyncCommands.run


@functools.wraps(_orig_commands_run)
async def _patched_commands_run(self, *a, **kw):
    label = _stream_label.get()
    if label is not None:
        kw.setdefault("on_stdout", _make_stream_handler(label, sys.stdout))
        kw.setdefault("on_stderr", _make_stream_handler(label, sys.stderr))
    return await _orig_commands_run(self, *a, **kw)


_AsyncCommands.run = _patched_commands_run

# ---------------------------------------------------------------------------
# SDK imports (after compat patches)
# ---------------------------------------------------------------------------
import httpx
from openai import AsyncOpenAI
from pydantic import BaseModel, Field

from agents import ModelSettings, Runner, set_tracing_disabled
from agents.models.openai_chatcompletions import OpenAIChatCompletionsModel
from agents.run import RunConfig
from agents.sandbox import Capability, Manifest, SandboxAgent, SandboxRunConfig
from agents.sandbox.entries import File
from agents.sandbox.session.base_sandbox_session import BaseSandboxSession
from agents.tool import FunctionTool, Tool

try:
    from agents.extensions.sandbox import (
        E2BSandboxClient,
        E2BSandboxClientOptions,
        E2BSandboxType,
    )
except Exception as exc:  # pragma: no cover
    raise SystemExit(
        "This example requires the E2B optional dependency:\n"
        "    pip install 'openai-agents[e2b]'"
    ) from exc

set_tracing_disabled(True)


# ---------------------------------------------------------------------------
# Sample data & task
# ---------------------------------------------------------------------------

SALES_CSV = """date,product,units,unit_price
2025-01-03,Alpha Widget,12,19.90
2025-01-17,Beta Gadget,8,49.00
2025-01-29,Gamma Gizmo,5,99.00
2025-02-05,Alpha Widget,22,19.90
2025-02-14,Beta Gadget,15,49.00
2025-02-20,Delta Doohickey,3,129.00
2025-03-04,Alpha Widget,17,19.90
2025-03-11,Beta Gadget,9,49.00
2025-03-18,Gamma Gizmo,7,99.00
2025-03-27,Delta Doohickey,4,129.00
2025-04-02,Alpha Widget,25,19.90
2025-04-08,Beta Gadget,12,49.00
2025-04-19,Gamma Gizmo,10,99.00
2025-04-28,Delta Doohickey,2,129.00
"""

DEFAULT_QUESTION = (
    "Analyze sales.csv with Python: "
    "(1) compute revenue = units * unit_price, "
    "(2) plot monthly total revenue as a line chart and save it to "
    "output/monthly_revenue.png, "
    "(3) report the top-3 products by total revenue as a Markdown table, "
    "(4) finish with a 2-sentence executive summary. "
    "Prefer one consolidated run_python call."
)


# ---------------------------------------------------------------------------
# Capability 1: lightweight shell for ls / cat / pwd
#
# NOTE: We use a plain FunctionTool instead of ShellTool. ShellTool is a
# "hosted tool" that requires the OpenAI Responses API. TokenHub (and most
# 3rd-party OpenAI-compatible providers) only speak the Chat Completions API,
# which rejects hosted tools.
# ---------------------------------------------------------------------------
class ShellRunRequest(BaseModel):
    command: str = Field(
        ...,
        description="Shell command to run inside the sandbox (executed via `sh -lc`).",
    )
    timeout_s: int = Field(
        60,
        ge=1,
        le=600,
        description="Command timeout in seconds (default 60, max 600).",
    )


class WorkspaceShell(Capability):
    """Expose the sandbox session's `exec` as a simple function tool."""

    def __init__(self) -> None:
        super().__init__(type="workspace_shell")

    async def instructions(self, manifest: Manifest) -> str | None:
        _ = manifest
        return (
            "Use the `shell` tool for light inspection (`ls`, `pwd`, `cat`, `head`).\n"
            "The workspace root is the CWD; prefer relative paths."
        )

    def tools(self) -> list[Tool]:
        return [
            FunctionTool(
                name="shell",
                description=(
                    "Run a shell command inside the sandbox and return exit code, "
                    "stdout, stderr. Intended for quick inspection (`ls`, `cat`, "
                    "`head`) of the workspace."
                ),
                params_json_schema=ShellRunRequest.model_json_schema(),
                on_invoke_tool=self._invoke,
            )
        ]

    async def _invoke(self, _ctx: Any, raw_args: str) -> str:
        session = self.session
        if session is None:
            raise RuntimeError("workspace_shell is not bound to a sandbox session")

        request = ShellRunRequest.model_validate_json(raw_args)
        # Use `sh -lc "<cmd> </dev/null"` with shell=False so the SDK doesn't open
        # stdin on its side and the shell closes stdin on the sandbox side — this
        # avoids hangs when the model invokes commands that accidentally read stdin
        # (e.g. `cat` without args, `grep` without a file).
        print(f"[shell] $ {request.command}", flush=True)
        token = _stream_label.set("shell")
        try:
            result = await session.exec(
                "sh", "-lc", f"{request.command} </dev/null",
                timeout=request.timeout_s,
                shell=False,
            )
        finally:
            _stream_label.reset(token)
        print(f"[shell] (exit={result.exit_code})", flush=True)
        payload = {
            "exit_code": result.exit_code,
            "stdout": result.stdout.decode("utf-8", errors="replace")[-4000:],
            "stderr": result.stderr.decode("utf-8", errors="replace")[-4000:],
        }
        return json.dumps(payload, ensure_ascii=False)


# ---------------------------------------------------------------------------
# Capability 2: run_python — the heart of this demo
# ---------------------------------------------------------------------------
class PythonRunRequest(BaseModel):
    code: str = Field(
        ...,
        description="Full Python source to execute. CWD is the workspace root; use relative paths.",
    )
    timeout_s: int = Field(60, ge=1, le=600)


class PythonRunner(Capability):
    """Write the model-generated Python code to disk, then run it with `python`.

    Requires a Cube template with pandas / numpy / matplotlib / scikit-learn
    preinstalled (typical data-science image).
    """

    def __init__(self) -> None:
        super().__init__(type="python_runner")

    async def instructions(self, manifest: Manifest) -> str | None:
        _ = manifest
        return dedent(
            """
            Call `run_python(code=..., timeout_s=60)` for computation and plotting.
            - The workspace root is the CWD; use relative paths.
            - pandas / numpy / matplotlib / scikit-learn are preinstalled.
            - Save charts to `output/<name>.png` with `plt.savefig(...)`.
            - Use `matplotlib.use("Agg")` before `import pyplot` so plotting works
              in this headless environment.
            - Print concise summaries to stdout for the user to inspect.
            - Prefer one well-structured call over many tiny ones.
            """
        ).strip()

    def tools(self) -> list[Tool]:
        return [
            FunctionTool(
                name="run_python",
                description=(
                    "Execute a Python snippet inside the cube sandbox and return "
                    "exit code, stdout tail, stderr tail, and newly created files "
                    "under `output/`."
                ),
                params_json_schema=PythonRunRequest.model_json_schema(),
                on_invoke_tool=self._invoke,
            )
        ]

    async def _invoke(self, _ctx: Any, raw_args: str) -> str:
        session = self.session
        if session is None:
            raise RuntimeError("python_runner is not bound to a sandbox session")

        request = PythonRunRequest.model_validate_json(raw_args)

        await session.mkdir(Path(".scratch"), parents=True)
        await session.mkdir(Path("output"), parents=True)

        script_rel = Path(f".scratch/{uuid.uuid4().hex}.py")
        await session.write(
            script_rel,
            io.BytesIO(request.code.encode("utf-8")),
        )

        before = await _list_output_files(session)
        print(f"[python] running {script_rel} ({len(request.code)} bytes)", flush=True)
        token = _stream_label.set("python")
        try:
            result = await session.exec(
                "python",
                "-u",
                "-I",
                "-B",
                str(script_rel),
                shell=False,
                timeout=request.timeout_s,
            )
        finally:
            _stream_label.reset(token)
        print(f"[python] (exit={result.exit_code})", flush=True)
        after = await _list_output_files(session)
        new_files = sorted(after - before)

        payload = {
            "exit_code": result.exit_code,
            "stdout_tail": result.stdout.decode("utf-8", errors="replace")[-4000:],
            "stderr_tail": result.stderr.decode("utf-8", errors="replace")[-4000:],
            "new_files": new_files,
        }
        return json.dumps(payload, ensure_ascii=False)


async def _list_output_files(session: BaseSandboxSession) -> set[str]:
    """List files under `output/`, used to diff before/after run_python."""
    result = await session.exec(
        "sh",
        "-lc",
        "find output -maxdepth 2 -type f 2>/dev/null | sort",
        shell=False,
        timeout=10,
    )
    if result.exit_code != 0:
        return set()
    return {
        line.strip()
        for line in result.stdout.decode("utf-8", errors="replace").splitlines()
        if line.strip()
    }


# ---------------------------------------------------------------------------
# Manifest / env / model
# ---------------------------------------------------------------------------
def build_manifest() -> Manifest:
    return Manifest(
        entries={
            "sales.csv": File(content=SALES_CSV.encode("utf-8")),
            "README.md": File(
                content=(
                    b"# Sales review\n\n"
                    b"`sales.csv` has 4 months of per-product sales.\n"
                    b"Use run_python to compute revenue and plot the monthly trend.\n"
                )
            ),
        }
    )


def load_env():
    load_dotenv()
    if os.environ.get("TOKENHUB_API_KEY") and not os.environ.get("OPENAI_API_KEY"):
        os.environ["OPENAI_API_KEY"] = os.environ["TOKENHUB_API_KEY"]
    if not os.environ.get("OPENAI_BASE_URL"):
        os.environ["OPENAI_BASE_URL"] = "https://tokenhub.tencentmaas.com/v1"

    for key in ("OPENAI_API_KEY", "E2B_API_KEY", "E2B_API_URL"):
        if not os.environ.get(key):
            raise SystemExit(f"Missing env var: {key}")

    cube_ssl = os.environ.get("CUBE_SSL_CERT_FILE")
    if cube_ssl and os.path.isfile(cube_ssl):
        os.environ["SSL_CERT_FILE"] = cube_ssl


def make_model(model_name: str) -> OpenAIChatCompletionsModel:
    """TokenHub only supports Chat Completions API, not the Responses API.

    Force the LLM http client to use the system CA bundle so that SSL_CERT_FILE
    (set for cube gRPC) doesn't break TokenHub HTTPS calls.
    """
    import ssl
    ssl_ctx = ssl.create_default_context()
    client = AsyncOpenAI(
        timeout=httpx.Timeout(120, connect=15),
        http_client=httpx.AsyncClient(verify=ssl_ctx),
    )
    bare = model_name.split("/", 1)[-1] if "/" in model_name else model_name
    return OpenAIChatCompletionsModel(model=bare, openai_client=client)


# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
SYSTEM_INSTRUCTIONS = (
    "You are a data analyst working inside a Python sandbox.\n"
    "- Use `run_python` for computation and plotting. Use `shell` only for "
    "quick inspection (`ls`, `cat`, `head`).\n"
    "- pandas / numpy / matplotlib / scikit-learn are preinstalled; no need "
    "to install anything.\n"
    "- Save charts to `output/<name>.png` with `plt.savefig(...)`.\n"
    "- Cite file names you actually read and include any files you produced."
)


async def main(
    *,
    model: str,
    question: str,
    template: str,
    timeout: int,
    pause_on_exit: bool,
) -> None:
    print(f"Model:    {model}")
    print(f"Template: {template}")
    print(f"CubeAPI:  {os.environ['E2B_API_URL']}")
    print(f"Question: {question}\n")

    agent = SandboxAgent(
        name="Cube Code Interpreter Analyst",
        model=make_model(model),
        instructions=SYSTEM_INSTRUCTIONS,
        default_manifest=build_manifest(),
        capabilities=[WorkspaceShell(), PythonRunner()],
        model_settings=ModelSettings(tool_choice="auto"),
    )

    run_config = RunConfig(
        sandbox=SandboxRunConfig(
            client=E2BSandboxClient(),
            options=E2BSandboxClientOptions(
                sandbox_type=E2BSandboxType.E2B,
                template=template,
                timeout=timeout,
                pause_on_exit=pause_on_exit,
            ),
        ),
        workflow_name="Cube code interpreter demo",
    )

    print("Creating sandbox & running agent ...")
    result = await Runner.run(agent, question, run_config=run_config)
    print("=" * 64)
    print(result.final_output)
    print("=" * 64)


def _parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--model", default="openai/glm-5.1",
                        help="LLM name (TokenHub models use 'openai/' prefix).")
    parser.add_argument("--question", default=DEFAULT_QUESTION,
                        help="Prompt to send to the agent.")
    parser.add_argument("--template", default=None,
                        help="Cube template ID (or set CUBE_TEMPLATE_ID).")
    parser.add_argument("--timeout", type=int, default=600,
                        help="Sandbox lifetime timeout in seconds.")
    parser.add_argument("--pause-on-exit", action="store_true", default=False,
                        help="Pause (not kill) the sandbox on shutdown, for later resume.")
    return parser.parse_args()


if __name__ == "__main__":
    args = _parse_args()
    load_env()
    template = args.template or os.environ.get("CUBE_TEMPLATE_ID")
    if not template:
        raise SystemExit("Missing template: set CUBE_TEMPLATE_ID or pass --template")

    asyncio.run(
        main(
            model=args.model,
            question=args.question,
            template=template,
            timeout=args.timeout,
            pause_on_exit=args.pause_on_exit,
        )
    )
