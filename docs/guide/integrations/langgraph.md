---
title: LangGraph Integration Guide
author: Mikey1129
date: 2026-08-29
tags:
  - integration
  - langgraph
  - agent
lang: en-US
---

# LangGraph Integration Guide

Run a [LangGraph](https://github.com/langchain-ai/langgraph) agent — an explicit graph of nodes
and conditional edges — that executes Python inside a
[CubeSandbox](https://github.com/TencentCloud/CubeSandbox) MicroVM. Because Cube exposes an
**E2B-compatible API**, the code-execution tool is a drop-in swap from E2B to Cube, while every line
of agent-generated code gets KVM-level isolation.

This is the LangGraph counterpart to the [LangChain integration](./langchain.md). Where the
LangChain guide uses the high-level `create_agent` helper, this guide builds the graph **explicitly**
with `StateGraph`, so you control the control flow, share one sandbox across nodes, and can wire
LangGraph checkpointing to Cube's `pause()` / `connect()`.

A runnable version of this guide ships in
[`examples/langgraph-integration`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/langgraph-integration),
which is the source of truth for the code listings below.

## LangGraph vs `create_agent`

| | `create_agent` (LangChain guide) | Explicit `StateGraph` (this guide) |
|---|---|---|
| Graph shape | Fixed tool-calling loop, hidden from you | You define every node and edge |
| Retry / loops | Implicit in the agent loop | Explicit `add_conditional_edges` |
| State | Opaque message history | Typed `State` you design (attempts, verdict…) |
| Resume | Not directly exposed | `checkpointer` ↔ Cube `pause()` / `connect()` |

Use `create_agent` when you just need a tool-calling agent. Reach for an explicit `StateGraph` when
you want a multi-step workflow — generate → execute → review → retry — with the sandbox shared across
stages and the run resumable later via checkpointing.

## Components and versions

| Component | Version | Notes |
|---|---|---|
| langgraph | `>=1,<2` | `StateGraph`, `START`/`END`, `add_messages`; 0.2.x is incompatible with langchain-openai 1.x |
| langchain-openai | `>=1.0,<2.0` | `ChatOpenAI` (any OpenAI-compatible endpoint) |
| cubesandbox SDK | `>=0.6.0` | `Sandbox.create` / `files.write` / `commands.run` |
| CubeSandbox platform | `>=0.3.0` | core; higher for optional features (see LangChain guide) |
| CubeSandbox base image | `ghcr.io/tencentcloud/cubesandbox-base:2026.16` | layer your Python stack on top |

## Prerequisites

The prerequisites are identical to the LangChain guide — the same sandbox template works for both:

- CubeSandbox deployed, CubeAPI reachable at `http://<node>:3000`.
- A template image with the Python stack (pandas / numpy / matplotlib / scikit-learn) built and
  registered. Follow the LangChain guide's
  [template image](../integrations/langchain.md) steps, or reuse the same template id.
- `cubesandbox` SDK env vars: `CUBE_TEMPLATE_ID` (required), plus `CUBE_API_URL` and
  `CUBE_PROXY_NODE_IP` (required only for remote / direct-IP deployments — the SDK falls back to
  `http://127.0.0.1:3000` / the wildcard-DNS host when unset), and `CUBE_API_KEY` when the CubeAPI
  backend has auth enabled (the SDK sends no auth header when unset).
- Python 3.10+ (the sample uses `str | None`, `Annotated`, and `langchain-openai` 1.x).
- An OpenAI-compatible LLM endpoint via `OPENAI_BASE_URL` / `OPENAI_API_KEY` (or `TOKENHUB_API_KEY`).

## Integration steps

### 1. Build the template image

Use the shipped `examples/langgraph-integration/Dockerfile` (a Python data-science stack layered on `cubesandbox-base`,
with envd listening on `:49983`). No LangGraph-specific packages need to be baked into the image —
the graph runs on the host and only *code execution* happens inside the sandbox. Build and push it
under the tag you will register in step 2:

```bash
docker build --platform linux/amd64 -t <your-registry>/langgraph-cube:latest examples/langgraph-integration
docker push <your-registry>/langgraph-cube:latest
```

### 2. Register the template and configure env vars

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/langgraph-cube:latest \
  --writable-layer-size 2G \
  --expose-port 49983 --probe 49983 --probe-path /health
```

Then set `CUBE_TEMPLATE_ID` and your LLM key; set `CUBE_API_URL` and `CUBE_PROXY_NODE_IP` only when
they differ from the SDK defaults. The variable table in the LangChain guide applies unchanged.

### 3. Define the graph

The graph has three moving parts:

- **`coder`** — asks the LLM for a Python script and executes it in the sandbox.
- **`reviewer`** — asks the LLM whether the output answers the request.
- **a conditional edge** — routes `reviewer` back to `coder` (retry) or to `END`.

One sandbox is created for the whole run and reused across every `coder` invocation. The graph holds
it through the `run_python` closure (not through `State`); its id is the key you use to resume the
run later via checkpointing.

```python
from __future__ import annotations

import ast
import itertools
import os
import sys
from typing import Annotated, TypedDict

from dotenv import load_dotenv
from langchain_core.messages import AIMessage, HumanMessage
from langchain_openai import ChatOpenAI
from langgraph.graph import StateGraph, START, END
from langgraph.graph import add_messages
from cubesandbox import Sandbox

load_dotenv()

for v in ("CUBE_TEMPLATE_ID",):
    if not os.environ.get(v):
        raise SystemExit(f"Missing env: {v}")

_llm_key = os.getenv("OPENAI_API_KEY") or os.getenv("TOKENHUB_API_KEY")
if not _llm_key:
    raise SystemExit("Missing LLM API key")

llm = ChatOpenAI(
    model=os.getenv("CHAT_MODEL") or "deepseek-v3",
    api_key=_llm_key,
    base_url=os.getenv("OPENAI_BASE_URL") or "https://tokenhub.tencentmaas.com/v1",
    timeout=60, max_retries=2, temperature=0,
)


class AgentState(TypedDict):
    """State shared across nodes. `messages` accumulates the conversation;
    `attempts` / `done` drive the retry loop. The sandbox itself is shared
    through the `run_python` closure, not through `State`."""
    messages: Annotated[list, add_messages]
    attempts: int
    done: bool


def make_run_python(sandbox: Sandbox):
    """Return a `run_python` tool bound to one Cube sandbox — the same
    `cubesandbox` SDK pattern used by the LangChain guide."""
    _counter = itertools.count()

    def run_python(code: str) -> str:
        script = f"/workspace/_agent_{next(_counter)}.py"
        try:
            sandbox.files.write(script, code)
            result = sandbox.commands.run(f"python3 {script}", timeout=120, cwd="/workspace")
        except Exception as exc:
            # A dead sandbox, a failed write, transport errors and per-command
            # timeouts all raise here; surface them as tool output so the reviewer
            # can see the failure and decide to retry, rather than letting the
            # exception abort the whole graph run.
            return f"[command error] {exc}"
        out = result.stdout
        if result.stderr:
            out += "\n--- stderr ---\n" + result.stderr
        if result.exit_code != 0:
            out += f"\n[non-zero exit code: {result.exit_code}]"
        return out

    return run_python


CODER_PROMPT = (
    "You are a data analyst. Write a single self-contained Python script that answers "
    "the user's task using the dataset file(s) named in the task. The environment has "
    "pandas, numpy, matplotlib, scikit-learn preinstalled. Print the final numbers. "
    "Do not rely on network access. Wrap the script in a single markdown ```python ... ``` "
    "fenced block. Messages prefixed with [reviewer] are feedback on your last attempt, "
    "not a new task: if one said RETRY, fix the issues it listed before re-running."
)

REVIEWER_PROMPT = (
    "You are a reviewer. Given the user request and the latest code output, decide "
    "whether the request is fully answered. Reply starting with exactly one word, "
    "`DONE` or `RETRY`, optionally followed by one line explaining what is missing."
)


def extract_text(content) -> str:
    """Return plain text from a message content, which may be a str, a single
    content block dict, or a list of blocks (some OpenAI-compatible endpoints
    return the latter two)."""
    if not content:
        return ""                     # None / empty content — treat as no text
    if isinstance(content, str):
        return content
    if isinstance(content, dict):
        content = [content]           # a lone content block, not a list
    if isinstance(content, list):
        parts = []
        for block in content:
            if isinstance(block, str):
                parts.append(block)
            elif isinstance(block, dict) and block.get("type") == "text":
                parts.append(block.get("text") or "")
        return "\n".join(parts)
    return str(content)


def strip_code_fence(text: str) -> str | None:
    """Return the code inside the first markdown fence, or None when the reply
    has no fenced block — models often prefix the fenced block with prose."""
    fence = "`" * 3                      # three backticks, without a literal fence
    if not text:
        return None
    # The opener may share a line with prose (`` The code is: ```python ``), so
    # search for the first fence token anywhere rather than only at a line start.
    start = text.find(fence)
    if start == -1:
        return None
    # Read the full backtick-run length (```, ````, ...) so a 4-backtick fence is
    # not mistaken for a 3-backtick opener, and require the closer to match it.
    fence_len = len(fence)
    while start + fence_len < len(text) and text[start + fence_len] == "`":
        fence_len += 1
    # Code begins on the line after the opener; a language tag like ```python and
    # any trailing prose on that line are dropped.
    after = text[start + fence_len:]
    first_nl = after.find("\n")
    if first_nl == -1:
        return None                      # opener with no body — treat as no code
    # The opener's indentation is the leading whitespace on its own line. A closer
    # sits at that same indent, so a fence nested in a markdown list still closes
    # correctly, while a ``` line indented deeper (a markdown example inside a
    # docstring or string literal) stays code.
    opener_line = text[text.rfind("\n", 0, start) + 1:start]
    opener_indent = len(opener_line) - len(opener_line.lstrip())
    inner = []
    for line in after[first_nl + 1:].splitlines():
        # A closer is a line whose stripped leading backtick run matches the
        # opener's length at the opener's indent and is followed by nothing but
        # prose — models sometimes slip as `` ``` Done! ``. A shorter bare ``` line
        # or a 4-backtick fence when the opener was 3 all stay code.
        s = line.lstrip()
        run = 0
        while run < len(s) and s[run] == "`":
            run += 1
        if run == fence_len and (len(s) == run or s[run].isspace()) and len(line) - len(s) == opener_indent:
            break
        inner.append(line)
    return "\n".join(inner).strip() or None  # empty fence (```\n```) counts as no code


def coder(state: AgentState, run_python) -> dict:
    """Ask the LLM for code, execute it in the Cube sandbox, append the output."""
    try:
        reply = llm.invoke(
            [{"role": "system", "content": CODER_PROMPT}, *state["messages"]]
        ).content
    except Exception as exc:
        # A transient LLM error (rate limit, 5xx, timeout) must not abort the
        # whole graph run; surface it as empty code so the reviewer retries.
        return {"messages": [AIMessage(content=f"[code output]\n(llm error: {exc})")]}
    code = strip_code_fence(extract_text(reply))
    if code is None:
        # The model returned no fenced code block; surface that instead of writing
        # prose to a .py file and wasting an attempt on a SyntaxError.
        return {"messages": [AIMessage(content="[code output]\n(no code block in model reply)")]}
    try:
        ast.parse(code)
    except SyntaxError as exc:
        # The fenced block isn't valid Python (e.g. the model prefaced the real
        # script with a diff/example fence); surface it so the reviewer retries
        # instead of writing a .py that always fails.
        return {"messages": [AIMessage(content=f"[code output]\n(extracted block is not valid Python: {exc})")]}
    output = run_python(code)
    # Cap both the code and the output so a huge result (e.g. a printed DataFrame)
    # or a large generated script cannot blow the model's context window across
    # retries. Keep the tail of each: the printed numbers (and any stderr/traceback)
    # land at the end, and the script's tail still shows the logic that ran.
    if len(code) > 4000:
        code = "[earlier code truncated]\n" + code[-4000:]
    if len(output) > 4000:
        output = "[earlier output truncated]\n" + output[-4000:]
    # Include the code alongside the output so that on a RETRY the coder can see
    # what it wrote last time instead of repeating the same mistake.
    return {"messages": [AIMessage(content=f"[code]\n{code}\n[code output]\n{output}")]}


def reviewer(state: AgentState) -> dict:
    """Judge whether the latest output answers the request."""
    try:
        reply = llm.invoke(
            [{"role": "system", "content": REVIEWER_PROMPT}, *state["messages"]]
        ).content
    except Exception as exc:
        # A transient LLM error in the reviewer degrades to a retry rather than
        # aborting the run.
        return {
            "messages": [HumanMessage(content=f"[reviewer] RETRY (llm error: {exc})")],
            "attempts": state.get("attempts", 0) + 1,
            "done": False,
        }
    verdict = extract_text(reply).strip().upper()
    # The prompt asks for a single leading verdict word, so only the leading
    # token counts as the verdict — a "DONE"/"RETRY" later in the explanation
    # is prose and doesn't flip the result.
    first = verdict.split(maxsplit=1)[0].strip(":*#`").rstrip(".,!?;") if verdict else ""
    done = first == "DONE"
    # Emit the verdict as a user-role message so the coder treats RETRY as a
    # directive to fix, not as its own prior assistant output.
    return {
        "messages": [HumanMessage(content=f"[reviewer] {verdict}")],
        "attempts": state.get("attempts", 0) + 1,
        "done": done,
    }


def route_after_review(state: AgentState) -> str:
    """Conditional edge: retry `coder`, or finish after N attempts."""
    if state["done"] or state["attempts"] >= 3:
        return "end"
    return "retry"


def build_graph(run_python, checkpointer=None):
    builder = StateGraph(AgentState)
    builder.add_node("coder", lambda s: coder(s, run_python))
    builder.add_node("reviewer", reviewer)
    builder.add_edge(START, "coder")
    builder.add_edge("coder", "reviewer")
    builder.add_conditional_edges(
        "reviewer",
        route_after_review,
        {"retry": "coder", "end": END},
    )
    return builder.compile(checkpointer=checkpointer)


if __name__ == "__main__":
    question = sys.argv[1] if len(sys.argv) > 1 else (
        "Load sales.csv from /workspace, compute total revenue per month, "
        "and report the month -> revenue numbers."
    )

    # One MicroVM for the whole run, reused across every coder -> reviewer loop.
    # The context manager tears it down on exit, so nothing is left behind.
    with Sandbox.create(template=os.environ["CUBE_TEMPLATE_ID"], timeout=600) as sandbox:
        run_python = make_run_python(sandbox)
        graph = build_graph(run_python)
        result = graph.invoke({
            "messages": [{"role": "user", "content": question}],
            "attempts": 0,
            "done": False,
        })
        # The last message is the reviewer's verdict; print the code output
        # instead so the user actually sees the computed numbers. rfind takes the
        # last "[code output]" occurrence, so a literal inside the generated script
        # (before the real marker) can't shift the excerpt — only stdout that itself
        # prints that token would, which merely truncates the display. Print the
        # last coder message verbatim, even a failed attempt's placeholder, so the
        # numbers shown match the run's final state rather than an earlier attempt
        # the reviewer already rejected.
        for msg in reversed(result["messages"]):
            content = str(msg.content)
            marker = "[code output]"
            idx = content.rfind(marker)
            if idx == -1:
                continue
            print(content[idx:])
            break
        else:
            print("(no code output)")
        if not result["done"]:
            print("\n(not verified: reviewer never returned DONE)")
```

Save the code above as `langgraph_agent_demo.py`, then run it:

```bash
pip install "langgraph>=1,<2" "langchain-openai>=1.0,<2.0" "cubesandbox>=0.6.0" python-dotenv
python langgraph_agent_demo.py "Load sales.csv, compute total revenue per month."
```

### Expected behavior

`coder` writes the LLM-generated script to a fresh `/workspace/_agent_<n>.py` and runs it inside the
MicroVM; `reviewer` reads the output and either returns `DONE` or sends the graph back to `coder`
(up to 3 attempts). The `attempts` counter in `State` caps the loop so a stubborn request cannot spin
forever.

## Advanced: checkpointing + `pause()` / `connect()`

LangGraph checkpoints the graph state; Cube snapshots the sandbox. The two pair naturally for
long-running, resumable agents:

> `MemorySaver` below keeps checkpoints in **process memory** — fine for a demo, but they are
> gone when the process restarts. For cross-process resume, use a durable checkpointer such as
> `SqliteSaver` / `PostgresSaver` from `langgraph-checkpoint-sqlite` / `-postgres`.

| LangGraph | Cube Sandbox |
|---|---|
| `builder.compile(checkpointer=MemorySaver())` | `Sandbox.create(template=...)` |
| `config = {"configurable": {"thread_id": sandbox.sandbox_id}}` | `sandbox.sandbox_id` |
| continue a new run on the same thread via `invoke(..., config)` | `sandbox.pause()` then `Sandbox.connect(sandbox_id)` |

The snippet below continues the graph defined above — `Sandbox`, `build_graph`, and
`make_run_python` come from the earlier sections. The standalone runnable script is
`examples/langgraph-integration/langgraph_checkpoint_demo.py`.

```python
from langgraph.checkpoint.memory import MemorySaver

checkpointer = MemorySaver()  # in-process demo; use a durable checkpointer for real resume


def stage_input(messages):
    """Build the graph input for one stage. `attempts`/`done` have no reducer,
    so explicit values here overwrite whatever the checkpoint stored. `messages`
    uses `add_messages`, so new messages are APPENDED to the prior history —
    use a fresh thread_id if you want a clean slate."""
    return {"messages": messages, "attempts": 0, "done": False}


sandbox = None
try:
    sandbox = Sandbox.create(template=os.environ["CUBE_TEMPLATE_ID"], timeout=1800)
    # Key the checkpoint thread by the sandbox id so the same thread_id reattaches
    # to the same MicroVM across pause() / connect().
    config = {"configurable": {"thread_id": sandbox.sandbox_id}}
    graph = build_graph(make_run_python(sandbox), checkpointer=checkpointer)
    # Stage 1 writes an intermediate artifact so stage 2 reads state that only
    # survives because /workspace persists across pause() / connect().
    stage1 = graph.invoke(stage_input([{"role": "user", "content": "Load sales.csv from /workspace (columns month,product,units,price), compute total revenue per month, write the month -> revenue table to /workspace/monthly_revenue.csv, and write the exact string 'stage1-complete' to /workspace/stage1_marker.txt."}]), config=config)
    if not stage1["done"]:
        print("(stage 1 not verified: reviewer never returned DONE)")
        sys.exit(1)

    sandbox.pause()                                   # snapshot VM + rootfs
    # Sandbox.connect() returns a NEW instance; the run_python closure captured
    # the pre-pause one, so rebind the tool and rebuild the graph on the new
    # instance, keeping the same checkpointer so the same checkpoint thread resumes.
    sandbox = Sandbox.connect(sandbox.sandbox_id)     # /workspace intact after resume
    graph = build_graph(make_run_python(sandbox), checkpointer=checkpointer)
    graph.invoke(stage_input([{"role": "user", "content": "Read /workspace/monthly_revenue.csv and /workspace/stage1_marker.txt (both written by stage 1) and report the marker's exact text and which month had the highest revenue. Use only those two files — do not recompute from sales.csv."}]), config=config)
finally:
    if sandbox is not None:
        try:
            sandbox.kill()
        except Exception:
            pass
```

Keep the LangGraph `thread_id` aligned with the Cube `sandbox_id` (e.g. store both in your
orchestration layer) so a resumed graph reattaches to the same sandbox. Because `MemorySaver` only
survives within the current process, pair that orchestration layer with a durable checkpointer
(`SqliteSaver` / `PostgresSaver`) to resume across restarts.

## Caveats

- **State is not serialized with the sandbox.** `State` lives in your process (or the LangGraph
  checkpointer); only `/workspace` persists across `pause()` / `connect()`. Persist anything the
  graph needs across a hard resume.
- **One sandbox, one graph run.** The `coder`/`reviewer` loop reuses a single MicroVM; do not create
  a new sandbox per node — you would pay the lifecycle cost every step.
- **State does not persist across tool calls.** Each `commands.run` is a fresh `python3` process;
  inline everything a snippet needs or write intermediate results back to `/workspace`.
- **Preinstall the stack into the image.** Under a default-deny egress policy a runtime `pip install`
  fails; bake pandas / numpy / matplotlib into the template.
- **Cap the retry loop.** Use the `attempts` counter (as above) or LangGraph's recursion limit, so a
  reviewer that keeps asking for retries cannot exhaust the sandbox `timeout`.
- **`MemorySaver` is in-process only.** Checkpoints live in memory and are lost on restart; use
  `SqliteSaver` / `PostgresSaver` (from `langgraph-checkpoint-sqlite` / `-postgres`) for
  cross-process resume, even though the Cube sandbox survives `pause()` / `connect()`.

## References

- LangChain integration (the `create_agent` counterpart): [`langchain.md`](./langchain.md)
- Custom template images: [`docs/guide/tutorials/bring-your-own-image.md`](../tutorials/bring-your-own-image.md)
- Snapshot / clone / rollback: [`docs/guide/snapshot-rollback-clone.md`](../snapshot-rollback-clone.md)
- LangGraph: <https://github.com/langchain-ai/langgraph>
- E2B SDK: <https://github.com/e2b-dev/e2b>
