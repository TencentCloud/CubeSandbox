---
title: "KakuriZai: Lightweight Isolated Workspaces for Parallel Multi-Agent Editing"
author: Ryo Sato (mizuame)
date: 2026-11-06
tags:
  - agent
  - overlayfs
  - snapshot
  - host-mount
lang: en-US
---

# KakuriZai: Lightweight Isolated Workspaces for Parallel Multi-Agent Editing

## Business Context

KakuriZai is a sandbox lifecycle control-plane project (with a Studio web UI) from IPA-CyberLab at the University of Tsukuba, Japan; CubeSandbox is one of the sandbox runtimes it integrates. The goal: let multiple agents edit a giant project like Unreal Engine — source plus binaries — in parallel: one adds a feature, another tries an alternative implementation, and afterwards each set of changes is reviewed and the wanted parts applied back to the original source.

## Key Challenges

- **Parallel-editing conflicts**: multiple agents working in the same project collide on file changes.
- **Giant projects can't be fully copied**: for UE-scale projects, a full environment copy per agent is unrealistic.
- **Template/disk-capacity mismatch**: changing disk size when deriving a sandbox from a template blocks at snapshot resize (`'rootfs-gen0' is a snapshot; cannot resize`).
- **The overlay deletion-semantics trap**: kernel overlayfs whiteouts (.wh. prefix) and unionfs-fuse (`_HIDDEN~` suffix) look different — copying the upper layer as-is loses deletions.
- **VM snapshots don't contain host-mount data**: lower/upper live on the host side; a VM snapshot doesn't necessarily preserve them as an independent backup.

## Solution with Cube Sandbox

- **Layered lower/upper sharing**: the host source tree is shared by all workspaces as a read-only lower layer; each World holds only its own writable upper layer, overlaid inside the guest as /workspace; copy-up, whiteout, and apply rules support "everyone edits their own, apply selected changes back."
- **Multiple mount backends**: setupOverlay() tries kernel overlayfs → unionfs-fuse → fuse-overlayfs in order; mount points are strictly confined under /workspace; the change-collection side adapts to read unionfs-fuse whiteouts.
- **Template resource matching**: resolveTemplateForResources() checks disk/CPU/memory/instance type/network type; on mismatch it reuses or asynchronously builds a compatible template (polling up to 20 minutes).
- **Graceful degradation**: World creation doesn't fail without cubecli on the host — the runtime state is marked planned and actually created once Cube becomes available.
- **Networking and advanced usage**: network_type=tap + iptables chains (KAKURIZAI-EGRESS/INGRESS) for egress control; batched multi-sandbox K8s labs; agctl replicate stateful mode for stateful forking — with host mounts present, /workspace content is first copied into the guest before being included in the snapshot.

## Results and Benefits

- Multiple agents edit the same giant project in parallel without cross-contamination; shared large files are always referenced, never copied.
- Three mount modes (agctl-overlay / cubesandbox-readonly / unsafe-rw) selectable by purpose.
- A small K8s cluster made of Cube sandboxes can be created and managed like a single World.

## References

- Full case study: [Building Lightweight Isolated Workspaces with CubeSandbox: A Layered Approach to Parallel Multi-Agent Editing](/blog/posts/2026-11-06-kakurizai-workspaces)
- KakuriZai project: [IPA-CyberLab/IPA-RS-KakuriZai](https://github.com/IPA-CyberLab/IPA-RS-KakuriZai)
- Cube Sandbox source: [TencentCloud/CubeSandbox](https://github.com/TencentCloud/CubeSandbox)
