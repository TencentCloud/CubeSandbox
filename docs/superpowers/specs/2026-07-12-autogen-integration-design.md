# AutoGen Integration Guide Design

## Goal

Add an issue-compliant AutoGen integration showing how to run AutoGen on the
host while using CubeSandbox as its isolated Python execution tool.

## Scope

The contribution will add:

- English and Chinese integration guides.
- Entries in both integration indexes.
- A runnable `examples/autogen-integration` demo with bilingual README files,
  dependencies, environment configuration, and one Python entry point.

It will not run AutoGen itself inside CubeSandbox or add separate pause/resume
and credential-vault demos.

## Integration Shape

The host process creates:

1. An AutoGen `AssistantAgent` using `OpenAIChatCompletionClient`.
2. A CubeSandbox sandbox through the E2B Code Interpreter SDK.
3. An `execute_python(code: str)` function registered as an AutoGen tool.

When the model calls the tool, the function sends the generated Python to the
long-lived CubeSandbox Jupyter kernel and returns normalized text output. The
driver closes both the sandbox and model client in `finally` blocks.

## Tested Versions and Lifecycle

- AutoGen AgentChat and OpenAI extension: `0.7.5`.
- Python: `3.10+`.
- E2B Code Interpreter SDK: a version compatible with the repository examples.
- CubeSandbox: `0.5.1`.

The guide will state that AutoGen is in maintenance mode and link to Microsoft's
Agent Framework migration guidance. This keeps the requested integration useful
without implying that AutoGen is Microsoft's preferred framework for new
projects.

## Documentation Content

Both languages will cover:

1. Prerequisites and dependency installation.
2. CubeSandbox endpoint and template environment variables.
3. A before/after comparison showing local execution replaced by a Cube tool.
4. A runnable task where AutoGen writes and executes Python in CubeSandbox.
5. Going further: execution timeout, default-deny egress, file artifacts, and
   explicit sandbox cleanup.
6. Caveats covering template IDs, the required envd/Jupyter-capable template,
   model tool-calling support, and AutoGen maintenance status.

## Validation

- Compile the Python demo with `python -m py_compile`.
- Install/import the pinned Python dependencies when practical.
- Run repository documentation checks, including bilingual parity and docs
  build.
- Perform a static review that English and Chinese filenames, frontmatter, code
  snippets, and index entries remain aligned.
- If no live CubeSandbox deployment or model key is available, explicitly state
  that end-to-end execution could not be performed rather than claiming it.

## Pull Request

Use the title prefix `docs(integrations):`, link issue #244, and describe the
exact validation performed. The PR must include the repository-required
AI-assistance attribution and must not include a Signed-off-by tag.
