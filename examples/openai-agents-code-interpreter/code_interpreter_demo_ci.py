"""Cube-sandbox code-interpreter demo (Jupyter kernel variant).

This is a simplified re-implementation of ``code_interpreter_demo.py`` that
uses ``E2BSandboxType.CODE_INTERPRETER`` instead of the generic E2B sandbox.

Why another file?
-----------------
``CODE_INTERPRETER`` is backed by ``e2b_code_interpreter.AsyncSandbox`` which
hosts a long-lived Jupyter kernel. That gives us `run_code(...)`, a stateful
execution primitive that returns a rich ``Execution`` object:

    - ``logs.stdout`` / ``logs.stderr`` — captured per cell
    - ``results``                       — display values (PNG/JPEG/SVG/HTML/text)
    - ``error``                         — traceback (if any)

So instead of:
    write snippet to .scratch/*.py  ->  exec `python script.py`
        ->  find new files under output/  ->  stitch everything together

we do:
    session._sandbox.run_code(code)    # stateful kernel
    -> decode any png/jpeg results into output/figure_*.png

Kernel state (variables, imports, `df`, fitted models...) persists across
calls, matching the OpenAI code-interpreter UX.

What stays the same
-------------------
- root-user FS patch (cube envd only serves `root`)
- live-streaming `[shell]` / `[python]` output to the terminal
- `WorkspaceShell` tool for quick `ls` / `cat`
- Manifest seed (`sales.csv`, `README.md`)

Usage
-----
    cp .env.example .env   # fill in real values (same vars as the other demo)
    pip install -r requirements.txt
    python code_interpreter_demo_ci.py

Requires a Cube template with a Jupyter kernel runtime compatible with
``e2b_code_interpreter`` (pandas / numpy / matplotlib preinstalled).
"""

from __future__ import annotations

import argparse
import asyncio
import base64
import contextvars
import functools
import io
import json
import os
import sys
from pathlib import Path
from textwrap import dedent
from typing import Any

os.environ.setdefault("OPENAI_AGENTS_DISABLE_TRACING", "1")

from dotenv import load_dotenv

# ---------------------------------------------------------------------------
# cube-sandbox runtime patches:
# 1. envd only serves the "root" user; E2B SDK defaults to "user". Force all
#    FS ops to user="root" by wrapping Filesystem methods. (Applies to both
#    e2b.AsyncSandbox and e2b_code_interpreter.AsyncSandbox, since the latter
#    inherits the former's Filesystem.)
# 2. Inject on_stdout / on_stderr callbacks into Commands.run so the `shell`
#    tool's output streams to the local terminal in real time. This ONLY
#    affects `session.exec` (which the WorkspaceShell / probe code uses);
#    run_code() talks to the code-interpreter HTTP gateway directly and
#    uses its own on_stdout/on_stderr callbacks in PythonRunner._invoke.
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

try:
    import e2b_code_interpreter as _e2b_ci  # noqa: F401
except Exception as exc:  # pragma: no cover
    raise SystemExit(
        "This demo uses the Jupyter-kernel sandbox and needs "
        "`e2b-code-interpreter`:\n"
        "    pip install e2b-code-interpreter\n"
        "(older versions of openai-agents[e2b] don't pull it in.)"
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
    "(2) plot monthly total revenue as a line chart (the plot will be captured "
    "automatically), "
    "(3) report the top-3 products by total revenue as a Markdown table, "
    "(4) finish with a 2-sentence executive summary. "
    "You can spread this across multiple run_python calls — kernel state is preserved."
)


# ---------------------------------------------------------------------------
# Capability 1: lightweight shell for ls / cat / pwd (unchanged from sibling demo)
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
# Capability 2: run_python backed by the Jupyter kernel (run_code)
# ---------------------------------------------------------------------------
class PythonRunRequest(BaseModel):
    code: str = Field(
        ...,
        description=(
            "Python source to execute inside a stateful Jupyter kernel. "
            "Variables, imports, and dataframes persist across calls."
        ),
    )
    timeout_s: int = Field(60, ge=1, le=600)


# matplotlib image formats we know how to persist
_IMAGE_KINDS: tuple[tuple[str, str], ...] = (
    ("png", "png"),
    ("jpeg", "jpg"),
    ("svg", "svg"),
)


# In the e2b code-interpreter image, port 49999 is the "code interpreter"
# HTTP gateway service (which in turn talks to Jupyter on 8888 internally).
# The constant is still called JUPYTER_PORT in the SDK for historical
# reasons, so we follow that naming here.
_JUPYTER_PORT = 49999  # e2b_code_interpreter.constants.JUPYTER_PORT


async def _probe_jupyter_internal(
    session: Any, *, attempts: int, interval_s: float,
) -> tuple[bool, str]:
    """Probe the code-interpreter HTTP gateway from INSIDE the sandbox.

    Runs one ``session.exec`` per attempt that does:
      1. TCP accept via bash's ``/dev/tcp/127.0.0.1/49999`` (fast check
         that the process is even listening),
      2. an HTTP GET to ``/health`` via python stdlib.

    Any HTTP response — including 4xx via ``HTTPError`` — counts as
    "service is responding"; we only reject socket errors / timeouts
    (i.e. process missing or wedged). This is intentional: 49999 is the
    e2b code-interpreter gateway (POST /contexts, POST /execute, GET
    /health; Jupyter itself sits behind it on 8888), so an endpoint like
    ``/`` or ``/api`` legitimately 404s on a healthy server.
    """
    probe_sh = rf"""
exec 3<>/dev/tcp/127.0.0.1/{_JUPYTER_PORT} 2>/dev/null || {{ echo "TCP_FAIL" >&2; exit 2; }}
exec 3>&- 3<&-
python3 - <<'PY' 2>&1
import urllib.request, urllib.error, sys, socket
socket.setdefaulttimeout(5)
try:
    with urllib.request.urlopen("http://127.0.0.1:{_JUPYTER_PORT}/health") as r:
        body = r.read(300).decode("utf-8", "replace").replace("\n", " ")
        print(f"HTTP_OK status={{r.status}} body[:300]={{body!r}}")
except urllib.error.HTTPError as e:
    # 4xx/5xx still means the HTTP server answered, i.e. not wedged.
    print(f"HTTP_OK status={{e.code}} (HTTPError, server is responsive)")
except Exception as e:
    print(f"HTTP_FAIL {{type(e).__name__}}: {{e}}")
    sys.exit(3)
PY
""".strip()

    last_err = ""
    for i in range(attempts):
        try:
            res = await session.exec(
                "bash", "-lc", probe_sh, shell=False, timeout=10,
            )
            stdout = res.stdout.decode("utf-8", errors="replace").strip()
            stderr = res.stderr.decode("utf-8", errors="replace").strip()
            if res.exit_code == 0 and stdout.startswith("HTTP_OK"):
                return True, f"probe #{i}: {stdout}"
            last_err = (
                f"exit={res.exit_code} stderr={stderr!r} stdout={stdout!r}"
            )
        except Exception as e:
            last_err = f"{type(e).__name__}: {e}"
        await asyncio.sleep(interval_s)
    return False, last_err


async def _run_code_annotated(inner: Any, code: str, **kwargs: Any) -> Any:
    """Call ``inner.run_code`` and, on a cube gateway 502, re-raise with a
    more actionable hint. The e2b SDK wraps 502 responses as a generic
    ``TimeoutException`` with a misleading "sandbox timeout" message; for
    cube deployments that usually means the sandbox was evicted / deleted,
    not that the request timed out.
    """
    try:
        return await inner.run_code(code, **kwargs)
    except Exception as e:
        msg = str(e)
        if "502" in msg or "Bad Gateway" in msg or "openresty" in msg:
            raise RuntimeError(
                "run_code hit a 502 Bad Gateway from cube's gateway. "
                "On cube this most commonly means the sandbox was evicted "
                "(deleted or paused) before the call landed, even though "
                "envd/session.exec may still look alive. "
                "Double-check: sandbox `timeout=` is large enough for the "
                "full run; nothing is calling .kill() / .pause() early; "
                "and your cube cluster isn't evicting on idle."
            ) from e
        raise


async def _wait_for_jupyter_ready(
    session: Any,
    *,
    attempts: int = 10,
    interval_s: float = 0.5,
) -> None:
    """Confirm the code-interpreter gateway is up and answering HTTP INSIDE
    the sandbox before letting ``run_code`` touch it through the cube
    gateway.

    We intentionally don't probe the cube public URL ourselves — an
    unauthenticated GET wouldn't carry the ``X-Access-Token`` /
    ``E2B-Traffic-Access-Token`` headers the SDK sends, and might be
    rejected by the gateway for that reason alone, producing a false
    positive. If ``run_code`` subsequently 502s despite this probe
    passing, the most likely cause is cube having evicted/deleted the
    sandbox (despite envd still looking alive).
    """
    ok, detail = await _probe_jupyter_internal(
        session, attempts=attempts, interval_s=interval_s,
    )
    if not ok:
        raise RuntimeError(
            f"code-interpreter gateway on 127.0.0.1:{_JUPYTER_PORT} inside "
            f"the sandbox is not answering HTTP after {attempts} attempts "
            f"(last probe: {detail}). "
            "Either code-interpreter did not start (check the template's "
            "boot script / supervisord config) or the jupyter process is "
            "wedged."
        )
    print(f"[python] code-interpreter ready: {detail}", flush=True)


def _find_code_interpreter_sandbox(session: Any, *, max_depth: int = 4) -> Any | None:
    """Locate the underlying `e2b_code_interpreter.AsyncSandbox` on the session.

    Different openai-agents versions stack several wrappers before the raw
    sandbox:
        SandboxSession._inner -> E2BSandboxSession._sandbox -> AsyncSandbox

    So we BFS through a handful of known attribute names (and any object
    attributes from __dict__) looking for something that exposes a callable
    `run_code`. Limited to `max_depth` to avoid pathological cycles.
    """
    if session is None:
        return None

    preferred = ("_sandbox", "sandbox", "_inner", "inner", "_client", "_e2b_sandbox")

    queue: list[tuple[Any, int]] = [(session, 0)]
    seen: set[int] = {id(session)}

    while queue:
        obj, depth = queue.pop(0)

        fn = getattr(obj, "run_code", None)
        if callable(fn) and obj is not session:
            return obj

        if depth >= max_depth:
            continue

        next_objs: list[Any] = []
        for name in preferred:
            child = getattr(obj, name, None)
            if child is not None:
                next_objs.append(child)

        try:
            next_objs.extend(vars(obj).values())
        except TypeError:
            pass

        for child in next_objs:
            if child is None or id(child) in seen:
                continue
            if isinstance(child, (str, bytes, int, float, bool)) or child is type(None):
                continue
            seen.add(id(child))
            queue.append((child, depth + 1))

    return None


class PythonRunner(Capability):
    """Run Python in the sandbox's Jupyter kernel and capture rich outputs.

    Uses ``e2b_code_interpreter.AsyncSandbox.run_code``, which we locate on
    the ``SandboxSession`` via :func:`_find_code_interpreter_sandbox` (BFS
    over the SDK's wrapper layers). The kernel is long-lived for the
    session, so subsequent calls share state.

    Image results returned by the kernel (e.g. matplotlib plots rendered by a
    bare ``plt.show()`` / last-line ``fig`` cell output) are decoded from
    base64 and written to ``output/figure_<n>.<ext>`` in the workspace.
    """

    def __init__(self) -> None:
        super().__init__(type="python_runner")
        self._figure_counter = 0
        self._kernel_bootstrapped = False

    async def instructions(self, manifest: Manifest) -> str | None:
        entries = sorted((manifest.entries or {}).keys())
        seeded = ", ".join(f"`{e}`" for e in entries) if entries else "(none)"
        return dedent(
            f"""
            Call `run_python(code=..., timeout_s=60)` to execute Python.
            - A stateful Jupyter kernel is used: variables, imports, and
              dataframes persist across calls. Feel free to split work
              across multiple small cells.
            - The CWD is the sandbox workspace root and already contains the
              seeded files: {seeded}. Read them with bare relative paths
              (e.g. `pd.read_csv("sales.csv")`); do NOT invent prefixes like
              `./data/`, `/mnt/data/`, `/workspace/`.
            - pandas / numpy / matplotlib are preinstalled.
            - For charts, end a cell with `plt.show()` (or leave a Figure as
              the last expression). The plot is captured automatically and
              saved to `output/figure_*.png`; no need to call `plt.savefig`.
            - Print concise summaries; avoid dumping giant tables.
            """
        ).strip()

    def tools(self) -> list[Tool]:
        return [
            FunctionTool(
                name="run_python",
                description=(
                    "Execute Python in the sandbox's persistent Jupyter kernel "
                    "and return stdout, stderr, any error traceback, and the "
                    "names of chart/image files written to `output/`."
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

        # Reach into the underlying e2b_code_interpreter.AsyncSandbox. The
        # agents-SDK wrapper normalizes to exec/read/write, so we bypass it
        # here to access run_code() and its Execution result.
        inner = _find_code_interpreter_sandbox(session)
        if inner is None:
            def _describe(obj: Any, depth: int = 0) -> str:
                if depth > 3 or obj is None:
                    return f"{type(obj).__name__}"
                try:
                    attrs = sorted(vars(obj).keys())
                except TypeError:
                    attrs = []
                parts = [f"{type(obj).__name__}[{','.join(attrs)}]"]
                for name in ("_inner", "_sandbox", "sandbox"):
                    nxt = getattr(obj, name, None)
                    if nxt is not None and nxt is not obj:
                        parts.append(f"{name}={_describe(nxt, depth + 1)}")
                return " ".join(parts)
            raise RuntimeError(
                "Could not locate an inner sandbox exposing run_code(). "
                f"session tree: {_describe(session)}. "
                "Check that sandbox_type=E2BSandboxType.CODE_INTERPRETER and "
                "that `e2b-code-interpreter` is installed."
            )

        await session.mkdir(Path("output"), parents=True)

        # One OutputMessage can carry multiple lines (e.g. `print(df.dtypes)`
        # delivers a single message whose `.line` has embedded newlines), so
        # split and re-prefix each line to keep the `[python]` tag consistent.
        def _emit(msg, stream):
            text = getattr(msg, "line", None)
            if text is None:
                text = str(msg)
            for line in text.splitlines() or [""]:
                print(f"[python] {line}", file=stream, flush=True)

        def _on_stdout(msg):  # msg: e2b_code_interpreter.OutputMessage
            _emit(msg, sys.stdout)

        def _on_stderr(msg):
            _emit(msg, sys.stderr)

        if not self._kernel_bootstrapped:
            # 1. Make sure Jupyter is up and answering HTTP inside the
            #    sandbox before run_code touches it through the gateway.
            await _wait_for_jupyter_ready(session)

            # 2. The Jupyter kernel's default CWD (e.g. /home/user) is not
            #    guaranteed to match the envd workspace root where Manifest
            #    files were seeded. Discover the envd CWD via exec("pwd")
            #    and chdir the kernel there so relative paths like
            #    "sales.csv" resolve to the seeded file.
            pwd_res = await session.exec(
                "sh", "-lc", "pwd", shell=False, timeout=10,
            )
            ws_root = pwd_res.stdout.decode("utf-8", errors="replace").strip() or "/"
            boot = f"import os as _os\n_os.chdir({ws_root!r})\n"
            await _run_code_annotated(inner, boot, timeout=10)
            print(f"[python] bootstrapped kernel cwd -> {ws_root}", flush=True)
            self._kernel_bootstrapped = True

        print(f"[python] running {len(request.code)} bytes in kernel", flush=True)

        execution = await _run_code_annotated(
            inner,
            request.code,
            on_stdout=_on_stdout,
            on_stderr=_on_stderr,
            timeout=request.timeout_s,
        )

        saved_files: list[str] = []
        for idx, result in enumerate(execution.results):
            for kind, ext in _IMAGE_KINDS:
                encoded = getattr(result, kind, None)
                if not encoded:
                    continue
                self._figure_counter += 1
                rel_path = Path(f"output/figure_{self._figure_counter:02d}.{ext}")
                try:
                    if kind == "svg":
                        blob = encoded.encode("utf-8")
                    else:
                        blob = base64.b64decode(encoded)
                except (ValueError, TypeError, base64.binascii.Error):
                    continue
                await session.write(rel_path, io.BytesIO(blob))
                saved_files.append(str(rel_path))
                print(f"[python] saved {rel_path} ({len(blob)} bytes)", flush=True)
                # one image per result is enough; don't write png+jpeg of the same plot
                break

        err_payload: dict[str, str] | None = None
        if execution.error is not None:
            err_payload = {
                "name": execution.error.name,
                "value": execution.error.value,
                "traceback": execution.error.traceback[-2000:],
            }
            print(f"[python] error: {execution.error.name}: {execution.error.value}", flush=True)
        else:
            print("[python] (ok)", flush=True)

        stdout_tail = "".join(execution.logs.stdout)[-4000:]
        stderr_tail = "".join(execution.logs.stderr)[-4000:]
        text_result = execution.text  # last-expression text repr, if any

        payload = {
            "ok": execution.error is None,
            "stdout_tail": stdout_tail,
            "stderr_tail": stderr_tail,
            "text_result": text_result,
            "saved_figures": saved_files,
            "error": err_payload,
        }
        return json.dumps(payload, ensure_ascii=False)


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
    "You are a data analyst working inside a stateful Python kernel.\n"
    "- Use `run_python` for computation and plotting. Variables persist across "
    "calls, so you can build up the analysis step by step.\n"
    "- Use `shell` only for quick inspection (`ls`, `cat`, `head`).\n"
    "- pandas / numpy / matplotlib are preinstalled; no installs needed.\n"
    "- For charts, call `plt.show()` at the end of the cell — the figure is "
    "captured automatically and saved to `output/figure_*.png`.\n"
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
        name="Cube Code Interpreter Analyst (kernel)",
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
                sandbox_type=E2BSandboxType.CODE_INTERPRETER,
                template=template,
                timeout=timeout,
                pause_on_exit=pause_on_exit,
            ),
        ),
        workflow_name="Cube code interpreter demo (kernel)",
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
