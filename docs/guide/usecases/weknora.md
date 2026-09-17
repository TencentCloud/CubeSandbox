---
title: "WeKnora: A Persistent Agent Runtime on CubeSandbox"
author: Chen Yang, Zhao Hailong
date: 2026-09-17
tags:
  - agent
  - snapshot
  - session-persistence
  - e2b-compat
lang: en-US
---

# WeKnora: A Persistent Agent Runtime on CubeSandbox

## Business Context

WeKnora is Tencent's open-source enterprise LLM knowledge platform (24.4k stars), turning enterprise document assets into RAG Q&A, ReAct agents, and a self-maintaining wiki. v0.8.0 centers on the skill sandbox runtime: session-resident sandboxes with per-space network policies. The sandbox layer supports three backends — Cube Sandbox, E2B, and Docker — with Cube Sandbox running the full chain of session binding, skill snapshots, pause/resume, template management, and network policies.

## Key Challenges

- **Missing session state**: the initial Docker `docker run --rm` approach destroyed everything after each run — packages installed in one turn were gone the next, and capabilities like attachment staging and artifact collection couldn't be registered.
- **High skill installation cost**: installing one skill means parsing SKILL.md, installing system packages and dependencies, and running verification — a multi-minute agent conversation that can't be amortized per session.
- **Shared-kernel isolation insufficient**: running model-generated code demands kernel-level isolation, cross-host scheduling, and memory-state snapshots — none of which Docker can deliver.
- **Application semantics of binding and reclamation**: Cube's TTL/AutoPause manages the MicroVM's lifetime but knows nothing about sessions, tenants, replicas, or skill generations — the application layer must cover these itself.

## Solution with Cube Sandbox

- **Neutral interface + capability advertisement**: RemoteSandboxClient with six lifecycle methods plus four auxiliary interfaces; all Cube-specific types/error codes/HTTP semantics translated into neutral DTOs — three backends interchangeable without changing a line of business code.
- **Skill snapshotting (deepest integration)**: create from base template → install skills → verify → CreateSnapshot; new sessions use the snapshot ID directly as TemplateID, skills ready in seconds. A fingerprint mechanism (SHA-256(provider+APIKey+APIURL)) guards ownership — after credential rotation, old snapshots silently invalidate and sessions fall back to the base template.
- **Session keep-alive**: onTimeout=pause + autoResume freezes the MicroVM when idle, preserving memory state, with the next Connect waking it automatically; /workspace is shaped into a session filesystem via envd Files API (read-only input/ for attachments, output/ collection for artifacts, cross-tool-call workspace).
- **Redis binding layer**: authoritative session→sandbox binding (SET NX, never expires) + lifecycle locks serialized across processes + a reaper periodically reconciling by tenant metadata to collect orphans.
- **Turn lease**: StaleAt declares "image changed" without acting; rebuild=1 at BeginTurn lets the first resolve of the turn rebuild and is consumed immediately — resolving the conflict of "admin installing a skill vs. user mid-conversation" on the same VM.
- **Explicit dual network switches**: allowInternetAccess for outbound, allowPublicTraffic for inbound — set explicitly at creation to guard against default drift.

## Results and Benefits

- Three backends (Docker/E2B/Cube) interchangeable under one abstraction; all Cube-specific adaptation consolidated in a single cube_remote_client.go file.
- Skill environments frozen as immutable "releases" — new sessions start from snapshots with skills ready in seconds; user experience: "the conversation is still there, and so is the environment."
- Distilled an 8-item sandbox verification checklist for Agent platforms (snapshot-as-template, snapshotting running instances, pause/resume semantics, metadata claiming, dual network switches, envd contract, envd in templates, pagination and idempotency).
- Fed 4 architecture-level requests back to the community: volume mount at Create, separate snapshot/template directories, pause/timeout callbacks, first-class session identity.

## References

- Full case study: [WeKnora: Building a Persistent Agent Runtime on CubeSandbox](/blog/posts/2026-09-17-weknora)
- WeKnora project: [Tencent/WeKnora](https://github.com/Tencent/WeKnora)
- Cube Sandbox source: [TencentCloud/CubeSandbox](https://github.com/TencentCloud/CubeSandbox)
