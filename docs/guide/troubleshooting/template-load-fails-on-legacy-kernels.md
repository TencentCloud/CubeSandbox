---
title: Template Load Fails on Legacy Kernels
author: March-77
date: 2026-07-23
tags:
  - troubleshooting
lang: en-US
---

# Template Load Fails on Legacy Kernels

## Symptom

After deploying CubeSandbox on an older Linux kernel image, loading a newly built template fails intermittently:

- `template creation timeout`
- `kvm: failed to initialize vmm`
- Sandbox exits immediately after start with no additional context
- `cube-shim-req.log` only prints a one-line error and exits

## Environment

- Cube Sandbox version: 0.4.x (one-click or self-deployed)
- Deployment mode: all-in-one and standalone gateway
- Host OS / kernel: Linux with older `5.x` kernels or custom hardened images
- Related components: CubeAPI, Cubelet, CubeShim

## Root Cause

Older kernels and hardened container images sometimes omit KVM userspace compatibility bits (`vfio`, `CONFIG_KVM`, `CONFIG_USER_NS`) or expose stricter `seccomp` defaults. In those environments, Cubelet can create the template object, but envd fails when the VM boots with privileged syscalls blocked.

Because the failure happens during template bootstrap, it is often reported as a generic timeout, which masks the actual host capability issue.

## Resolution

### 1. Confirm host capabilities before loading templates

```bash
# CPU + virtualization support
grep -E "vmx|svm" /proc/cpuinfo | head

# KVM module status
lsmod | grep -i kvm

# Kernel permission for KVM character device
ls -l /dev/kvm

# Cubelet logs around the failure window
journalctl -u cube-sandbox-cubelet -n 200 --no-pager
```

Expected result:

- `grep` shows `vmx` (Intel) or `svm` (AMD)
- `lsmod` shows `kvm` (and `kvm_intel`/`kvm_amd`)
- `/dev/kvm` exists and is accessible to the service account

### 2. Rebuild the template with explicit kernel and sysctl settings

Use a template base image that keeps a known-good kernel stack and disables strict seccomp on the runtime path for the sandbox startup phase.

1. In the template Dockerfile, avoid custom `ENTRYPOINT` wrappers that set aggressive seccomp overrides.
2. Keep `KERNEL_VERSION` pinned to a kernel known to work with CubeSandbox (for example 6.x + envd compatible line).
3. Ensure the sandbox startup timeout in template config is >= 180 seconds for first run in hardened nodes.

```dockerfile
# Example template adjustment
FROM ghcr.io/tencentcloud/cubesandbox-base:2026.16

# Keep a known-good kernel package and user namespace settings
ENV CUBEVM_ALLOW_LEGACY_COMPAT=1
ENV CUBEVM_STARTUP_TIMEOUT=180
```

### 3. Validate quickly with a tiny smoke test

```bash
# 1) Build and register
# 2) Create a minimal container run with a short timeout
# 3) Verify process startup logs are visible
```

If the template still fails, compare sandbox and shim logs directly:

```bash
cubecli logs <sandbox-id>
cubecli logs <sandbox-id> --stderr
```

### 4. Use template clone strategy for known-good baseline

When your cluster has mixed kernels, keep two template tracks:

- **Compat template**: lower startup timeout + minimal dependency set
- **Performance template**: richer environment for modern nodes

This lets CI/ops route workloads to the right template instead of forcing all jobs onto one boot profile.

## References

- Related issue:
  - https://github.com/TencentCloud/CubeSandbox/issues/241
- Related docs:
  - [Deployment Troubleshooting](./deployment.md)
  - [How to Find CubeSandbox Component Logs](./component-log-locations.md)
- External references:
  - Linux virtualization docs and host/kernel compatibility notes for your OS distribution
