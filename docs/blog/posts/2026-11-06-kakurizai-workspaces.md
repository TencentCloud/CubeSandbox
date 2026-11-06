---
title: "Building Lightweight Isolated Workspaces with CubeSandbox: A Layered Approach to Parallel Multi-Agent Editing"
date: 2026-11-06
author: Ryo Sato (mizuame, University of Tsukuba — lead of the KakuriZai project)
description: "Running multiple agents in parallel on the same large project means their file changes collide, and a full environment copy per agent is unaffordable for a giant project like Unreal Engine with source and binaries together. In the KakuriZai project at IPA-CyberLab, the host's source tree is shared by all workspaces as a read-only lower layer, each agent holds only its own writable upper layer, and execution is delegated to CubeSandbox."
featured: true
weight: 1
---

# Building Lightweight Isolated Workspaces with CubeSandbox: A Layered Approach to Parallel Multi-Agent Editing

*Author: Ryo Sato (mizuame) — University of Tsukuba, lead of the KakuriZai project*

**Editor's note:** Running multiple agents in parallel on the same large project is becoming a common shape for AI-assisted development, but file changes collide with each other. Giving every agent a full copy of the environment does not scale either: for a project like Unreal Engine, where source code ships together with binaries, a full copy is not affordable. The author of this practice, from the University of Tsukuba, presents a complete solution in the KakuriZai project at IPA-CyberLab: the host's source tree is shared by all workspaces as a read-only lower layer, each agent holds only its own writable upper layer, and execution is delegated to CubeSandbox.

KakuriZai positions itself as a lifecycle control plane for sandboxes, with a Studio web interface, and CubeSandbox is one of the sandbox runtimes it integrates. This article records the full implementation path, from matching template resources, to overlay mounts inside the guest, to whiteout handling, to stateful forking. One boundary in particular — **a VM snapshot does not contain host-mount data** — is directly relevant to anyone combining mounts with snapshots.

## 1. Problem and Approach: Sharing a Large Project Through Layering

Having AI write code brings real efficiency gains, but running multiple agents in the same project means their file changes collide. Giving each agent an isolated workspace resolves the conflicts — yet as soon as you count Unreal Engine's source code and binaries, a full copy per workspace is not realistic.

So we settled on a principle when designing the workflow: all agents reference the same large directory, while each agent holds only its own changes, and commands run in an isolated environment. KakuriZai is built around exactly this use case. On top of the existing IsolatedAgent / agentctl environment management, it adds a cube-sandbox-overlay backend that uses CubeSandbox. Workspaces are managed in units called Worlds, through the agctl CLI and the Studio web interface.

Our goal is to let multiple agents edit a giant engine like UE, along with its surrounding tools and binaries, in parallel: **one agent adds a feature, another tries an alternative implementation, and afterwards we review each set of changes and apply the parts we want back into the original source.**

To do this, the source tree on the host is referenced as a read-only lower layer, each World gets its own writable upper layer, and inside the guest the two are overlaid and presented as /workspace. For example: the shared source contains src/main.cpp. World A edits it, and World B must not see A's change — A's modification lands in A's upper layer, while B keeps reading the shared lower layer.

The file operation rules in agctl-overlay mode are:

| Operation | How the file is handled |
|:--|:--|
| Read an unmodified file | Read from the shared lower layer |
| Modify an existing file | Copy-up to the World's own upper layer, then modify there |
| Create a new file | Saved into the World's own upper layer |
| Delete a file that exists in the lower layer | Leave a whiteout marker on the writable side |
| agctl apply | Apply the upper layer's changes and deletions back to the original source |

The large shared files are always referenced rather than copied, and each environment holds only what it modified — which is exactly the effect we wanted.

That said, the writable layer is not free. First, copy-up works at file granularity: even a small edit to a large binary may require copying the entire file, and build artifacts pile up on the upper side as well, so the actual size and I/O of what you plan to modify need to be measured in advance. Second, a read-only lower layer means the sandbox cannot write to it directly — it does not mean the referenced source is frozen at some past state. Once A's changes update the shared source through apply, what World B reads changes along with it. Third, the current apply only copies changes and deletions; it does no conflict detection or three-way merge like Git. In parallel work, which baseline you build on, when you apply, and whether conflicts exist all need to be managed by yourself.

## 2. Execution Environment: Why CubeSandbox, and Matching Template Resources

For the sandbox runtime, we adopted CubeSandbox.

The primary reason we chose it is how lightweight it is. Compared with other sandbox options, it asks less of the host and runs comfortably even on a small machine like a mini PC. For a use case that needs many workspaces, the cost of adding one more environment stays very small, which is a significant advantage. We did not want to assume a large server from day one; we wanted to start from a small host and add execution nodes when needed — Cube's design matched that thinking. On top of that, delegating the execution environment to Cube let us focus on workspace management and change adoption, which was another point in its favor.

For startup time and memory overhead, the official README benchmarks are a reference: at 50 concurrent creations, the average creation time is 66.7 ms; on memory, the VMM-side overhead (PSS) stays within a small range even when CPU and memory configurations change. These numbers come from the official bare-metal measurements, and CPU, memory, I/O, and in-sandbox processing all affect the results — the official performance report itself asks readers to evaluate on their own environment and workload. Our mini PC and KakuriZai's development environment need their own measurements. And of course, building UE still requires the CPU, memory, and disk I/O that the actual job demands; a KVM-based setup also assumes that KVM is available on the Linux host running Cube.

When actually creating a World, we first decide the Cube base template and the required resources, then generate a creation request containing mounts, networking, and other settings. On the regular CubeMaster path, this is submitted with `cubemastercli multirun --norm request.json`.

The first trouble we ran into here was the relationship between templates and disk capacity. When deriving a sandbox from a template, creation failed with the following error:

```
'rootfs-gen0' is a snapshot; cannot resize
```

We had expected that changing the disk size in the creation request would let the template be built at the new capacity, but instead the process blocked at the snapshot resize step. For scenarios that need development tools and a large source tree installed, being blocked before startup is a real obstacle — and this is a problem any user who adjusts disk capacity on demand can hit.

The current resolveTemplateForResources() checks five items: writable disk capacity, CPU, memory, instance type, and network type. If the original template does not match, it looks for a compatible template built from the same base image; if one exists it reuses it, otherwise it builds a new one from the base image. Building a compatible template is an asynchronous task, and the client polls the task status until READY or FAILED, with a timeout of 20 minutes.

Another design that improves the development experience: World creation does not fail even when cubecli is not installed on the host. The sandbox is recorded normally — including its metadata, the upper/work/whiteout paths, and the complete generated Cube request — with its runtime state marked as planned, to be actually created once Cube becomes available. Environment definition and actual execution are therefore decoupled, and this graceful degradation also makes configuration management possible on hosts without Cube.

## 3. Overlaying Lower and Upper Inside the Guest

Once the sandbox is created, the lower and upper layers passed from the host need to become the working directory inside the guest. The original files must be readable, writes must land in the World's own upper layer, and changes to the original source must wait for apply — for this semantics to hold, it is not enough for the directories to be visible; the overlaid filesystem must genuinely support real file operations.

The initial implementation started with kernel overlayfs, and FUSE support was added later. On the regular CubeMaster path today, setupOverlay() runs after creation and tries candidates in the following order:

1. kernel overlayfs
2. unionfs-fuse
3. fuse-overlayfs

All mount points must live under /workspace inside the guest, and any path outside that boundary is rejected outright. This constraint keeps the file scope a sandbox can access very tight.

### Handling Deletion

When a file from the lower layer is deleted inside the sandbox, the original file is not deleted directly; instead, a whiteout marker is left behind. When changes are adopted later, this deletion record also needs to be read correctly.

There is a trap that is easy to step into here: overlay-style whiteouts are file names starting with `.wh.`, while unionfs-fuse appends `_HIDDEN~` to file names under the `.unionfs-fuse` directory — the two representations are not the same. If you assume copying the upper layer as-is is enough, deletions will not be reflected, and the management files will get mixed into the source tree along the way.

To handle this, we added unionfs-fuse whiteout reading to KakuriZai's change collection: deletions are passed to apply as deletions, and management files are excluded from adoption. Every time a mount backend is added, the side that reads changes must adapt in step — this is a hidden cost of maintaining multiple mount backends, and worth planning for early.

Depending on how the host's files are meant to be used, the mount mode can also be chosen:

| Mode | Behavior |
|:--|:--|
| agctl-overlay | Overlay each World's writable layer on top of the read-only source |
| cubesandbox-readonly | Present the source directly, read-only |
| unsafe-rw | Write directly into the original source; changes do not wait for apply |

Our advice is to first validate the complete flow — reading, modifying, creating, deleting, and applying an existing file — on a small directory. Once you have confirmed the behavior of every file operation you will actually use, it becomes much easier to isolate problems after migrating a large project.

## 4. Advanced Usage: Multi-Sandbox Networking and Stateful Forking

Once the execution environment worked, Cube's networking capabilities came into play as well. KakuriZai uses Cube's `network_type=tap` directly in the creation request, and exposed ports, DNS configuration, and the cube_network_config egress policy are all delivered through the request. Egress control is enforced on the host side with iptables chains (KAKURIZAI-EGRESS / KAKURIZAI-INGRESS); for sandboxes without external access, drop rules are constructed from the full set of public CIDRs. Studio's network view probes ICMP/TCP reachability from each configured sandbox, using each sandbox's Cube runtime IP, and renders the reachable and blocked relationships between sandboxes as a topology.

This networking capability also supports a more advanced use case: a multi-sandbox Kubernetes lab. Studio's Create K8s Lab flow creates control-plane and worker sandboxes in a batch, sharing the same TAP egress policy, exposed API/NodePorts, CNI, pod/service CIDRs, and join token. The generated workspaces carry kakurizai.kubernetes.\* annotations and labels for the runtime bootstrapper to consume. A small K8s cluster made of multiple Cube sandboxes can be created and managed just like a single World.

At the environment level, once the development tools and dependencies are in place, it is natural to want to branch out multiple experiments directly from that state. This is where snapshots and stateful replication come in: they skip the repeated environment setup while letting you try different changes. `agctl replicate` uses the stateful mode by default, which captures the state of the running environment first and then creates the replicas. After registering the placement nodes, the request is issued like this:

```
agctl replicate experiment-a --replicas 2 --state-mode stateful --json
```

### What a Snapshot Actually Saves

There is one piece of knowledge here that applies to everyone combining host mounts with snapshots: data inside and outside the VM is stored in different places. The lower and upper layers live on the host side — they are data outside the VM. Even if you take a VM snapshot, that data will not necessarily be preserved as an independent backup.

For this reason, the current stateful replication, when host mounts are present, first copies what /workspace presents into /kakurizai/replication-state/workspace inside the guest, then includes that state in the snapshot or template, and restores it on the replication target.

Restore modes are divided as follows:

| Mode | Implementation |
|:--|:--|
| runtime-snapshot | Restore on the original node, including rootfs and memory state |
| template-snapshot | Distribute the committed AppSnapshot template to other nodes; the regular path does not carry over the running RAM |
| stateful | Switch between runtime snapshot and portable template depending on the placement target |
| definition | Create from the saved environment definition; no running state is carried over |

Rootfs, RAM, host mounts, and external services each differ in what can be saved and restored. When reviewing a restore result, you need to check the selected mode and the metadata of the memory capture; when a process stalls, look at cube-replication-materialize.log or the snapshot- and template-related logs.

## Closing Notes

The writable layer in this setup is not free: the copy-up cost of large binaries and the capacity of build artifacts need to be measured in advance. Apply does no three-way merge, so the choice of baseline and the timing of adoption in parallel work are yours to manage. And the official startup performance numbers are a reference only — your own host needs its own measurements.

For readers who want to reproduce this kind of workspace, our advice is to start small: validate the complete flow of read, modify, create, delete, and apply on a small directory first, and confirm that every file operation behaves as expected, before migrating a large project. The cost of troubleshooting grows together with the size of the project.

## References

- [KakuriZai](https://github.com/IPA-CyberLab/IPA-RS-KakuriZai): project positioning, CLI, mounts, stateful replication
- [CubeSandboxOverlayBackend](https://github.com/IPA-CyberLab/IPA-RS-KakuriZai/blob/4aaac549910715eaaf03a2eac6f936e3bc0f434e/src/backends/cubeSandboxOverlay.ts): template resolution before startup, creation results and overlay state management
- [CubeSandboxClient](https://github.com/IPA-CyberLab/IPA-RS-KakuriZai/blob/4aaac549910715eaaf03a2eac6f936e3bc0f434e/src/cube/client.ts): CLI / API connections, exec, overlay probing, snapshot and template distribution
- [Mount configuration (mounts.ts)](https://github.com/IPA-CyberLab/IPA-RS-KakuriZai/blob/4aaac549910715eaaf03a2eac6f936e3bc0f434e/src/core/mounts.ts): mount modes and the /workspace path constraint
- [CubeSandbox official README](https://github.com/TencentCloud/CubeSandbox) and [performance report](https://github.com/TencentCloud/CubeSandbox/blob/e02976ae54729fb1684c5b51535381e54df1aea2/docs/blog/posts/2026-06-01-cubesandbox-perf-benchmark.md): benchmark data and evaluation conditions
