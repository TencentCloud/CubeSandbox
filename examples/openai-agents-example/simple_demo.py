"""
Minimal demo: OpenAI Agents SDK + CubeAPI (E2B-compatible).

CubeAPI provides an E2B-compatible API, so we use E2BSandboxClient directly —
no client code changes needed, just point E2B_API_URL to the CubeAPI service.

Usage:
    cp .env.example .env   # fill in real values
    pip install -r requirements.txt

    # Basic agent demo
    python simple_demo.py
    python simple_demo.py --question "What Linux distro is this?"

    # Pause / Resume demo
    python simple_demo.py --pause-resume

    # Test without any custom SSL handling (use raw defaults)
    python simple_demo.py --no-ssl-patch

    # Test with SSL_CERT_FILE injected but also used for LLM calls
    python simple_demo.py --llm-cube-ssl
"""

import argparse
import asyncio
import functools
import io
import os
import time
from pathlib import Path

os.environ.setdefault("OPENAI_AGENTS_DISABLE_TRACING", "1")

from dotenv import load_dotenv

# ---------------------------------------------------------------------------
# cube-sandbox envd compatibility patches:
# 1. envd only supports "root" user; E2B SDK defaults to "user"
# 2. envd 0.2.0 doesn't support the `stdin` kwarg in commands.run()
# ---------------------------------------------------------------------------
import e2b.envd.rpc as _e2b_rpc
from e2b.sandbox_async.filesystem.filesystem import Filesystem as _AsyncFS
from e2b.sandbox_async.commands.command import Commands as _AsyncCommands

_e2b_rpc.default_username = "root"

import inspect as _inspect

for _name in ("read", "write", "write_files", "list", "exists",
              "get_info", "remove", "rename", "make_dir", "watch_dir"):
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

_orig_commands_run = _AsyncCommands.run

@functools.wraps(_orig_commands_run)
async def _patched_commands_run(self, *a, **kw):
    if hasattr(self, "_envd_version"):
        from e2b.sandbox_async.commands.command import ENVD_COMMANDS_STDIN
        if self._envd_version < ENVD_COMMANDS_STDIN:
            kw.pop("stdin", None)
    return await _orig_commands_run(self, *a, **kw)

_AsyncCommands.run = _patched_commands_run

from agents import ModelSettings, Runner, set_tracing_disabled

set_tracing_disabled(True)

import httpx
from openai import AsyncOpenAI
from agents.models.openai_chatcompletions import OpenAIChatCompletionsModel
from agents.run import RunConfig
from agents.sandbox import Manifest, SandboxAgent, SandboxRunConfig
from agents.sandbox.capabilities import Shell
from agents.extensions.sandbox import (
    E2BSandboxClient,
    E2BSandboxClientOptions,
    E2BSandboxType,
)


def load_env(need_llm: bool = True, ssl_patch: bool = True):
    load_dotenv()
    if os.environ.get("TOKENHUB_API_KEY") and not os.environ.get("OPENAI_API_KEY"):
        os.environ["OPENAI_API_KEY"] = os.environ["TOKENHUB_API_KEY"]
    if not os.environ.get("OPENAI_BASE_URL"):
        os.environ["OPENAI_BASE_URL"] = "https://tokenhub.tencentmaas.com/v1"

    required = ("E2B_API_KEY", "E2B_API_URL")
    if need_llm:
        required = ("OPENAI_API_KEY", *required)
    for key in required:
        if not os.environ.get(key):
            raise SystemExit(f"Missing env var: {key}")

    if ssl_patch:
        cube_ssl = os.environ.get("CUBE_SSL_CERT_FILE")
        if cube_ssl and os.path.isfile(cube_ssl):
            os.environ["SSL_CERT_FILE"] = cube_ssl
            print(f"[ssl] SSL_CERT_FILE={cube_ssl}")
    else:
        print("[ssl] skipped — no custom cert handling")


def make_model(model_name: str, ssl_patch: bool = True, llm_ssl_override: bool = True) -> OpenAIChatCompletionsModel:
    """TokenHub only supports Chat Completions API, not the Responses API.

    ssl_patch=True + llm_ssl_override=True (default):
        Force the LLM http client to use the system CA bundle so that
        SSL_CERT_FILE (set for cube gRPC) doesn't break TokenHub HTTPS calls.
    ssl_patch=True + llm_ssl_override=False (--llm-cube-ssl):
        SSL_CERT_FILE is still injected for cube gRPC, but the LLM client
        also uses it — tests whether cube's cert is trusted for LLM calls too.
    ssl_patch=False (--no-ssl-patch):
        No SSL customisation at all; use raw httpx defaults.
    """
    if ssl_patch and llm_ssl_override:
        import ssl
        ssl_ctx = ssl.create_default_context()
        client = AsyncOpenAI(
            timeout=httpx.Timeout(120, connect=15),
            http_client=httpx.AsyncClient(verify=ssl_ctx),
        )
        print("[ssl] LLM client: system CA bundle (SSL_CERT_FILE isolated)")
    else:
        client = AsyncOpenAI(timeout=httpx.Timeout(120, connect=15))
        if ssl_patch:
            print("[ssl] LLM client: httpx default (will use SSL_CERT_FILE if set)")
        else:
            print("[ssl] LLM client: httpx raw default (no SSL customisation)")
    bare = model_name.split("/", 1)[-1] if "/" in model_name else model_name
    return OpenAIChatCompletionsModel(model=bare, openai_client=client)


# ---------------------------------------------------------------------------
# Demo 1: Basic agent run
# ---------------------------------------------------------------------------
async def run_agent(model: str, question: str, template: str, timeout: int,
                    ssl_patch: bool = True, llm_ssl_override: bool = True):
    print(f"Model:    {model}")
    print(f"Template: {template}")
    print(f"CubeAPI:  {os.environ['E2B_API_URL']}")
    print(f"Question: {question}\n")

    agent = SandboxAgent(
        name="Cube Demo Agent",
        model=make_model(model, ssl_patch=ssl_patch, llm_ssl_override=llm_ssl_override),
        instructions=(
            "You are a helpful assistant running inside a cloud sandbox. "
            "Use shell commands to explore the environment and answer questions. "
            "Be concise."
        ),
        default_manifest=Manifest(),
        capabilities=[Shell()],
        model_settings=ModelSettings(tool_choice="auto"),
    )

    run_config = RunConfig(
        sandbox=SandboxRunConfig(
            client=E2BSandboxClient(),
            options=E2BSandboxClientOptions(
                sandbox_type=E2BSandboxType.E2B,
                template=template,
                timeout=timeout,
            ),
        ),
        workflow_name="Cube simple demo",
    )

    print("Creating sandbox & running agent ...")
    result = await Runner.run(agent, question, run_config=run_config)
    print(f"\n{'='*60}")
    print(result.final_output)


# ---------------------------------------------------------------------------
# Demo 2: Pause / Resume — verify sandbox state survives a stop/resume cycle
# ---------------------------------------------------------------------------
MARKER_PATH = Path("pause-resume-test.txt")
MARKER_CONTENT = "cube sandbox pause/resume works!\n"


async def run_pause_resume(template: str, timeout: int):
    """Create sandbox → write file → pause → resume → verify file persists."""
    print(f"Template: {template}")
    print(f"CubeAPI:  {os.environ['E2B_API_URL']}")
    print()

    client = E2BSandboxClient()
    options = E2BSandboxClientOptions(
        sandbox_type=E2BSandboxType.E2B,
        template=template,
        timeout=timeout,
        pause_on_exit=True,
    )

    # Step 1: Create sandbox and write a marker file
    print("[step 1] Creating sandbox ...")
    t0 = time.monotonic()
    session = await client.create(options=options, manifest=Manifest())
    print(f"         Created in {(time.monotonic()-t0)*1000:.0f} ms  (sandbox_id={session.state.sandbox_id})")

    t1 = time.monotonic()
    await session.start()
    print(f"         Started in {(time.monotonic()-t1)*1000:.0f} ms")

    # Write marker file
    print(f"[step 2] Writing marker file: {MARKER_PATH}")
    await session.write(MARKER_PATH, io.BytesIO(MARKER_CONTENT.encode("utf-8")))

    result = await session.exec(f"cat {MARKER_PATH}")
    stdout = result.stdout if hasattr(result, "stdout") else str(result)
    if isinstance(stdout, bytes):
        stdout = stdout.decode("utf-8")
    print(f"         Content before pause: {stdout.strip()!r}")

    # Step 3: Pause (stop + shutdown with pause_on_exit=True)
    print("[step 3] Pausing sandbox ...")
    t2 = time.monotonic()
    saved_state = session.state
    await session.stop()
    await session.shutdown()
    print(f"         Paused in {(time.monotonic()-t2)*1000:.0f} ms")

    # Step 4: Resume
    print("[step 4] Resuming sandbox ...")
    t3 = time.monotonic()
    resumed_session = await client.resume(saved_state)
    await resumed_session.start()
    print(f"         Resumed in {(time.monotonic()-t3)*1000:.0f} ms")

    # Step 5: Verify marker file persists
    print(f"[step 5] Reading marker file after resume: {MARKER_PATH}")
    try:
        restored = await resumed_session.read(MARKER_PATH)
        restored_text = restored.read()
        if isinstance(restored_text, bytes):
            restored_text = restored_text.decode("utf-8")

        if restored_text.strip() == MARKER_CONTENT.strip():
            print(f"         Content after resume:  {restored_text.strip()!r}")
            print(f"\n{'='*60}")
            print("PASS: Pause/Resume round-trip succeeded!")
            print(f"      File content preserved across pause/resume cycle.")
        else:
            print(f"         Expected: {MARKER_CONTENT.strip()!r}")
            print(f"         Got:      {restored_text.strip()!r}")
            print(f"\n{'='*60}")
            print("FAIL: Content mismatch after resume.")
    except Exception as e:
        print(f"         Error reading file: {e}")
        print(f"\n{'='*60}")
        print("FAIL: Could not read marker file after resume.")
    finally:
        # Cleanup: kill (not pause) the sandbox
        print("\n[cleanup] Destroying sandbox ...")
        resumed_session.state.pause_on_exit = False
        t4 = time.monotonic()
        await resumed_session.shutdown()
        print(f"          Done in {(time.monotonic()-t4)*1000:.0f} ms")

    total = (time.monotonic() - t0) * 1000
    print(f"          Total: {total:.0f} ms")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description="Minimal OpenAI Agents + CubeAPI demo")
    parser.add_argument("--model", default="openai/glm-5.1")
    parser.add_argument("--question", default="What OS is running? Show uname and the first 3 lines of /etc/os-release.")
    parser.add_argument("--template", default=None, help="Cube template ID (or set CUBE_TEMPLATE_ID)")
    parser.add_argument("--timeout", type=int, default=300, help="Sandbox timeout in seconds")
    parser.add_argument("--pause-resume", action="store_true", dest="pause_resume",
                        help="Run pause/resume demo instead of agent demo")
    parser.add_argument("--no-ssl-patch", action="store_false", dest="ssl_patch",
                        default=True,
                        help="Disable custom SSL handling (no SSL_CERT_FILE override, "
                             "no system-CA LLM client) — use raw defaults")
    parser.add_argument("--llm-cube-ssl", action="store_false", dest="llm_ssl_override",
                        default=True,
                        help="Let LLM client use SSL_CERT_FILE (cube cert) instead of "
                             "the system CA bundle — tests cube cert for TokenHub calls")
    args = parser.parse_args()

    load_env(need_llm=not args.pause_resume, ssl_patch=args.ssl_patch)

    template = args.template or os.environ.get("CUBE_TEMPLATE_ID")
    if not template:
        raise SystemExit("Missing template: set CUBE_TEMPLATE_ID or pass --template")

    if args.pause_resume:
        asyncio.run(run_pause_resume(template=template, timeout=args.timeout))
    else:
        asyncio.run(run_agent(
            ssl_patch=args.ssl_patch,
            llm_ssl_override=args.llm_ssl_override,
            model=args.model,
            question=args.question,
            template=template,
            timeout=args.timeout,
        ))
