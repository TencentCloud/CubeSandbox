---
title: Templates registered after an upgrade return empty exec output (exit 0, empty stdout)
author: JJan11
date: 2026-10-22
tags:
  - troubleshooting
lang: en-US
---

# Templates registered after an upgrade return empty exec output (exit 0, empty stdout)

## Symptom

After upgrading v0.7.0 → v0.7.3, templates registered **before** the upgrade keep working, but sandboxes launched from templates registered **after** it show: the command actually ran (the exit code is real), yet reading the output back through containerd task exec (containerd v2 client, `cio` stream collection) returns an **empty stdout/stderr forever** — about 40 ms, `exit_code:0, stdout:""`. Same node, same window: sandboxes on the v0.7.0 shim are completely fine; the only variable is the shim binary.

## Environment

- Cube Sandbox version: v0.7.0 → v0.7.3 (Kubernetes form)
- Deployment mode: Kubernetes chart, both shim versions present under `component_versions/cube-shim/`
- Host OS / kernel: CentOS Stream 9, 6.x kernel
- Related components: CubeShim (containerd-shim-cube-rs), cube-agent

## Root Cause

What is verified is the correlation: the failing sandboxes are exactly those whose template snapshots pin the v0.7.3 shim (post-upgrade registrations), and swapping only the shim binary (below) restores readback — a same-node, same-window controlled experiment leaves the shim binary as the only variable.

The precise internal mechanism still needs maintainer confirmation. Our initial reading (an agent resurrected from a memory snapshot never runs CreateContainer, so the `reset_log_stream_readers` fix in agent/src/rpc.rs never fires) is **incomplete**: restore-from-template does reach CreateContainer with the `cube.appsnapshot.container.id` stamp (Cubelet/plugins/cbri/.../cubebox.go), which is where the reset is called. Something in the v0.7.3 snapshot/restore path leaves the log-stream readback dead despite that reset — we report the symptom, the controlled experiment, and the workaround, and leave the exact break to #2015.

Note the product exec API itself is not affected: CubeMaster `/cube/sandbox/exec` is fire-and-forget (dummy IO); we verified commands really run inside the guest. What breaks is output readback via containerd task exec — a behavior external tooling may rely on.

## Resolution

Controlled downgrade of the shim slot — replace the v0.7.3 binary with the v0.7.0 one, leaving the rest of the v0.7.3 stack untouched:

```sh
cd /data/cubelet/root/component_versions/cube-shim/v0.7.3/bin
cp containerd-shim-cube-rs containerd-shim-cube-rs.v073.bak
# stage the replacement INSIDE the target directory, then mv: same-filesystem
# rename(2) is atomic; staging under /tmp (often tmpfs) degrades mv to
# copy+unlink and re-hits ETXTBSY, plus drags a /tmp SELinux label along.
cp ../v0.7.0/bin/containerd-shim-cube-rs ./.stage && chmod +x ./.stage
mv ./.stage containerd-shim-cube-rs
```

**Caveats (know the trade-off):**
- Running sandboxes keep the old inode and are unaffected; new sandboxes get the v0.7.0 shim behavior. Verified in production across mixed workloads.
- A snapshot binds the shim **and its sibling cube-runtime** as a pair (`cubebox.proto`: `cube-shim (and sibling cube-runtime) version bound when this snapshot was created`). After this swap the "v0.7.3" slot holds a v0.7.0 shim with a v0.7.3 cube-runtime and `version.json` still says v0.7.3 — a deliberately mixed slot that works in practice here but does not self-heal and will confuse version reporting. Treat it as a bridge until the underlying defect is fixed, not a permanent state.
- The node artifact redistributor may restore the original binary; re-apply after component redistribution.

Triage tip from the same incident: the GitHub compare API truncates at 300 files — verify tag-to-tag CubeShim/hypervisor changes via tree shas or a source diff, not the compare file list.

## References

- Related code: agent/src/rpc.rs (`reset_log_stream_readers`), CubeShim/shim/src/container/exec.rs
- Related issue: #1616 (RPC log forward after template create)
