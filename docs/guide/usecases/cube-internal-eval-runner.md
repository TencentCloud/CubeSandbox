---
title: Cube-Powered Internal Evaluation Runner
author: March-77
date: 2026-07-23
tags:
  - evaluation
  - e2b
  - platform
lang: en-US
---

# Cube-Powered Internal Evaluation Runner

## Business Context

A research team needed a self-hosted execution environment for code-execution tasks in model evaluation. Requirements:

- run untrusted model output safely,
- keep per-run artifacts reproducible,
- produce deterministic metrics logs,
- scale with many concurrent jobs without long startup queues.

Public SaaS sandboxes were used at first, but latency and compliance constraints prevented adoption.

## Key Challenges

- **Security isolation**: evaluation prompts can contain arbitrary code that must never touch production hosts.
- **Turn cost and throughput**: each sample requires a short isolated sandbox with cleanup.
- **Artifact retention**: generated files, stdout logs, and error traces needed structured retention.
- **Cross-region drift**: one container image behaves differently across environments.

## Solution with Cube Sandbox

The team replaced shared runtime containers with Cube templates and aligned execution with a per-job workspace.

1. Build a lean evaluation template with pinned dependencies.
2. Route evaluator workers to different templates based on language/runtime demand.
3. Mount a per-run storage key so artifacts are traceable to model run IDs.
4. Use `cubecli logs` + structured JSON logs to compare run outputs across environments.

### 1. Template model matrix

- `py-eval-basic` for data parsing and deterministic script execution.
- `py-eval-ext` for graph/math heavy workloads.
- Shared `code-interpreter-v1` for agentic tasks requiring package installation on demand.

### 2. Workflow pattern

- Each evaluator worker creates a new sandbox with a bounded timeout.
- The worker uploads test prompt, input data path, and run metadata.
- Outputs are written back as artifacts and collected by the orchestrator.
- The sandbox is terminated; snapshots and logs remain addressable via run ID.

### 3. Determinism checks

- Strict timeout policy for wall-clock and CPU budgets.
- Deterministic dependency lockfile in template build.
- Explicit error contract: missing timeout or runtime crash is mapped to same failure code across languages.

## Results and Benefits

- Isolated sandbox startup stayed under target warm-up budget with low variance.
- Security reviews were easier because host escape risk was reduced from the execution layer.
- Evaluation dashboards became more comparable: same template hash + version tags made cross-node results stable.
- On-call workload dropped because noisy infra failures (network, dependency drift) were caught at template version boundaries.

## References

- Related docs:
  - [模板系统与部署文档](../template-inspection-and-preview.md)
  - [组件日志排障](./troubleshooting/index.md)
- Demo or repository:
  - Internal evaluation runner repository (internal)
- Additional reading:
  - [How to Build Reliable Evals](https://docs.google.com/)
