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
- Deployment mode: Kubernetes, `cubeNode.hostNetwork: true`
- Host OS / kernel: CentOS Stream 9, 6.x kernel
- Related components: cube-node DaemonSet, containerd-shim-cube-rs

## Root Cause

`containerd-shim-cube-rs` (with the embedded VMM) is visible in host `ps`, but it lives in the cubelet container's **PID namespace**. When the pod is recreated, kubelet tears the whole PID namespace down and every process inside it — including each sandbox's shim/VM — is SIGKILLed. Sandbox lifetime is coupled to pod lifetime by design in the K8s form. This is the same family as #1958 (orphaned eBPF port mappings after pod recreation): both are host state coupled to the pod lifecycle without a reconciliation path.

## Resolution

1. **Hard rule**: never roll the cube-node DS on a production cluster — iterate node-side components via hot upgrade (drop the new binary on the host and let the process replace itself; the pod stays put).
2. When a roll is unavoidable (version upgrade), run it in a planned window with a sandbox rebuild procedure ready:
   - record each sandbox's templateID / volumeMounts / timeout beforehand;
   - after the roll, `DELETE` the stale `running` entries and recreate with the same parameters (`POST /sandboxes`); volume data survives, guest memory state is lost.
3. Verify before/after with:
   ```sh
   ps aux | grep containerd-shim | grep -oE '\-id [0-9a-f]{8}' | sort
   ```
   and diff against the sandbox list.

## References

- Related issue: #1958 (same pod-recreation family, eBPF port mappings)
- Related issue: #1890 (shim lifecycle on failed creates)
