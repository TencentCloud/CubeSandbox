---
title: CodeBuddy Integration Guide
author: toxitoxi
date: 2026-07-02
tags:
  - integration
  - codebuddy
lang: en-US
---

# CodeBuddy Integration Guide

## Integration Target and Version

This guide shows how to run the CodeBuddy CLI inside Cube Sandbox as an
isolated terminal coding agent. The example is tested against:

- Cube Sandbox templates built from `ghcr.io/tencentcloud/cubesandbox-base:2026.16`
- `@tencent-ai/codebuddy-code@2.114.2`
- `e2b-code-interpreter>=2.4.1`

The integration uses Cube Sandbox's E2B-compatible API. The local runner creates
a sandbox from a CodeBuddy template, injects credentials with instance-scoped
environment variables, and starts CodeBuddy in headless mode with `codebuddy -p`.

## Prerequisites

- A running Cube Sandbox deployment with CubeAPI reachable at `E2B_API_URL`.
- `cubemastercli` configured for the same cluster.
- Docker and a registry that Cube nodes can pull from.
- A CodeBuddy account or API key that works with the CodeBuddy CLI.
- Network egress from sandboxes to CodeBuddy and the backing LLM API.
- Python 3.8+ with `e2b-code-interpreter` and `python-dotenv`.

Required environment variables for the example:

```bash
export E2B_API_URL="http://<your-node-ip>:3000"
export E2B_API_KEY="e2b_000000"
export CUBE_TEMPLATE_ID="<template-id>"
export CODEBUDDY_API_KEY="<your-codebuddy-api-key>"
```

## Integration Steps

1. Build the template image from
   `examples/codebuddy-integration/template/Dockerfile`.
2. Push the image to a registry reachable by Cube nodes.
3. Create a Cube template with `cubemastercli tpl create-from-image`, exposing
   and probing envd on `49983 /health`.
4. Copy `examples/codebuddy-integration/.env.example` to
   `examples/codebuddy-integration/.env` and fill in Cube and CodeBuddy
   settings.
5. Run `python examples/codebuddy-integration/run_codebuddy.py` from the
   repository root to create a sandbox and start CodeBuddy.
6. Run `python examples/codebuddy-integration/run_codebuddy.py --pause-resume`
   to verify state survives `sandbox.pause()` and `sandbox.connect()`.

The example build command is:

```bash
IMAGE_NAME=registry.example.com/cube/codebuddy:latest \
  DOCKER_PLATFORM=linux/amd64 \
  PUSH_IMAGE=1 \
  CREATE_TEMPLATE=1 \
  WATCH_JOB=1 \
  bash examples/codebuddy-integration/build-template.sh
```

The runner injects `CODEBUDDY_API_KEY` using `Sandbox.create(envs={...})`. Do
not store API keys in the Dockerfile, template environment, or Git.
Keep `DOCKER_PLATFORM=linux/amd64` when building from Apple Silicon macOS for
the x86_64 Linux hosts used by Cube Sandbox.

## Key Code Snippets

Template image:

```dockerfile
FROM ghcr.io/tencentcloud/cubesandbox-base:2026.16

ENV DISABLE_AUTOUPDATER=1 \
    CODEBUDDY_CONFIG_DIR=/workspace/.codebuddy

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl git gnupg python3 python3-pip ripgrep \
    && mkdir -p /etc/apt/keyrings \
    && curl -fsSL https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key \
        | gpg --dearmor -o /etc/apt/keyrings/nodesource.gpg \
    && chmod 0644 /etc/apt/keyrings/nodesource.gpg \
    && echo "deb [signed-by=/etc/apt/keyrings/nodesource.gpg] https://deb.nodesource.com/node_22.x nodistro main" \
        > /etc/apt/sources.list.d/nodesource.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends nodejs \
    && npm install -g @tencent-ai/codebuddy-code@2.114.2 \
    && codebuddy --version \
    && npm cache clean --force \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/cube-entrypoint.sh"]
CMD ["sleep", "infinity"]
```

Credential injection:

```python
with Sandbox.create(
    template=os.environ["CUBE_TEMPLATE_ID"],
    timeout=600,
    envs={
        "CODEBUDDY_API_KEY": os.environ["CODEBUDDY_API_KEY"],
        "CODEBUDDY_CONFIG_DIR": "/workspace/.codebuddy",
        "DISABLE_AUTOUPDATER": "1",
    },
) as sandbox:
    result = sandbox.commands.run("codebuddy -p 'Inspect this workspace' --output-format text")
    print(result.stdout)
```

Pause and reconnect:

```python
sandbox.commands.run("echo ready > /workspace/.codebuddy/state.txt")
sandbox.pause()
sandbox.connect()
result = sandbox.commands.run("cat /workspace/.codebuddy/state.txt")
print(result.stdout)
```

## Caveats

- CodeBuddy needs outbound access to its service and the backing LLM API. If
  Cube network policy is enabled, allow the required destinations or route
  traffic through CubeEgress.
- `CODEBUDDY_API_KEY` is injected per sandbox instance, but it is still visible
  to processes inside that sandbox because CodeBuddy runs there. Prefer scoped
  or short-lived credentials, a pre-authenticated `CODEBUDDY_CONFIG_DIR`, or
  CubeSandbox's security proxy for production.
- Host proxy variables are not forwarded by default. Set
  `CODEBUDDY_FORWARD_PROXY_ENV=1` only when required; the runner strips
  `HTTP_PROXY` and `HTTPS_PROXY` URL userinfo before sandbox injection. Avoid
  storing proxy credentials in `.env`.
- `CODEBUDDY_ALLOWED_TOOLS` and `CODEBUDDY_PERMISSION_MODE` in the example are
  headless-run defaults. The demo uses `bypassPermissions` so CodeBuddy can run
  `python3 hello.py` without an interactive approval prompt inside the sandbox.
  Tune it to match your approval policy for production.
- Keep `DISABLE_AUTOUPDATER=1` in templates and upgrade by rebuilding with a
  pinned CodeBuddy package version.
- If your CodeBuddy account uses interactive login instead of API-key auth,
  inject a pre-authenticated `CODEBUDDY_CONFIG_DIR` at sandbox creation time
  rather than storing login state in the image.
- If template creation times out, first confirm the image starts envd and that
  the template probes `49983 /health`.

## References

- Runnable example: `examples/codebuddy-integration`
- CodeBuddy CLI installation: https://www.codebuddy.ai/docs/cli/installation
- CodeBuddy headless mode: https://www.codebuddy.ai/docs/cli/headless
- Cube Sandbox bring-your-own-image guide: ../tutorials/bring-your-own-image.md
- Cube Sandbox network policy guide: ../network-policy.md
