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

Template memory snapshots carry a shim-version tag: pre-upgrade templates pin v0.7.0 (their sandboxes still get the v0.7.0 shim), templates registered after the upgrade get tagged v0.7.3 and hit the new snapshot machinery. A guest agent resurrected from a memory snapshot never runs the CreateContainer path, so the agent-side `reset_log_stream_readers` fix (agent/src/rpc.rs) never fires — the log-stream read ends are stale and the shim-side forwarder spins on empty reads. Note the product exec API itself is not broken: CubeMaster `/cube/sandbox/exec` is fire-and-forget (dummy IO, no output collection); what breaks is reading output back from a restored agent, a behavior the product never relied on.

## Resolution

Controlled downgrade of the shim slot — replace the v0.7.3 binary with the v0.7.0 one on every node, leaving the rest of the v0.7.3 stack untouched:

```sh
cd /data/cubelet/root/component_versions/cube-shim/v0.7.3/bin
cp containerd-shim-cube-rs containerd-shim-cube-rs.v073.bak
cp ../v0.7.0/bin/containerd-shim-cube-rs /tmp/.s && chmod +x /tmp/.s
mv /tmp/.s containerd-shim-cube-rs   # plain cp fails with "Text file busy" while shims run
```

Running sandboxes are unaffected (they keep the old inode); new sandboxes fall back to the v0.7.0 shim. Verify by launching a sandbox from a post-upgrade template — exec output comes back. Caveat for triage: the GitHub compare API truncates at 300 files, so verify tag-to-tag CubeShim/hypervisor changes via tree shas or a source diff, not the compare file list.

## References

- Related code: agent/src/rpc.rs (`reset_log_stream_readers`), CubeShim/shim/src/container/exec.rs
- Related issue: #1616 (RPC log forward after template create)
