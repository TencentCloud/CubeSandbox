---
title: Gemini CLI Integration Guide
author: initiallyqq
date: 2026-07-10
tags:
  - integration
  - gemini-cli
  - coding-agent
  - agent
lang: en-US
---

# Gemini CLI Integration Guide

This guide runs Gemini CLI in a CubeSandbox MicroVM so an agent can inspect and modify a workspace without receiving host access. The companion example is in [`examples/gemini-cli-integration`](../../../examples/gemini-cli-integration/).

## What the integration provides

- A template Dockerfile based on `ghcr.io/tencentcloud/cubesandbox-base` with Node.js and `@google/gemini-cli`.
- A one-shot coding-agent runner using the E2B-compatible Python SDK.
- A pause/resume runner that verifies workspace state survives a CubeSandbox snapshot.
- A default-deny egress example that uses CubeEgress to inject `x-goog-api-key` on the allowed Google API request.

## Prerequisites

- A running CubeSandbox deployment with a `READY` template.
- Python 3.10+ and the packages in `requirements.txt` on the host that runs the example.
- A Google AI Studio API key for Gemini API-key mode.
- A registry reachable by CubeSandbox nodes when publishing a custom template.

## Build the template

```bash
cd examples/gemini-cli-integration
chmod +x build-template.sh
IMAGE=registry.example.com/cube/gemini-cli:2026-07-10 ./build-template.sh
```

The script builds the image, pushes it, and registers it with a readiness probe on the inherited `envd` port `49983`. Pin `GEMINI_CLI_VERSION` after validating upgrades instead of relying on `latest` in production.

## Configure the host-side runner

```bash
cp .env.example .env
python3 -m pip install -r requirements.txt
```

Set `E2B_API_URL`, `E2B_API_KEY`, `CUBE_TEMPLATE_ID`, and `GEMINI_API_KEY` in `.env`. Keep `.env` on the trusted runner host; it is excluded from Git.

## Run a coding task

```bash
python3 run_gemini.py --approve-all
```

The runner creates a sandbox, seeds a small Python project, and invokes `gemini --prompt ...`. `--approve-all` maps to Gemini CLI `--yolo`; use it only for an explicitly scoped sandbox because it lets the agent perform tool actions without per-action confirmation.

## Preserve work across turns

```bash
python3 resume_gemini.py --approve-all
```

The first turn writes `/workspace/plan.md`, then `sandbox.pause()` creates a snapshot. The runner reconnects with `Sandbox.connect(...)`, verifies `plan.md`, and runs a second turn. Do not use a `with Sandbox.create(...)` context manager for this workflow: leaving the context kills the sandbox and defeats pause/resume.

## Secure egress and secret injection

The simple runner passes `GEMINI_API_KEY` in the per-command environment and is useful for development only. For shared clusters, run:

```bash
python3 network_policy.py --approve-all
```

This creates the sandbox with `allow_internet_access=False` and a single CubeEgress rule for `generativelanguage.googleapis.com`. The rule injects the real `x-goog-api-key` header after the host/SNI match. Gemini receives a placeholder `GEMINI_API_KEY`; the actual key is not present in the VM environment, filesystem, or command line.

For HTTPS interception, build templates with the CubeEgress CA available. The example sets `NODE_EXTRA_CA_CERTS=/etc/ssl/certs/ca-certificates.crt` so Node can trust the interception chain. See [Security Proxy](../security-proxy.md) for the rule grammar and audit behavior.

## Recommended operating model

- Use a dedicated template with fixed Gemini CLI and Node versions.
- Keep `--yolo` disabled unless the sandbox workload and file scope are intentionally constrained.
- Give each user/session a separate sandbox; pause idle sessions and enforce lifecycle timeouts.
- Use default-deny egress with a narrow Google API host rule in production.
- Store only non-sensitive workspace state in snapshots. A snapshot preserves writable filesystem and process state.
- Review CubeEgress metadata audit logs for the rule name, destination, status, and latency; secrets are redacted.

## Troubleshooting

| Symptom | Cause and resolution |
| --- | --- |
| `gemini: command not found` | Rebuild the template and inspect `gemini --version` inside the image. |
| Authentication fails | Verify the key is valid for API-key mode. For the vault path, ensure the CubeEgress rule uses the correct host and injects `x-goog-api-key`. |
| TLS self-signed or connection error | The template may not trust the CubeEgress CA. Rebuild with the standard base image/CA path and set `NODE_EXTRA_CA_CERTS` as shown. |
| Egress request is denied | Default-deny is expected. Add only the required API host as an explicit rule; do not enable unrestricted internet access as a workaround. |
| Files disappear after the first turn | Confirm the workflow calls `pause()` and reconnects to the same sandbox ID, rather than creating a new sandbox or exiting a context manager. |
| Agent waits for approval | Add `--approve-all` only when autonomous writes are intended, or keep approval-gated behavior for safer runs. |

## Validation

```bash
python3 -m unittest examples/gemini-cli-integration/test_common.py
python3 -m py_compile examples/gemini-cli-integration/*.py
bash -n examples/gemini-cli-integration/build-template.sh
docker build -t gemini-cli-cube:local examples/gemini-cli-integration
```

A live run additionally needs a registered CubeSandbox template and credentials. Test `run_gemini.py`, `resume_gemini.py`, and `network_policy.py` against a non-production project before enabling autonomous actions.
