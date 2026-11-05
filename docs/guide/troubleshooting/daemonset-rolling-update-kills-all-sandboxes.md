---
title: Rolling the cube-node DaemonSet kills every sandbox on the node
author: JJan11
date: 2026-10-22
tags:
  - troubleshooting
lang: en-US
---

# Rolling the cube-node DaemonSet kills every sandbox on the node

## Symptom

On a Kubernetes deployment, anything that recreates the `cube-node` Big Pod (`helm upgrade` touching the pod spec, `kubectl set image`, node rescheduling) silently kills **every running sandbox on that node**. All sandbox VMs vanish at once. Afterwards `GET /sandboxes` keeps reporting them as `running` — stale entries that are never reconciled. We hit this twice while rolling an exec sidecar; each roll wiped all production sandboxes on the node.

## Environment

- Cube Sandbox version: v0.7.3 (chart cube-0.7.3), seen since v0.7.0-era K8s form
- Deployment mode: Kubernetes, `cubeNode.hostNetwork: true`, chart-default `security.hostPID: true` (verified live)
- Host OS / kernel: CentOS Stream 9, 6.x kernel
- Related components: cube-node DaemonSet, containerd-shim-cube-rs

## Root Cause

The exact kill path needs confirmation from the maintainers — what is verified in production is the outcome and the trigger. The naive "PID-namespace teardown" theory does **not** apply here: the chart runs the Big Pod with `security.hostPID: true`, so shims share the host PID namespace and pod recreation alone cannot kill them.

What we did observe: during pod replacement, the new container's `component-entrypoint.sh` terminates the previous node components (we captured `component-entrypoint.sh: line 1082: ... Killed "${bin}"` in cubelet logs at the same moment the shims disappeared). With `hostPID: true` a process-group kill or an explicit sweep of the old component tree reaches every `containerd-shim-cube-rs` on the node. Notably the documented upgrade path (`docs/guide/kubernetes/upgrade.md`) routes shim/kernel/guest upgrades through the `cube-node-installer` DaemonSet precisely without recreating the Big Pod — recreating it appears to be the unsupported path where this happens. After the shims die, nothing reconciles the stale `running` entries (same family as #1958).

## Resolution

1. **Prefer the supported path**: upgrade node-side artifacts (shim/kernel/guest) through the `cube-node-installer` DaemonSet, which does not recreate the Big Pod (`docs/guide/kubernetes/upgrade.md`). Only cubelet/runtime image changes recreate it, and those require a drain per the upgrade guide.
2. If you must recreate the Big Pod anyway, treat it as destructive: record each sandbox's templateID / volumeMounts / timeout beforehand; afterwards `DELETE` the stale `running` entries and recreate the sandboxes with the same parameters (volume data survives, guest memory state is lost).
3. In our deployment we iterate our own sidecar via a self-replace (`/self-upgrade` style) endpoint so the pod is never recreated — this is our operational choice, not an upstream mechanism.
4. Verify before/after with:
   ```sh
   ps aux | grep containerd-shim | grep -oE '\-id [0-9a-f]{8}' | sort
   ```
   and diff against the sandbox list.

## References

- Related issue: #1958 (same pod-recreation family, eBPF port mappings)
- Related issue: #1890 (shim lifecycle on failed creates)
