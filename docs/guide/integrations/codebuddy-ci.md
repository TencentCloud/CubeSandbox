---
title: CodeBuddy CI Integration Guide
author: dujunjin
date: 2026-07-23
tags:
  - integration
  - codebuddy
  - ci
  - coding-agent
lang: en-US
---

# CodeBuddy CI integration guide

Run [CodeBuddy Code CLI](https://www.npmjs.com/package/@tencent-ai/codebuddy-code) inside a CubeSandbox MicroVM when CI must inspect, test, or report on an untrusted checkout. Source and agent tools stay in the MicroVM, while the runner retains only the minimal CubeAPI and CodeBuddy secrets.

The runnable companion is in the repository at `examples/codebuddy-ci-integration/`.

## Supported interface

The example pins CodeBuddy Code `2.125.5` and uses its headless interface: `--print --output-format json --session-id <id>`. A later turn uses the same ID and `--resume <id>`. Check `codebuddy --help` after any CLI upgrade; interactive flags are not suitable for an E2B command channel.

## Setup

1. Build the supplied Dockerfile on top of `cubesandbox-base` and register the image with a health probe on port `49983`.
2. Store `E2B_API_URL`, `E2B_API_KEY`, and `CODEBUDDY_AUTH_TOKEN` as CI secrets; store the ready `CUBE_TEMPLATE_ID` as a non-secret CI variable.
3. Give the runner network access to CubeAPI, but apply default-deny CubeEgress to the template. Allow only the CodeBuddy API hostname required by the tenant and explicitly justified endpoints.
4. Upload a source `.tar` rather than mounting the CI workspace. Exclude `.git`, credentials, and files the task does not need.

The driver forwards only `CODEBUDDY_AUTH_TOKEN` into the command environment. It does not forward the runner's GitHub token, registry password, or arbitrary host environment variables.

## Minimal execution

```bash
tar --exclude=.git -cf /tmp/project.tar .
cd examples/codebuddy-ci-integration
python -m pip install -r requirements.txt
python run_codebuddy_ci.py --source-tar /tmp/project.tar
```

The default prompt runs the smallest relevant test and writes `/workspace/codebuddy-ci-report.md`. Provide a narrow `--prompt` for a real pipeline and keep the instruction to avoid commits/pushes. The CLI uses `bypassPermissions` only inside the disposable, egress-restricted MicroVM; never combine it with broad outbound access or production secrets.

## Snapshot-backed continuation

```bash
python run_codebuddy_ci.py --source-tar /tmp/project.tar --pause
python resume_codebuddy_ci.py <printed-sandbox-id>
```

`pause()` captures the writable layer, including `/workspace` and the CodeBuddy session directory. The resume driver reconnects using `Sandbox.connect` and invokes the CLI with the same session ID. Snapshots are sensitive artifacts: limit access, use expiry, and kill the sandbox after collecting the final report.

## GitHub Actions pattern

Copy the companion `github-actions.yml` into the repository that owns the CI job. It uses `pull_request`, `permissions: contents: read`, a tarball upload, and no `pull_request_target`. Keep external side effects out of the agent prompt; publish a comment or commit only in a separate reviewed workflow step.

## Troubleshooting

| Symptom | Resolution |
| --- | --- |
| Auth failure | Confirm `CODEBUDDY_AUTH_TOKEN` is a CI secret and injected at command time, not in the Dockerfile. |
| `403` or model timeout | Adjust only the necessary CubeEgress allow rule; default deny should remain enabled. |
| Template readiness failure | Preserve the base image's envd health endpoint on port `49983`. |
| Session not found after resume | Use the same `CODEBUDDY_SESSION_ID` and do not kill the paused sandbox first. |

## References

- `examples/codebuddy-ci-integration/` in this repository
- [Issue #644](https://github.com/TencentCloud/CubeSandbox/issues/644)
- [CodeBuddy Code CLI package](https://www.npmjs.com/package/@tencent-ai/codebuddy-code)
