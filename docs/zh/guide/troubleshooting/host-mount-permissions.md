---
title: Host Mount 写入时报 Permission Denied
author: barry166
date: 2026-06-14
tags:
  - runtime
  - host-mount
  - permissions
lang: zh-CN
---

# Host Mount 写入时报 Permission Denied

## 问题现象

沙箱里可以看到 host mount 目录，但写入时报 `Permission denied`：

```text
/bin/bash: line 1: /mnt/rw/1.txt: Permission denied
```

常见复现方式是创建了一个可写 host mount：

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

## 环境信息

- Cube Sandbox 版本：v0.4.0 或更新版本
- 部署模式：one-click、裸金属或多机集群部署
- 宿主机 OS / 内核：运行 Cubelet 的 Linux 宿主机
- 相关组件：Cubelet、host mount metadata、沙箱命令执行用户

## 根因分析

`metadata["host-mount"]` 会把 Cubelet 宿主机上的已有路径映射到沙箱内。它不会复制文件、创建工作区快照，也不会自动改写文件 owner。

对于读写挂载，Linux 文件权限仍然生效：

- `hostPath` 必须已经存在于运行该沙箱的 Cubelet 节点上。
- 挂载目录会保留宿主机侧的 owner、group 和 mode bits。
- 沙箱里的命令默认以沙箱用户身份运行。
- 如果宿主机目录属于另一个 UID/GID，沙箱用户可能能读到目录，但不能写入。

例如 `/tmp/rw` 在宿主机上属于 `uid=1002`，但沙箱命令以 `uid=1000` 运行，即使 `readOnly` 是 `False`，写 `/mnt/rw` 也可能失败。

这是预期的 Linux 挂载行为。`readOnly` 只控制 Cube 用只读还是读写方式挂载目录；它不会绕过宿主机文件系统权限。

## 解决方案

先确认 host path 确实存在于 Cubelet 节点上：

```bash
sudo test -d /tmp/rw
sudo stat -c 'owner=%u:%g mode=%a path=%n' /tmp/rw
```

然后根据场景选择一种处理方式。

### 方案 1：代码工作区优先使用只读挂载

如果 agent 只需要读取和分析代码，建议把工作区只读挂载进沙箱：

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

这是把宿主机仓库共享给沙箱时最安全的默认方式。

### 方案 2：读写挂载时对齐目录 owner

如果沙箱必须把结果写回宿主机目录，需要让该目录对沙箱命令用户可写。

常见做法是准备一个由沙箱 UID/GID 拥有的专用宿主机目录：

```bash
sudo mkdir -p /tmp/rw
sudo chown 1000:1000 /tmp/rw
sudo chmod 0755 /tmp/rw
```

然后按读写方式挂载：

```python
mounts = json.dumps([
    {
        "hostPath": "/tmp/rw",
        "mountPath": "/mnt/rw",
        "readOnly": False,
    },
])
```

如果你的沙箱镜像使用了不同用户，可以在沙箱内检查：

```python
with Sandbox.create(metadata={"host-mount": mounts}) as sandbox:
    result = sandbox.commands.run("id && stat -c '%u:%g %a %n' /mnt/rw")
    print(result.stdout)
```

再用实际输出的 UID/GID 准备宿主机目录。

### 方案 3：宿主机 owner 不能改时使用 ACL

如果不能修改目录 owner，可以用 POSIX ACL 给沙箱 UID 授权：

```bash
sudo setfacl -m u:1000:rwx /tmp/rw
sudo setfacl -d -m u:1000:rwx /tmp/rw
```

这样可以保留原有宿主机 owner，同时允许沙箱用户写入新文件。

### 方案 4：仅在合适场景下用 root 写入

对于管理类操作，如果 SDK 路径和安全策略允许，可以指定以 `root` 执行命令：

```python
with Sandbox.create(metadata={"host-mount": mounts}) as sandbox:
    sandbox.commands.run("echo hello > /mnt/rw/1.txt", user="root")
```

只建议在可信 workload 中使用。它可能在 host mount 中创建 root-owned 文件，后续宿主机工具处理这些文件时可能会遇到权限差异。

## 工作区映射注意事项

Host mount 是节点本地能力。在多机集群中，`hostPath` 必须存在于实际调度运行该沙箱的 Cubelet 节点上。

如果希望沙箱使用和本地开发机一致的仓库，可以选择：

- 先把仓库同步到每个 Cubelet 节点上的固定目录，再挂载该目录。
- 使用共享存储，并确保所有 Cubelet 节点上路径一致。
- 如果沙箱只需要固定版本的代码快照，可以把仓库构建进 template image。
- 如果工作区较小且不需要写回宿主机，可以在运行时把文件复制进沙箱。

不要只依赖 `host-mount` 把本地笔记本上的文件传到远端 Cubelet 节点。

## 参考

- 相关 issue：[#239](https://github.com/TencentCloud/CubeSandbox/issues/239)
- 示例：[`examples/host-mount`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/host-mount)
