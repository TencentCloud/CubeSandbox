# OpenCode + CubeSandbox Example

[中文文档](README_zh.md)

This example runs the OpenCode terminal coding agent inside a CubeSandbox microVM. It demonstrates template construction, provider key injection, non-interactive `opencode run`, and optional snapshot-based session persistence.

## Files

| File | Description |
| --- | --- |
| `template/Dockerfile` | Sandbox image with Node.js, Git, Python, ripgrep, and `opencode-ai` |
| `build-template.sh` | Builds the image and prints the `cubemastercli tpl create-from-image` command |
| `run_opencode.py` | Creates a sandbox, seeds `/workspace`, runs OpenCode, and optionally snapshots it |
| `.env.example` | Local environment template |

## 1. Build the Template

```bash
./build-template.sh
```

If your Cube nodes cannot pull from your local Docker daemon, push the image first:

```bash
export IMAGE_REGISTRY=<your-registry>/<namespace>
export PUSH_IMAGE=1
./build-template.sh
```

Then run the printed `cubemastercli tpl create-from-image` command and copy the returned template ID.

## 2. Configure Environment

```bash
cp .env.example .env
```

Edit `.env`:

```bash
export E2B_API_URL="http://<cube-api-host>:3000"
export E2B_API_KEY="e2b_000000"
export CUBE_TEMPLATE_ID="<template-id>"
export OPENAI_API_KEY="<provider-key>"
export OPENCODE_MODEL="openai/gpt-4.1-mini"
```

OpenCode supports multiple providers. Set the provider-specific key and model name accepted by your OpenCode installation.

## 3. Run

```bash
pip install -r requirements.txt
python run_opencode.py --prompt "Inspect /workspace and create a short project summary."
```

Expected flow:

```text
sandbox sbx-...
$ opencode --version
...
$ opencode run --auto 'Inspect /workspace and create a short project summary.'
...
```

## 4. Snapshot and Resume

Create a snapshot after OpenCode initializes project state:

```bash
python run_opencode.py --prompt "Run /init and summarize this project." --snapshot
```

Resume from the snapshot:

```bash
python run_opencode.py --template <snapshot-id> --prompt "Continue from the previous state and inspect AGENTS.md."
```

The snapshot preserves `/workspace`, generated files, dependency caches, and OpenCode local state stored in the sandbox filesystem.

## 5. Network Policy

For normal OpenCode tasks, allow outbound access to the selected LLM provider and required package registries. For deterministic execution without model calls, start the sandbox with no internet:

```bash
python run_opencode.py --network no-internet --prompt "Run python app.py and report the output."
```

Use the no-internet mode only when the prompt does not require OpenCode to contact an external model, or when your provider endpoint is reachable through an internal allowlist/proxy.

## Troubleshooting

| Symptom | Fix |
| --- | --- |
| `Missing required environment variable` | Source `.env` or run the script from this directory |
| `Template not found` | Verify `CUBE_TEMPLATE_ID` with `cubemastercli tpl list` |
| `opencode: command not found` | Rebuild the OpenCode template and create a new Cube template |
| Provider auth fails | Check provider key env and OpenCode model name |
| Command timeout | Increase the sandbox timeout or pre-bake dependencies into the image |

## Related Docs

- `docs/guide/integrations/opencode.md`
- `examples/code-sandbox-quickstart/`
- `examples/snapshot-rollback-clone/`
