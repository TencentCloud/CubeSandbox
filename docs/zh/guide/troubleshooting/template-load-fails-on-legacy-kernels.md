---
title: 旧内核上模板加载失败
author: March-77
date: 2026-07-23
tags:
  - troubleshooting
lang: zh-CN
---

# 旧内核上模板加载失败

## 问题现象

在较老 Linux 内核版本或经过安全加固的镜像上部署 CubeSandbox 后，加载新模板时出现：

- `template creation timeout`
- `kvm: failed to initialize vmm`
- 沙箱启动后立刻退出
- `cube-shim-req.log` 只输出一行错误后退出

## 环境信息

- Cube Sandbox 版本：0.4.x（one-click 或自建）
- 部署方式：all-in-one 与 standalone 网关
- 宿主机 OS / 内核：Linux，内核版本较旧或定制加固镜像
- 相关组件：CubeAPI、Cubelet、CubeShim

## 根因分析

旧宿主机、嵌套虚拟化环境或经过安全加固的部署可能无法向 CubeSandbox
提供 KVM。常见原因包括 CPU 虚拟化未启用、KVM 模块未加载，或服务权限
阻止访问 `/dev/kvm`。

此类问题常被表现为泛化超时，掩盖了宿主能力不足的真实原因。

## 解决方案

### 1. 在模板加载前先验证主机能力

```bash
# CPU 虚拟化支持
grep -E "vmx|svm" /proc/cpuinfo | head

# KVM 模块
lsmod | grep -i kvm

# 检查 /dev/kvm 权限
ls -l /dev/kvm

# 查看 Cubelet 最近日志
journalctl -u cube-sandbox-cubelet -n 200 --no-pager
```

期望结果：

- `grep` 能看到 `vmx`（Intel）或 `svm`（AMD）
- `lsmod` 能看到 `kvm`（及 `kvm_intel` / `kvm_amd`）
- `/dev/kvm` 对服务账号可访问

### 2. 修复宿主能力或改用 PVM

模板 Dockerfile 无法在宿主机上启用 KVM，因此应先修复宿主环境：

1. 在固件或云实例配置中启用 CPU 虚拟化。
2. 加载对应的 `kvm_intel` 或 `kvm_amd` 模块，并允许 Cubelet 服务访问
   `/dev/kvm`。
3. 如果云环境无法提供 KVM 或可靠的嵌套虚拟化，请使用
   [PVM 部署](../pvm-deploy.md)。

自定义镜像应基于受支持的 CubeSandbox 基础镜像重建，并固定标签以保证可复现。
除非组件文档明确说明并实现了相应变量，否则不要自行添加兼容性环境变量。

```dockerfile
FROM ghcr.io/tencentcloud/cubesandbox-base:2026.16

# 只安装工作负载所需的软件包。
RUN apt-get update \
    && apt-get install -y --no-install-recommends python3 \
    && rm -rf /var/lib/apt/lists/*
```

### 3. 快速冒烟验证

注册重建后的镜像，等待模板就绪，再运行一条短命令：

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

如仍失败，建议直接比对沙箱与 Shim 日志：

```bash
cubecli logs <sandbox-id>
cubecli logs <sandbox-id> --stderr
```

### 4. 使用模板分层策略避免环境回归

对于混合内核集群，建议保留两套模板：

- **兼容模板**：固定受支持的基础镜像，依赖更少
- **性能模板**：功能更丰富，面向现代内核

这样可按主机特征路由到匹配模板，避免一个模板覆盖所有场景。

## 参考资料

- 相关 Issue：
  - https://github.com/TencentCloud/CubeSandbox/issues/241
- 相关文档：
  - [部署相关排障](./deployment.md)
  - [自定义镜像接入](../tutorials/bring-your-own-image.md)
  - [如何查看 CubeSandbox 组件日志（含 cubecli logs 与 guest kernel log）](./component-log-locations.md)
