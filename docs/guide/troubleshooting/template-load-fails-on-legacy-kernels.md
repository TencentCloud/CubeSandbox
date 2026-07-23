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

Older hosts, nested-virtualization environments, and hardened deployments can
leave KVM unavailable to CubeSandbox. Common causes include disabled CPU
virtualization, unloaded KVM modules, or service permissions that block
`/dev/kvm`.

Because the failure happens during template bootstrap, it is often reported as
a generic timeout, which masks the actual host capability issue.

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

### 2. Fix the host capability or use PVM

A template Dockerfile cannot enable KVM on its host. Fix the host first:

1. Enable CPU virtualization in firmware or the cloud instance configuration.
2. Load the appropriate `kvm_intel` or `kvm_amd` module and grant the Cubelet
   service access to `/dev/kvm`.
3. If the cloud environment cannot expose KVM or reliable nested
   virtualization, use the [PVM deployment](../pvm-deploy.md).

For custom images, rebuild from the supported CubeSandbox base and pin its tag
for reproducibility. Do not add compatibility environment variables unless they
are documented by the component that reads them.

```dockerfile
FROM ghcr.io/tencentcloud/cubesandbox-base:2026.16

# Add only the packages required by your workload.
RUN apt-get update \
    && apt-get install -y --no-install-recommends python3 \
    && rm -rf /var/lib/apt/lists/*
```

### 3. Validate quickly with a tiny smoke test

Register the rebuilt image, wait for the template to become ready, and run one
short command:

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/cube-compat:2026.16 \
  --writable-layer-size 1G \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health

cubemastercli tpl watch --job-id <job-id>

CUBE_TEMPLATE_ID=<template-id> python3 - <<'PY'
import os
from e2b import Sandbox

with Sandbox.create(template=os.environ["CUBE_TEMPLATE_ID"]) as sandbox:
    result = sandbox.commands.run("echo cube-ready", timeout=30)
    print(result.stdout)
PY
```

If the template still fails, compare sandbox and shim logs directly:

```bash
cubecli logs <sandbox-id>
cubecli logs <sandbox-id> --stderr
```

### 4. Use template clone strategy for known-good baseline

When your cluster has mixed kernels, keep two template tracks:

- **Compat template**: pinned supported base image + minimal dependency set
- **Performance template**: richer environment for modern nodes

This lets CI/ops route workloads to the right template instead of forcing all jobs onto one boot profile.

## References

- Related issue:
  - https://github.com/TencentCloud/CubeSandbox/issues/241
- Related docs:
  - [Deployment Troubleshooting](./deployment.md)
  - [Bring Your Own Image](../tutorials/bring-your-own-image.md)
  - [How to Find CubeSandbox Component Logs](./component-log-locations.md)
