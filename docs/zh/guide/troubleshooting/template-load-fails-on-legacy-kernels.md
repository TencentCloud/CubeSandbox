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

部分旧内核和加固镜像缺少 KVM 兼容能力（`CONFIG_KVM` / `CONFIG_USER_NS`）或采用了更严格的 `seccomp` 约束，导致模板创建后在 VM 启动阶段被拒绝。

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

### 2. 使用显式启动参数重建模板

选择已知兼容内核栈的模板基础镜像，并在启动阶段放宽过激的运行时限制。

1. 模板 Dockerfile 中避免自定义 `ENTRYPOINT` 覆盖导致严格 seccomp 的启动路径。
2. 将内核相关配置保持与环境兼容。
3. 将模板启动超时设为 >=180 秒，给第一次启动留出更多准备时间。

```dockerfile
# 示例：降低启动阶段的兼容压力
FROM ghcr.io/tencentcloud/cubesandbox-base:2026.16

# 保持兼容内核与启动行为
ENV CUBEVM_ALLOW_LEGACY_COMPAT=1
ENV CUBEVM_STARTUP_TIMEOUT=180
```

### 3. 快速冒烟验证

```bash
# 1) 重新构建并注册模板
# 2) 用短任务发起一次创建
# 3) 使用 cubecli 拉取模板/沙箱日志确认启动序列
```

如仍失败，建议直接比对沙箱与 Shim 日志：

```bash
cubecli logs <sandbox-id>
cubecli logs <sandbox-id> --stderr
```

### 4. 使用模板分层策略避免环境回归

对于混合内核集群，建议保留两套模板：

- **兼容模板**：更保守的启动参数，依赖更少
- **性能模板**：功能更丰富，面向现代内核

这样可按主机特征路由到匹配模板，避免一个模板覆盖所有场景。

## 参考资料

- 相关 Issue：
  - https://github.com/TencentCloud/CubeSandbox/issues/241
- 相关文档：
  - [部署相关排障](./deployment.md)
  - [如何查看 CubeSandbox 组件日志（含 cubecli logs 与 guest kernel log）](./component-log-locations.md)
- 外部资料：
  - Linux 虚拟化与 KVM 配置文档，以及各 Linux 发行版内核兼容说明
