"""
OpenAI Agents SDK + E2B + Cube Sandbox example.

Uses E2BSandboxClient to run a SandboxAgent inside a cube-sandbox instance
with a SWE-bench Django image. The agent explores the Django codebase,
analyzes a bug, and proposes a fix.

LLM API uses TokenHub (OpenAI-compatible).
The sandbox template is built from:
  cube-sandbox-image.tencentcloudcr.com/demo/django_1776_django-13447:latest

Usage:
    cp .env.example .env   # fill in real values
    pip install -r requirements.txt
    python main.py
    python main.py --question "What Python version is installed?"
    python main.py --model openai/glm-5.1
    python main.py --sandbox-only              # just create & destroy sandbox
    python main.py --sandbox-only --timeout 60 # with custom timeout
"""

import argparse
import asyncio
import functools
import os
import time

os.environ.setdefault("OPENAI_AGENTS_DISABLE_TRACING", "1")

from dotenv import load_dotenv

# ---------------------------------------------------------------------------
# cube-sandbox envd compatibility patches:
# 1. envd only supports "root" user; E2B SDK defaults to "user"
# 2. envd 0.2.0 doesn't support the `stdin` kwarg in commands.run()
# ---------------------------------------------------------------------------
import e2b.envd.rpc as _e2b_rpc  # noqa: E402
from e2b.sandbox_async.filesystem.filesystem import Filesystem as _AsyncFS  # noqa: E402
from e2b.sandbox_async.commands.command import Commands as _AsyncCommands  # noqa: E402

_e2b_rpc.default_username = "root"

import inspect as _inspect

for _name in ("read", "write", "write_files", "list", "exists",
              "get_info", "remove", "rename", "make_dir", "watch_dir"):
    _orig = getattr(_AsyncFS, _name, None)
    if _orig is None:
        continue
    _params = list(_inspect.signature(_orig).parameters.keys())
    _user_pos = _params.index("user") - 1 if "user" in _params else None  # -1 for self

    def _make(fn, user_pos=_user_pos):  # noqa: E301
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
# ---------------------------------------------------------------------------

from agents import ModelSettings, Runner, set_tracing_disabled

set_tracing_disabled(True)

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

# ---------------------------------------------------------------------------
# Wrap E2BSandboxClient lifecycle methods to log timing
# ---------------------------------------------------------------------------
_orig_e2b_create = E2BSandboxClient.create
_orig_e2b_session_cls = None

async def _traced_create(self, *, options, manifest, **kw):
    print(f"  [e2b] create() called, template={getattr(options, 'template', '?')}", flush=True)
    t0 = time.monotonic()
    session = await _orig_e2b_create(self, options=options, manifest=manifest, **kw)
    ms = (time.monotonic() - t0) * 1000
    print(f"  [e2b] create() done in {ms:.0f} ms", flush=True)

    # Wrap all session lifecycle methods
    def _wrap(name):
        orig = getattr(session, name)
        async def _traced(*a, **k):
            print(f"  [e2b] session.{name}() ...", flush=True)
            t = time.monotonic()
            try:
                r = await orig(*a, **k)
                print(f"  [e2b] session.{name}() done in {(time.monotonic()-t)*1000:.0f} ms", flush=True)
                return r
            except Exception as e:
                print(f"  [e2b] session.{name}() FAILED in {(time.monotonic()-t)*1000:.0f} ms: {e}", flush=True)
                raise
        setattr(session, name, _traced)

    for _m in ("start", "exec", "stop", "shutdown", "running",
               "run_pre_stop_hooks"):
        if hasattr(session, _m):
            _wrap(_m)
    return session

E2BSandboxClient.create = _traced_create

# Wrap OpenAIChatCompletionsModel to trace LLM calls.
# stream_response is an ASYNC GENERATOR (yields events), not a regular coroutine.
_orig_stream_response = OpenAIChatCompletionsModel.stream_response

async def _traced_stream_response(self, *a, **kw):
    print("  [model] stream_response() called ...", flush=True)
    t0 = time.monotonic()
    first_event = True
    try:
        async for event in _orig_stream_response(self, *a, **kw):
            if first_event:
                ms = (time.monotonic() - t0) * 1000
                print(f"  [model] first event in {ms:.0f} ms", flush=True)
                first_event = False
            yield event
        ms = (time.monotonic() - t0) * 1000
        print(f"  [model] stream_response() complete ({ms:.0f} ms)", flush=True)
    except Exception as e:
        ms = (time.monotonic() - t0) * 1000
        print(f"  [model] stream_response() FAILED in {ms:.0f} ms: {e}", flush=True)
        raise

OpenAIChatCompletionsModel.stream_response = _traced_stream_response
# ---------------------------------------------------------------------------

DJANGO_BUG_DESCRIPTION = """\
Bug: django__django-13447

When a model admin class has `list_display` containing a field name and a
`list_display_links` set to `None`, Django should display the fields as
plain text (not clickable). However, the current behavior still generates
a link for the first field.

The relevant code is in `django/contrib/admin/templatetags/admin_list.py`,
specifically in the `items_for_result` function.
"""


def load_env(sandbox_only: bool = False):
    load_dotenv()

    if os.environ.get("TOKENHUB_API_KEY") and not os.environ.get("OPENAI_API_KEY"):
        os.environ["OPENAI_API_KEY"] = os.environ["TOKENHUB_API_KEY"]
    if not os.environ.get("OPENAI_BASE_URL"):
        os.environ["OPENAI_BASE_URL"] = "https://tokenhub.tencentmaas.com/v1"

    required = ("E2B_API_KEY", "E2B_API_URL")
    if not sandbox_only:
        required = ("OPENAI_API_KEY", *required)
    for key in required:
        if not os.environ.get(key):
            raise SystemExit(f"Missing required env var: {key}")

    cube_ssl = os.environ.get("CUBE_SSL_CERT_FILE")
    if cube_ssl and os.path.isfile(cube_ssl):
        os.environ["SSL_CERT_FILE"] = cube_ssl
        print(f"[env] SSL_CERT_FILE={cube_ssl} (for cube-sandbox only)")


_req_timers: dict[str, float] = {}


def _llm_http_client(**kwargs):
    """Build an httpx.AsyncClient that ignores CUBE_SSL_CERT_FILE.

    SSL_CERT_FILE is set globally for cube-sandbox gRPC, but TokenHub
    uses public CAs.  We must NOT let the custom cert break LLM calls.
    Includes request/response event hooks for debugging.
    """
    import httpx
    import ssl
    ssl_ctx = ssl.create_default_context()  # system CA bundle

    async def _on_request(request: httpx.Request):
        _req_timers[str(id(request))] = time.monotonic()
        body_len = len(request.content) if request.content else 0
        print(f"  [http] → {request.method} {request.url} ({body_len} bytes)", flush=True)

    async def _on_response(response: httpx.Response):
        t0 = _req_timers.pop(str(id(response.request)), time.monotonic())
        ms = (time.monotonic() - t0) * 1000
        print(f"  [http] ← {response.status_code} ({ms:.0f} ms)", flush=True)

    return httpx.AsyncClient(
        verify=ssl_ctx,
        event_hooks={"request": [_on_request], "response": [_on_response]},
        **kwargs,
    )


def _make_chat_model(model_name: str) -> OpenAIChatCompletionsModel:
    """Build a Chat Completions model adapter.

    TokenHub (and most OpenAI-compatible providers) only support the
    Chat Completions API, NOT the Responses API.  When a plain string
    is passed to SandboxAgent, the SDK defaults to the Responses API
    (POST /v1/responses) which will hang or 404.
    """
    import httpx
    client = AsyncOpenAI(
        timeout=httpx.Timeout(120, connect=15),
        http_client=_llm_http_client(),
    )
    bare_name = model_name.split("/", 1)[-1] if "/" in model_name else model_name
    return OpenAIChatCompletionsModel(model=bare_name, openai_client=client)


async def _preflight_llm(model_name: str) -> None:
    """Verify the LLM is reachable: plain call, then tool-calling + streaming."""
    import httpx
    bare = model_name.split("/", 1)[-1] if "/" in model_name else model_name
    client = AsyncOpenAI(
        timeout=httpx.Timeout(30, connect=10),
        http_client=_llm_http_client(),
    )
    try:
        # 1) plain, non-streaming
        t0 = time.monotonic()
        resp = await client.chat.completions.create(
            model=bare,
            messages=[{"role": "user", "content": "hi"}],
            max_tokens=5,
        )
        ms = (time.monotonic() - t0) * 1000
        text = resp.choices[0].message.content if resp.choices else "(empty)"
        print(f"[preflight] 1/3 plain ok — {bare} @ {client.base_url}  {ms:.0f} ms: {text!r}", flush=True)

        # 2) with tools (function calling)
        test_tools = [{
            "type": "function",
            "function": {
                "name": "get_info",
                "description": "Get system info",
                "parameters": {
                    "type": "object",
                    "properties": {"cmd": {"type": "string"}},
                    "required": ["cmd"],
                },
            },
        }]
        t1 = time.monotonic()
        resp2 = await client.chat.completions.create(
            model=bare,
            messages=[{"role": "user", "content": "Run uname"}],
            tools=test_tools,
            max_tokens=50,
        )
        ms2 = (time.monotonic() - t1) * 1000
        choice = resp2.choices[0] if resp2.choices else None
        if choice and choice.message.tool_calls:
            tc = choice.message.tool_calls[0]
            print(f"[preflight] 2/3 tool-call ok — {ms2:.0f} ms: {tc.function.name}({tc.function.arguments})", flush=True)
        else:
            text2 = choice.message.content if choice else "(empty)"
            print(f"[preflight] 2/3 tool-call ok (no tool used) — {ms2:.0f} ms: {text2!r}", flush=True)

        # 3) streaming
        t2 = time.monotonic()
        stream = await client.chat.completions.create(
            model=bare,
            messages=[{"role": "user", "content": "Say OK"}],
            max_tokens=5,
            stream=True,
        )
        chunks = 0
        async for chunk in stream:
            chunks += 1
        ms3 = (time.monotonic() - t2) * 1000
        print(f"[preflight] 3/3 streaming ok — {ms3:.0f} ms, {chunks} chunks", flush=True)

    except Exception as e:
        ms = (time.monotonic() - t0) * 1000
        print(f"[preflight] LLM FAILED after {ms:.0f} ms: {e}", flush=True)
        raise SystemExit(1)
    finally:
        await client.close()


def build_agent(model: str) -> SandboxAgent:
    return SandboxAgent(
        name="SWE-bench Agent",
        model=_make_chat_model(model),
        instructions=(
            "You are a software engineer debugging a Django issue inside a "
            "cube-sandbox environment. The Django source code is at /testbed.\n\n"
            f"{DJANGO_BUG_DESCRIPTION}\n"
            "Your task:\n"
            "1. Find the `items_for_result` function in "
            "`django/contrib/admin/templatetags/admin_list.py`.\n"
            "2. Analyze the bug and explain the root cause.\n"
            "3. Propose a fix.\n\n"
            "Use shell commands to explore the codebase, read files, and run tests. "
            "Be concise and cite the exact file paths and line numbers you inspected."
        ),
        default_manifest=Manifest(root="/testbed"),
        capabilities=[Shell()],
        model_settings=ModelSettings(tool_choice="auto"),
    )


def build_run_config(template: str | None, timeout: int) -> RunConfig:
    options = E2BSandboxClientOptions(
        sandbox_type=E2BSandboxType.E2B,
        template=template or os.environ.get("CUBE_TEMPLATE_ID"),
        timeout=timeout,
        pause_on_exit=False,
    )
    return RunConfig(
        sandbox=SandboxRunConfig(
            client=E2BSandboxClient(),
            options=options,
        ),
        workflow_name="SWE-bench Django demo",
    )


async def run_sandbox_only(template: str | None, timeout: int) -> None:
    """Create a sandbox via OpenAI Agents SDK E2BSandboxClient, run a
    health-check command, then destroy it."""
    client = E2BSandboxClient()
    options = E2BSandboxClientOptions(
        sandbox_type=E2BSandboxType.E2B,
        template=template or os.environ.get("CUBE_TEMPLATE_ID"),
        timeout=timeout,
        pause_on_exit=False,
    )
    manifest = Manifest()

    print(f"Creating sandbox via E2BSandboxClient (template={options.template}) ...")
    t0 = time.monotonic()
    session = await client.create(options=options, manifest=manifest)
    create_ms = (time.monotonic() - t0) * 1000

    sandbox_id = getattr(session, "sandbox_id", None) or getattr(
        getattr(session, "_inner", None), "state", None
    ) and session._inner.state.sandbox_id
    print(f"  sandbox_id = {sandbox_id}")
    print(f"  created in {create_ms:.0f} ms")

    try:
        t1 = time.monotonic()
        await session.start()
        start_ms = (time.monotonic() - t1) * 1000
        print(f"  session started in {start_ms:.0f} ms (workspace materialized)")
    except Exception as e:
        print(f"  session start failed: {e}")

    try:
        t2 = time.monotonic()
        result = await session.exec("uname -a && cat /etc/os-release | head -3")
        exec_ms = (time.monotonic() - t2) * 1000
        stdout = result.stdout if hasattr(result, "stdout") else str(result)
        print(f"  exec ({exec_ms:.0f} ms): {stdout.strip()}")
    except Exception as e:
        print(f"  exec failed: {e}")

    t3 = time.monotonic()
    try:
        await session.shutdown()
    except Exception as e:
        print(f"  shutdown warning: {e}")
    shutdown_ms = (time.monotonic() - t3) * 1000
    print(f"  sandbox destroyed in {shutdown_ms:.0f} ms")

    print(f"  total: {(time.monotonic() - t0) * 1000:.0f} ms")


async def run_agent(
    model: str,
    question: str,
    template: str | None,
    timeout: int,
    max_turns: int = 50,
) -> None:
    from openai.types.responses import ResponseTextDeltaEvent

    base_url = os.environ.get("OPENAI_BASE_URL", "(default)")
    print(f"[config] model={model}, base_url={base_url}")
    print(f"[config] template={template or os.environ.get('CUBE_TEMPLATE_ID')}")
    print(f"[question] {question}\n")

    print("[preflight] verifying LLM connection ...", flush=True)
    await _preflight_llm(model)

    agent = build_agent(model)
    run_config = build_run_config(template, timeout)

    turn = 0
    in_text = False
    t_start = time.monotonic()

    print("[status] creating sandbox & starting session ...", flush=True)
    result = Runner.run_streamed(agent, question, run_config=run_config, max_turns=max_turns)

    # Monitor the background run-loop task for silent exceptions
    async def _watch_task():
        task = result.run_loop_task
        if task is None:
            return
        try:
            await task
        except Exception as e:
            print(f"\n[ERROR] run loop task crashed: {type(e).__name__}: {e}", flush=True)
            import traceback
            traceback.print_exc()

    watcher = asyncio.create_task(_watch_task())

    got_first_event = False
    idle_timeout = 30  # cancel if no new events for 30s after answer
    got_answer = False

    async def _consume_with_idle_timeout():
        """Consume stream events with an idle timeout that kicks in after the
        LLM produces an answer.  This works around the Runner hanging during
        its internal finalization (get_single_step_result_from_response)."""
        nonlocal got_first_event, in_text, turn, got_answer
        event_iter = result.stream_events().__aiter__()
        while True:
            effective_timeout = idle_timeout if got_answer else timeout
            try:
                event = await asyncio.wait_for(
                    event_iter.__anext__(), timeout=effective_timeout
                )
            except StopAsyncIteration:
                break
            except asyncio.TimeoutError:
                if got_answer:
                    print(f"\n[status] no new events for {idle_timeout}s after answer — finishing",
                          flush=True)
                else:
                    print(f"\n[WARN] stream timed out after {timeout}s", flush=True)
                break

            if not got_first_event:
                sandbox_ms = (time.monotonic() - t_start) * 1000
                print(f"[status] first event received ({sandbox_ms:.0f} ms)\n", flush=True)
                got_first_event = True

            if event.type == "agent_updated_stream_event":
                print(f"[agent] {event.new_agent.name} running", flush=True)
                print("[status] waiting for LLM response ...", flush=True)

            elif event.type == "run_item_stream_event":
                if in_text:
                    print()
                    in_text = False

                if event.name == "tool_called":
                    turn += 1
                    item = event.item
                    call_name = getattr(getattr(item, "raw_item", None), "name", "tool")
                    call_args = getattr(getattr(item, "raw_item", None), "arguments", "")
                    if len(call_args) > 200:
                        call_args = call_args[:200] + "..."
                    print(f"[step {turn}] tool_call: {call_name}({call_args})", flush=True)

                elif event.name == "tool_output":
                    item = event.item
                    output = getattr(item, "output", "")
                    if isinstance(output, str) and len(output) > 300:
                        output = output[:300] + "...(truncated)"
                    print(f"  → output: {output}", flush=True)

                else:
                    print(f"[event] run_item: {event.name}", flush=True)

            elif event.type == "raw_response_event":
                if isinstance(event.data, ResponseTextDeltaEvent):
                    if not in_text:
                        print("\n[answer] ", end="", flush=True)
                        in_text = True
                    print(event.data.delta, end="", flush=True)
                    got_answer = True

            else:
                print(f"[event] {event.type}", flush=True)

    await _consume_with_idle_timeout()

    if in_text:
        print()

    watcher.cancel()
    elapsed = (time.monotonic() - t_start) * 1000
    print(f"\n[done] {turn} tool calls, {elapsed:.0f} ms total")


def main():
    parser = argparse.ArgumentParser(
        description="OpenAI Agents SDK + E2B + Cube Sandbox (SWE-bench Django)"
    )
    parser.add_argument(
        "--model", default="openai/glm-5.1",
        help="Model name (default: openai/glm-5.1 via TokenHub)",
    )
    parser.add_argument(
        "--question",
        default="Analyze the bug described in your instructions and propose a fix.",
        help="Question to ask the agent",
    )
    parser.add_argument(
        "--template", default=None,
        help="E2B template ID (default: from CUBE_TEMPLATE_ID env)",
    )
    parser.add_argument(
        "--timeout", type=int, default=300,
        help="Sandbox timeout in seconds (default: 300)",
    )
    parser.add_argument(
        "--max-turns", type=int, default=50, dest="max_turns",
        help="Max tool-call rounds before the agent stops (default: 50)",
    )
    parser.add_argument(
        "--sandbox-only", action="store_true", dest="sandbox_only",
        help="Only create and destroy a sandbox (no LLM, no agent)",
    )
    args = parser.parse_args()

    load_env(sandbox_only=args.sandbox_only)

    if args.sandbox_only:
        asyncio.run(run_sandbox_only(
            template=args.template,
            timeout=args.timeout,
        ))
    else:
        asyncio.run(run_agent(
            model=args.model,
            question=args.question,
            template=args.template,
            timeout=args.timeout,
            max_turns=args.max_turns,
        ))


if __name__ == "__main__":
    main()
