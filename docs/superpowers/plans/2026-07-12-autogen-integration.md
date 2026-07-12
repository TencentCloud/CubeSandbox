# AutoGen Integration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Publish a bilingual, runnable AutoGen 0.7.5 integration that uses CubeSandbox as an isolated Python execution tool.

**Architecture:** AutoGen runs on the host and receives a synchronous `execute_python` tool backed by one long-lived E2B Code Interpreter sandbox. The demo owns both the model client and sandbox lifecycle, normalizes execution output for the model, and always closes resources.

**Tech Stack:** Python 3.10+, AutoGen AgentChat 0.7.5, `autogen-ext[openai]` 0.7.5, E2B Code Interpreter SDK, VitePress Markdown.

---

### Task 1: Add the runnable AutoGen example

**Files:**
- Create: `examples/autogen-integration/autogen_cube.py`
- Create: `examples/autogen-integration/requirements.txt`
- Create: `examples/autogen-integration/.env.example`

- [ ] **Step 1: Add pinned runtime dependencies**

Use:

```text
autogen-agentchat==0.7.5
autogen-ext[openai]==0.7.5
e2b-code-interpreter>=2.4.1
python-dotenv>=1.0.0
```

- [ ] **Step 2: Define explicit environment configuration**

`.env.example` must include `OPENAI_API_KEY`, optional `OPENAI_BASE_URL`,
`AUTOGEN_MODEL`, `E2B_API_URL`, `E2B_API_KEY`, and `CUBE_TEMPLATE_ID`, with no
real credentials.

- [ ] **Step 3: Implement sandbox output normalization**

Create a helper with this contract:

```python
def format_execution(execution: object) -> str:
    """Return stdout, stderr, rich text results, or an explicit empty marker."""
```

It must collect `execution.logs.stdout`, `execution.logs.stderr`, and textual
`execution.results` without inventing output.

- [ ] **Step 4: Implement the AutoGen tool and lifecycle**

The entry point must:

```python
sandbox = Sandbox.create(template=template_id, timeout=600)

def execute_python(code: str) -> str:
    """Execute Python in the isolated CubeSandbox and return captured output."""
    return format_execution(sandbox.run_code(code))

agent = AssistantAgent(
    "cube_assistant",
    model_client=model_client,
    tools=[execute_python],
    system_message=(
        "Use execute_python for every calculation or code execution. "
        "Do not claim code ran unless the tool returned a result."
    ),
)
```

Wrap execution in `try/finally`; call `sandbox.kill()` and
`await model_client.close()` in cleanup.

- [ ] **Step 5: Compile the example**

Run:

```bash
python3 -m py_compile examples/autogen-integration/autogen_cube.py
```

Expected: exit code 0.

### Task 2: Document the runnable example

**Files:**
- Create: `examples/autogen-integration/README.md`
- Create: `examples/autogen-integration/README_zh.md`

- [ ] **Step 1: Write the English README**

Include prerequisites, virtualenv setup, environment variables, template
requirements, the run command, expected tool flow, cleanup behavior, and a
statement that AutoGen is in maintenance mode.

- [ ] **Step 2: Write the matching Chinese README**

Keep commands, filenames, environment variables, and version numbers identical
to the English document.

- [ ] **Step 3: Check bilingual structure**

Run:

```bash
rg '^## ' examples/autogen-integration/README.md
rg '^## ' examples/autogen-integration/README_zh.md
```

Expected: equivalent section ordering in both files.

### Task 3: Add the bilingual integration guides

**Files:**
- Create: `docs/guide/integrations/autogen.md`
- Create: `docs/zh/guide/integrations/autogen.md`

- [ ] **Step 1: Write aligned frontmatter**

Use `author: coder-jeffery`, date `2026-07-12`, tags `integration`, `autogen`,
`code-execution`, and the matching `en-US` / `zh-CN` language field.

- [ ] **Step 2: Cover every Issue #244 requirement**

Both guides must include:

1. Setup and tested versions.
2. Before/after snippet replacing local `exec` with `execute_python`.
3. Runnable demo instructions linking `examples/autogen-integration`.
4. Going-further guidance for timeout, default-deny egress, file artifacts,
   and cleanup.

- [ ] **Step 3: State compatibility constraints**

Document that Cube template IDs use `tpl-*`, the template must expose the envd
and Jupyter-compatible endpoints expected by the E2B SDK, the selected model
must support tool calls, and AutoGen is maintained but no longer Microsoft's
recommended choice for new projects.

### Task 4: Publish and validate the documentation

**Files:**
- Modify: `docs/guide/integrations/index.md`
- Modify: `docs/zh/guide/integrations/index.md`

- [ ] **Step 1: Add matching index rows**

Add AutoGen rows with author `coder-jeffery`, date `2026-07-12`, and identical
tag sets.

- [ ] **Step 2: Run bilingual parity**

Run the same check used by `.github/workflows/docs-bilingual-check.yml`.
Expected: exit code 0 and no missing language pair.

- [ ] **Step 3: Build the docs**

Run:

```bash
npm ci --prefix docs
npm run docs:build --prefix docs
```

Expected: VitePress build completes successfully.

- [ ] **Step 4: Review the final diff**

Confirm no secrets, placeholder template IDs are clearly marked, all links are
valid relative paths, and no claim of live end-to-end testing is made unless a
real deployment and model key were used.

### Task 5: Commit and open the pull request

- [ ] **Step 1: Commit the implementation**

Use a concise `docs(integrations):` commit subject and include:

```text
Autonomously-by: Cursor:GPT-5.6 Sol
```

Do not add a Signed-off-by tag.

- [ ] **Step 2: Push the branch**

```bash
git push -u origin docs/autogen-integration
```

- [ ] **Step 3: Open the PR**

Use title `docs(integrations): add AutoGen integration guide`. The body must
summarize the bilingual guide and runnable demo, list actual validation, link
Issue #244 without closing the umbrella issue, and include:

```text
Autonomously-by: Cursor:GPT-5.6 Sol
```
