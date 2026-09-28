---
title: "Huajiao: Sandbox as an Option, Not a Default — Agent Platform Practice"
author: Wang Chenglong, Feng Bingqing
date: 2026-09-17
tags:
  - agent
  - host-mount
  - go-sdk
  - execution-environment
lang: en-US
---

# Huajiao: Sandbox as an Option, Not a Default — Agent Platform Practice

## Business Context

Huajiao Live's (a Huafang Group company) enterprise Agent platform completed its 0-to-1 validation on the Huajiao Live business side, then deployed an independent instance for the Huafang Group middle platform on the same Agent Runtime. The two deployments together run about 30 Agent applications (DMG business assistant, ChatBI, content operations assistant, legal workbench, GR workbench, etc.), consuming about 83 million tokens daily. CubeSandbox has been in production for over 3 months.

## Key Challenges

- **Multi-user physical isolation**: the early single-server directory isolation meant user A could theoretically operate on user B's files through the Agent.
- **User data persistence across sandboxes**: sandboxes are reclaimable, but users' working state must survive; ordinary data disks can't attach to multiple cloud servers simultaneously.
- **Strong isolation + fast startup**: Docker starts slowly with incomplete isolation; traditional VMs are costly and inflexible.
- **Self-owned terminal semantics**: the platform needs a self-defined terminal protocol that can evolve independently, rather than binding the runtime session model to an external sandbox API's semantics.

## Solution with Cube Sandbox

- **Execution-environment abstraction**: the platform's key abstraction is the "execution environment," and CubeSandbox is one sandbox option integrated into it. The isolation decision is not made uniformly at the platform layer — each Agent decides by "do resources need physical isolation among users." Between organizations (Huajiao Agent / Huafang Agent), two fully independent instances run with no shared control plane.
- **NFS + host-mount persistence**: all persistent user data goes to an NFS directory; on server failure, a new machine mounts the same NFS and continues serving. The execution-environment daemon maps directories into sandboxes via host-mount, one session per sandbox.
- **Deep community collaboration**: contributed the official Go SDK ([PR #254](https://github.com/TencentCloud/CubeSandbox/pull/254), 15 files, 3,288 lines), fixed a connection-pool mis-closure ([PR #322](https://github.com/TencentCloud/CubeSandbox/pull/322)), and root-caused the host-mount snapshot-restore failure where the virtio-fs root inode lost migration state, raising InvalidVirtioFsState ([PR #341](https://github.com/TencentCloud/CubeSandbox/pull/341)).
- **Self-developed agentd terminal protocol**: runs on port 49984, coexisting with envd (49983), started as the template's main command (PID 1); exposes a narrow HTTP API (sessions/stdin/resize/healthz) with a monotonic-chunk_id ring buffer for incremental output polling; it is the platform's sole execution data plane for exec_command / write_stdin, flattening away differences between underlying execution-environment types.

## Results and Benefits

- One Agent Runtime supports two independent organizational deployments with ~30 Agent applications in production.
- The resource-understanding correction distilled into a practice principle: a sandbox is a high-performance, strongly isolated VM, not an elastic resource pool that auto-scales by usage — plan specs in tiers (test/production) with idle reclamation and monitoring-driven adjustments.
- The Go SDK and multiple fixes merged upstream; the host-mount snapshot-restore issue is currently worked around by destroy-and-rebuild.

## References

- Full case study: [Sandbox as an Option, Not a Default: Huajiao's Agent Platform Architecture and Cube Practice](/blog/posts/2026-09-17-huajiao)
- Cube Sandbox source: [TencentCloud/CubeSandbox](https://github.com/TencentCloud/CubeSandbox)
