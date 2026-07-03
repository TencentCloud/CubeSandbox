# CodeBuddy Integration with Cube Sandbox

[中文文档](README_zh.md)

This example runs the CodeBuddy CLI inside a Cube Sandbox MicroVM. The local
Python runner creates a sandbox through the E2B-compatible SDK, injects the
CodeBuddy credential into that sandbox instance, writes a tiny demo workspace,
and starts CodeBuddy in headless mode with `codebuddy -p`.

## Prerequisites

- A running Cube Sandbox deployment with CubeAPI reachable from your machine.
- `cubemastercli` configured for the same deployment.
- Docker access to build and push an image that Cube nodes can pull.
- Python 3.8+ on your local machine.
- A CodeBuddy account or API key that works with the CodeBuddy CLI.
- Network egress from the sandbox to the CodeBuddy and model API endpoints.

## 1. Build the CodeBuddy template

Set `IMAGE_NAME` to a registry path reachable from your Cube cluster:

```bash
IMAGE_NAME=registry.example.com/cube/codebuddy:latest \
  DOCKER_PLATFORM=linux/amd64 \
  PUSH_IMAGE=1 \
  CREATE_TEMPLATE=1 \
  WATCH_JOB=1 \
  bash build-template.sh
```

The template image is based on `ghcr.io/tencentcloud/cubesandbox-base:2026.16`,
installs Node.js 22, and pins `@tencent-ai/codebuddy-code@2.114.2`.
`DOCKER_PLATFORM=linux/amd64` is recommended when building from Apple Silicon
macOS for the x86_64 Linux hosts used by Cube Sandbox.

Record the generated `template_id` and use it as `CUBE_TEMPLATE_ID`.

## 2. Configure local environment

```bash
python3 -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt

cp .env.example .env
```

Edit `.env`:

```bash
export E2B_API_URL="http://<your-node-ip>:3000"
export E2B_API_KEY="e2b_000000"
export CUBE_TEMPLATE_ID="<template-id>"
export CODEBUDDY_API_KEY="<your-codebuddy-api-key>"
```

Do not bake `CODEBUDDY_API_KEY` into the Docker image. The runner injects it
through `Sandbox.create(envs={...})`, so it only applies to the current sandbox
instance.

## 3. Run CodeBuddy in Cube Sandbox

```bash
python run_codebuddy.py
```

The runner creates `/tmp/codebuddy-demo` in the sandbox and asks CodeBuddy to
inspect it, run `python3 hello.py`, and summarize the result.

You can override the prompt:

```bash
python run_codebuddy.py \
  --prompt "Inspect /tmp/codebuddy-demo and run python3 hello.py"
```

## 4. Verify pause and resume

```bash
python run_codebuddy.py --pause-resume
```

After the CodeBuddy run, the script writes a marker under
`CODEBUDDY_CONFIG_DIR`, calls `sandbox.pause()`, reconnects with
`sandbox.connect()`, and verifies the marker still exists. This demonstrates
the state-preserving workflow used for long-running coding sessions.

## Operational notes

- Keep `DISABLE_AUTOUPDATER=1` in reproducible templates. Upgrade CodeBuddy by
  rebuilding the image with an explicit package version.
- Keep credentials out of images and Git. For production, prefer CubeSandbox's
  credential handling, security proxy, or an outbound gateway that can inject
  service credentials without exposing them to user code.
- `CODEBUDDY_API_KEY` is still visible to processes inside the sandbox because
  CodeBuddy runs there. Use scoped or short-lived credentials for demos, and
  avoid running untrusted generated code in the same sandbox as long-lived
  account credentials.
- Host `HTTP_PROXY`, `HTTPS_PROXY`, and `NO_PROXY` are not forwarded by default.
  Set `CODEBUDDY_FORWARD_PROXY_ENV=1` only when your deployment requires it. The
  runner strips proxy URL userinfo before sandbox injection, but credentials
  should still be managed by your proxy or egress layer instead of `.env`.
- The sandbox must be allowed to reach CodeBuddy and the backing LLM API. If
  your deployment uses network allowlists, allow the required domains or route
  traffic through CubeEgress.
- `CODEBUDDY_ALLOWED_TOOLS` and `CODEBUDDY_PERMISSION_MODE` are example
  defaults for headless runs. The demo uses `bypassPermissions` so CodeBuddy
  can execute `python3 hello.py` without an interactive approval prompt inside
  the sandbox. Tune it to your team's approval policy for production.

## Troubleshooting

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| `Missing required environment variables` | `.env` was not created or sourced values are empty | Copy `.env.example` to `.env` and fill required values |
| `Template not found` | `CUBE_TEMPLATE_ID` points to the wrong template | Re-run `cubemastercli tpl list` and update `.env` |
| `codebuddy: command not found` | The sandbox did not boot from the CodeBuddy template | Recreate the template from this example image |
| CodeBuddy authentication fails | Invalid `CODEBUDDY_API_KEY` or interactive login is required | Verify the key locally; if your account uses login state, inject a pre-authenticated `CODEBUDDY_CONFIG_DIR` instead |
| Requests time out | Sandbox egress is blocked | Check Cube network policy, DNS, proxy, and CubeEgress rules |
| Template creation times out | `envd` did not become healthy | Ensure the image is based on `cubesandbox-base` and template creation probes `49983 /health` |

## Directory structure

```text
codebuddy-integration/
├── .env.example
├── README.md
├── README_zh.md
├── build-template.sh
├── env_utils.py
├── requirements.txt
├── run_codebuddy.py
├── template/
│   └── Dockerfile
└── test_run_codebuddy.py
```
