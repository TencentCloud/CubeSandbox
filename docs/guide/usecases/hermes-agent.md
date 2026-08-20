---
title: "Hermes Agent: Running a Resident Agent Platform in CubeSandbox"
author: Chen Jinbo
date: 2026-08-20
tags:
  - agent
  - persistence
  - skills
  - host-mount
lang: en-US
---

# Hermes Agent: Running a Resident Agent Platform in CubeSandbox

## Business Context

The Shanghai Yangpu New Energy Technology AI team migrated their internal Agent runtime platform, Hermes Studio, onto CubeSandbox. A resident Agent application needs the sandbox to remember not just task artifacts, but also the Agent's own configuration, conversation history, and a set of frequently invoked Skills. But sandboxes are inherently ephemeral — restarts, pauses, and migrations can zero out all of this state.

## Key Challenges

- **Persistence**: Hermes home (`/root/.hermes`) and workspace (`/workspace`) need to survive sandbox rebuilds.
- **Skills layering**: Public Skills need centralized maintenance; private Skills must not be overwritten.
- **Network allowlisting**: LAN model services need separate outbound CIDR configuration.
- **Pause/resume**: TAP device EBUSY causes resume failure after pause.

## Solution with CubeSandbox

Using persistent directories on Cubelet nodes as the single storage foundation, mounting Hermes home, workspace, and public Skills into sandboxes via `metadata["host-mount"]`. Public Skills use a "read-only shared directory + private directory symlink" overlay design.

| Content | Cubelet Directory | Sandbox Path | Permission |
|---|---|---|---|
| Hermes home | `/data/shared/hermes-homes/<home-id>` | `/root/.hermes` | read-write |
| Workspace | `/data/shared/hermes-workspaces/<workspace-id>` | `/workspace` | read-write |
| Public Skills | `/data/shared/hermes-common-skills` | `/opt/hermes-common-skills` | read-only |

## Results and Benefits

- Dozens of Hermes sandboxes on a single node, running for about a month.
- Clear persistence boundaries — state recoverable after sandbox rebuild.
- Centralized Skills maintenance with private version priority.
- Submitted Issue [#953](https://github.com/TencentCloud/CubeSandbox/issues/953) to track TAP EBUSY issue.

## References

- Full case study: [Running Hermes Agent in Cube Sandbox](/blog/posts/2026-08-20-hermes-agent)
- Cube Sandbox source: [TencentCloud/CubeSandbox](https://github.com/TencentCloud/CubeSandbox)
