---
title: Python Incident RCA Code Interpreter Sandbox
author: Sam
date: 2026-07-04
tags:
  - python
  - data-science
  - code-interpreter
  - incident-rca
  - e2b
lang: en-US
---

# Python Incident RCA Code Interpreter Sandbox

## Business Context

AI agents are increasingly used to help SRE and platform teams triage incidents. During an outage, engineers often export service metrics, deployment events, alerts, and runbook thresholds, then ask the agent to identify the incident window, correlate changes, and draft a report.

The `examples/python-data-science` template demonstrates this workflow inside Cube Sandbox. The agent-style client uploads multiple files, runs Python in an isolated sandbox, checkpoints intermediate analysis state with a CubeSandbox snapshot, forks a fresh sandbox from that checkpoint for follow-up analysis, and downloads a packaged RCA result.

## Key Challenges

- Incident analysis needs real data processing, not just natural-language guessing.
- Metrics, alerts, deployment events, and runbook policies usually arrive as separate files.
- Code interpreter sessions need a persistent workspace across multiple analysis turns.
- Incident analysis often needs a checkpoint before trying follow-up hypotheses or sharing a reproducible state with another sandbox.
- Chinese reports and charts require CJK fonts in the sandbox image.
- Follow-up analysis may need packages that were not known when the image was built.
- RCA outputs should be structured enough to attach to tickets, review docs, or downstream automation.

## Solution with Cube Sandbox

The template builds from a digest-pinned `ghcr.io/tencentcloud/cubesandbox-base:latest` and installs a version-pinned Python data-science stack plus `fonts-wqy-zenhei`. The demo starts a sandbox through the E2B-compatible SDK and uploads:

- `incident_metrics.csv`: time-series service metrics,
- `deployments.json`: deployment events,
- `alerts.csv`: alert timeline,
- `runbook.json`: SLO thresholds and response hints.

The workflow uses a reusable code-interpreter pattern:

1. **Anomaly detection round**: calculates error rate, detects SLO breaches, and writes intermediate state under `/tmp/cubesandbox-incident-rca/state`.
2. **Snapshot checkpoint**: creates a CubeSandbox snapshot after the input files and round 1 state are in place.
3. **Forked follow-up RCA round**: starts a fresh sandbox from the checkpoint, verifies that the state files were inherited, correlates the incident with deployment events, renders a Chinese chart, writes a Markdown RCA report, emits structured JSON/CSV artifacts, and packages everything into a tarball.

The client downloads the archive, extracts it locally, and validates the manifest. This mirrors the way an AI code interpreter handles multi-file input, stateful analysis, checkpointable workspaces, and downloadable artifacts while keeping code execution isolated.

This is different from a generic SDK integration demo: the reusable template is the Python code-interpreter runtime and artifact pattern, while incident RCA is the production-style validation scenario.

## Results and Benefits

- Provides a reusable starting point for AI incident-analysis sandboxes.
- Demonstrates a stateful multi-turn code-interpreter workflow without requiring host-side file mutation.
- Shows CubeSandbox snapshot/fork as a checkpoint mechanism for reproducible follow-up analysis.
- Shows dynamic dependency expansion through `pip3 install emoji humanize` inside the sandbox.
- Produces reviewable artifacts: chart, RCA report, SLO summary, deployment correlation JSON, manifest, and intermediate state.
- Demonstrates Chinese Matplotlib rendering for localized engineering reports.
- Registers the example in the tutorial index so new users can discover it alongside other templates.

For restricted-egress or offline deployments, the dynamic packages can be preinstalled in the image or served from an internal PyPI mirror. After dependencies are baked into the template, the core workflow does not need unrestricted public internet access.

## References

- Related example: `examples/python-data-science`
- Related issue: [CubeSandbox sandbox templates and example ecosystem](https://github.com/TencentCloud/CubeSandbox/issues/645)
