# Real Environment Verification Guide

This guide helps you verify all demos against a real CubeSandbox deployment and capture the logs/screenshots required by the PR review (per [fslongjin's request](https://github.com/TencentCloud/CubeSandbox/pull/748#issuecomment-4933134670)).

## Prerequisites

- A running CubeSandbox cluster
- `cubemastercli` on `$PATH`, connected to the cluster
- Docker on the build workstation, with a registry the Cube nodes can pull from

## Step 1: Build and register the template

```bash
# Build the image
docker build --platform linux/amd64 \
  -t <your-registry>/rust-playground:latest \
  examples/rust-playground

# Push to your registry
docker push <your-registry>/rust-playground:latest

# Register as a Cube template
cubemastercli tpl create-from-image \
  --image <your-registry>/rust-playground:latest \
  --writable-layer-size 4G \
  --expose-port 49983 \
  --probe       49983 \
  --probe-path  /health

# Watch until READY
cubemastercli tpl watch --job-id <job_id>

# Note the template_id
cubemastercli tpl list | grep rust-playground
```

## Step 2: Configure the host

```bash
cd examples/rust-playground
cp .env.example .env
# Edit .env:
#   E2B_API_URL=http://<cube-node>:3000
#   CUBE_TEMPLATE_ID=<template-id-from-step-1>
pip install -r requirements.txt
```

## Step 3: Run each demo and capture output

### Demo 1: Stateful Workspace Management

```bash
# Run and capture output
python parallel_workspaces.py 2>&1 | tee verification-logs/parallel_workspaces.log

# Capture sandbox state
cubemastercli sandbox list >> verification-logs/parallel_workspaces.log
```

Take a screenshot of the terminal showing:
- The full output of `parallel_workspaces.py`
- The `cubemastercli sandbox list` output showing created sandboxes

### Demo 2: Egress Network Policy Enforcement

```bash
python network_isolation.py 2>&1 | tee verification-logs/network_isolation.log
```

Take a screenshot of the terminal showing:
- sb-1 (online) build succeeds
- sb-2 (offline) build is blocked

### Demo 3: Checkpoint-Driven Development

```bash
python snapshot_driven_dev.py 2>&1 | tee verification-logs/snapshot_driven_dev.log
```

Take a screenshot of the terminal showing:
- All 5 phases executing successfully
- Rollback timing (should be sub-100ms)

### Demo 4: Multi-Sandbox Collaboration

```bash
python multi_container.py 2>&1 | tee verification-logs/multi_container.log
```

Take a screenshot of the terminal showing:
- Builder sandbox compiles with internet access
- Binary read by host SDK
- Runner sandbox executes binary without internet

## Step 4: Include in PR

Add the following to the PR description or as a PR comment:

```markdown
## Real Environment Verification

### Environment
- CubeSandbox version: <version>
- Deployment: <single-node / multi-node>
- Template ID: <template-id>

### Screenshots

| Demo | Screenshot | Log |
|------|------------|-----|
| Stateful Workspace | ![screenshot](verification-logs/screenshots/parallel_workspaces.png) | [log](verification-logs/parallel_workspaces.log) |
| Egress Policy | ![screenshot](verification-logs/screenshots/network_isolation.png) | [log](verification-logs/network_isolation.log) |
| Checkpoint Dev | ![screenshot](verification-logs/screenshots/snapshot_driven_dev.png) | [log](verification-logs/snapshot_driven_dev.log) |
| Multi-Sandbox | ![screenshot](verification-logs/screenshots/multi_container.png) | [log](verification-logs/multi_container.log) |

### Results Summary

| Demo | Status | Notes |
|------|--------|-------|
| parallel_workspaces.py | ✅ PASS | 3 workspaces created concurrently, lifecycle pause/resume working |
| network_isolation.py   | ✅ PASS | Online build succeeds, offline build blocked by egress policy |
| snapshot_driven_dev.py | ✅ PASS | Snapshot outlives sandbox, rollback in ~Xms, clone(n=3) works |
| multi_container.py     | ✅ PASS | Builder pulls crates, runner executes binary air-gapped |
```
