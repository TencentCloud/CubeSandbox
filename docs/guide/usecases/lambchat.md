---
title: "LambChat: Migrating an Agent Platform's Sandbox to CubeSandbox"
author: Clivia
date: 2026-10-15
tags:
  - agent
  - e2b-compat
  - pause-resume
  - multi-tenant
lang: en-US
---

# LambChat: Migrating an Agent Platform's Sandbox to CubeSandbox

## Business Context

LambChat is a multi-tenant Agent system with integrated Skills capabilities, built on deepagents / LangGraph. Its core concept is the "digital space": every user binds to a resident sandbox that persists across sessions — users install packages, upload files, and let the Agent tinker freely. The sandbox layer supports three backends — E2B, CubeSandbox, Daytona — switchable by changing a single config field.

## Key Challenges

- **Isolation as a hard requirement**: free user tinkering must never let one user's actions affect other users or the host.
- **Lifecycle of user-level persistent sandboxes**: auto-pause when idle, auto-resume on return, memory state preserved.
- **Control-plane / data-plane state inconsistency**: after resume, the control plane shows running while the data-plane envd isn't ready and returns 504.
- **The bash -lc login shell**: the e2b SDK launches a fresh login shell per execution by default, so environment variables don't persist across two-phase tasks.

## Solution with Cube Sandbox

- **Inheritance-based adaptation**: CubeSandboxBackend inherits E2BBackend and only overrides the differences (ls via an inline Python script, get_info with camelCase/snake_case bidirectional compatibility, Cube-only pause, and list_user_sandboxes enumerating by metadata.user_id) — about 200 lines of adaptation code.
- **User-level persistent binding + four-level lookup chain**: in-memory LRU cache → MongoDB binding reconnect (sandbox.connect()) → enumerate and reuse existing sandboxes → create new; plus a fifth-level fallback that asynchronously cleans up duplicate sandboxes for the same user.
- **AutoPause configuration**: on_timeout=pause + auto_resume=true at creation — auto-pause when idle, auto-resume on request.
- **504 tolerance**: health checks hit both the control-plane state and a lightweight data-plane request; a 504 is judged unhealthy, triggering fallback to another sandbox or a new one.
- **Login-shell workaround**: explicitly re-set environment variables that must persist between two-phase tasks instead of relying on shell state.

## Results and Benefits

- Switching from the default backend to CubeSandbox took ~300 lines of code and one day — made possible by E2B protocol compatibility allowing direct base-class inheritance.
- KVM hardware-level isolation plus pause/resume memory-state preservation supports the "one user, one resident sandbox" digital-space model.
- Five distilled selection suggestions: decide binding granularity first; E2B compatibility is leverage; treat resume state inconsistency as an anomaly; know the bash -lc problem in advance; differentiated capabilities (snapshots / templates / network policies) can come later.

## References

- Full case study: [How Much Code Does Switching Sandbox Backends Take? Selection and Field Notes from Migrating an Agent Platform](/blog/posts/2026-10-15-lambchat-migration)
- Cube Sandbox source: [TencentCloud/CubeSandbox](https://github.com/TencentCloud/CubeSandbox)
