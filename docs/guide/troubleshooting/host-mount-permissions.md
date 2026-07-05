---
title: Host Mounts Fail with Permission Denied
author: barry166
date: 2026-06-14
tags:
  - runtime
  - host-mount
  - permissions
lang: en-US
---

# Host Mounts Fail with Permission Denied

## Symptom

A sandbox can see a host-mounted directory, but writing to it fails with `Permission denied`:

```text
/bin/bash: line 1: /mnt/rw/1.txt: Permission denied
```

This often appears after creating a sandbox with a writable host mount:

```python
import json
from cubesandbox import Sandbox

mounts = json.dumps([
    {
        "hostPath": "/tmp/rw",
        "mountPath": "/mnt/rw",
        "readOnly": False,
    },
])

with Sandbox.create(metadata={"host-mount": mounts}) as sandbox:
    sandbox.commands.run("echo hello > /mnt/rw/1.txt")
```

## Environment

- Cube Sandbox version: v0.4.0 or later
- Deployment mode: one-click, bare-metal, or multi-node deployment
- Host OS / kernel: Linux host running Cubelet
- Related components: Cubelet, host mount metadata, sandbox command user

## Root Cause

`metadata["host-mount"]` maps an existing path on the Cubelet host into the sandbox. It does not copy files, create a workspace snapshot, or rewrite ownership.

For a read-write mount, Linux permissions still apply:

- `hostPath` must already exist on the Cubelet node that runs the sandbox.
- The mounted directory keeps its host-side owner, group, and mode bits.
- Commands inside the sandbox run as the sandbox user by default.
- If the host directory is owned by a different UID/GID, the sandbox user may be able to read the path but not write to it.

For example, if `/tmp/rw` is owned by host user `uid=1002`, but the sandbox command runs as `uid=1000`, a write to `/mnt/rw` can fail even when `readOnly` is `False`.

This is expected Linux mount behavior. The `readOnly` flag controls whether Cube mounts the path read-only or read-write; it does not grant write permission to users that the host filesystem would reject.

## Resolution

First, confirm the host path exists on the Cubelet node:

```bash
sudo test -d /tmp/rw
sudo stat -c 'owner=%u:%g mode=%a path=%n' /tmp/rw
```

Then choose one of the following patterns.

### Option 1: Use a read-only mount for source workspaces

For agent workloads that only need to inspect code, mount the workspace read-only:

```python
import json
from cubesandbox import Sandbox

mounts = json.dumps([
    {
        "hostPath": "/srv/workspaces/my-repo",
        "mountPath": "/workspace",
        "readOnly": True,
    },
])

with Sandbox.create(metadata={"host-mount": mounts}) as sandbox:
    result = sandbox.commands.run("ls /workspace")
    print(result.stdout)
```

This is the safest default for sharing a host repository with a sandbox.

### Option 2: Align ownership for read-write mounts

If the sandbox must write back to the host directory, make the host directory writable by the sandbox command user.

One common setup is to create a dedicated host directory owned by the sandbox UID/GID:

```bash
sudo mkdir -p /tmp/rw
sudo chown 1000:1000 /tmp/rw
sudo chmod 0755 /tmp/rw
```

Then mount it read-write:

```python
mounts = json.dumps([
    {
        "hostPath": "/tmp/rw",
        "mountPath": "/mnt/rw",
        "readOnly": False,
    },
])
```

If your sandbox image uses a different user, check it from inside the sandbox:

```python
with Sandbox.create(metadata={"host-mount": mounts}) as sandbox:
    result = sandbox.commands.run("id && stat -c '%u:%g %a %n' /mnt/rw")
    print(result.stdout)
```

Use the reported UID/GID when preparing the host directory.

### Option 3: Use ACLs when the host owner must stay unchanged

If you cannot change the directory owner, grant write access to the sandbox UID with POSIX ACLs:

```bash
sudo setfacl -m u:1000:rwx /tmp/rw
sudo setfacl -d -m u:1000:rwx /tmp/rw
```

This keeps the existing host owner while allowing the sandbox user to write new files.

### Option 4: Run the write as root only when appropriate

For administrative operations, you can run the command as `root` if your SDK path and policy allow it:

```python
with Sandbox.create(metadata={"host-mount": mounts}) as sandbox:
    sandbox.commands.run("echo hello > /mnt/rw/1.txt", user="root")
```

Use this only for trusted workloads. It can create root-owned files on the host mount, which may surprise later host-side tools.

## Workspace Mapping Notes

Host mounts are node-local. In a multi-node cluster, the `hostPath` must exist on the Cubelet node where the sandbox is scheduled.

If you want the sandbox to work with the same repository as your local development machine, use one of these approaches:

- Sync the repository to a known directory on each Cubelet node, then mount that directory.
- Place the repository on shared storage that is mounted at the same path on every Cubelet node.
- Build the repository into a template image if the sandbox only needs a fixed snapshot.
- Copy files into the sandbox at runtime if the workspace is small and does not need host-side write-back.

Do not rely on `host-mount` alone to transfer files from your laptop to a remote Cubelet node.

## References

- Related issue: [#239](https://github.com/TencentCloud/CubeSandbox/issues/239)
- Example: [`examples/host-mount`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/host-mount)
