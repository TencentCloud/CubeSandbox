---
title: 沙箱日志
lang: zh-CN
---

# 沙箱日志

::: warning 功能迭代中
日志能力目前仍在持续迭代，本文介绍的 `cubecli logs` 命令是**临时使用版本**——后续版本可能对接口及底层存储结构进行调整。
:::

CubeSandbox 提供两个互补的日志层：

| 层级 | 记录内容 | 获取方式 |
|---|---|---|
| **沙箱日志** | 容器 init 进程（主入口）的 stdout/stderr | `cubecli logs`（本文） |
| **`envd` 任务日志** | 在沙箱内通过 `exec` 接口启动的子任务的 stdout/stderr | E2B SDK（`on_stdout` / `on_stderr` 回调） |

本文仅介绍**沙箱级别的日志**。`envd` 子任务的日志获取方式请参阅 [E2B SDK 文档](https://e2b.dev/docs)。

## 前置条件

`cubecli` 随 Cubelet 一同构建，并在一键部署时自动安装。`logs` 子命令访问的日志文件位于 **Cubelet 挂载命名空间**内，因此必须**直接在计算节点上执行**，无法通过 API 或非节点主机远程调用。

## 读取沙箱日志

```bash
# 最后 100 行 stdout（默认）
cubecli logs <sandbox-id>

# 最后 100 行 stderr
cubecli logs --stderr <sandbox-id>

# 完整日志（全部行）
cubecli logs --all <sandbox-id>

# 最后 N 行
cubecli logs --tail 50 <sandbox-id>
# 简写
cubecli logs -t 50 <sandbox-id>

# 前 N 行
cubecli logs --head 20 <sandbox-id>
# 简写
cubecli logs -H 20 <sandbox-id>
```

### 参数说明

| 参数 | 简写 | 说明 |
|---|---|---|
| `--stderr` | `-e` | 读取 stderr，默认读取 stdout |
| `--all` | `-a` | 输出全部行；不可与 `--tail` 或 `--head` 同时使用 |
| `--tail N` | `-t N` | 输出最后 N 行（未指定其他标志时默认为 100） |
| `--head N` | `-H N` | 输出前 N 行 |

## 读取模板构建日志

模板构建过程中，容器的 stdout/stderr 会写入宿主机文件系统 `/data/log/template/<templateID>_0/`。这些文件无需进入 Cubelet 挂载命名空间，使用 `--tpl` 标志可跳过命名空间切换：

```bash
# 最后 100 行模板构建 stdout
cubecli logs --tpl <template-id>

# 完整模板构建 stderr
cubecli logs --tpl --all --stderr <template-id>
```

## 日志文件路径

| 场景 | 路径 |
|---|---|
| 沙箱 stdout | `/data/cubelet/state/io.containerd.runtime.v2.task/default/<sandbox-id>/stdout`（Cubelet 挂载命名空间内） |
| 沙箱 stderr | `/data/cubelet/state/io.containerd.runtime.v2.task/default/<sandbox-id>/stderr`（Cubelet 挂载命名空间内） |
| 模板 stdout | `/data/log/template/<template-id>_0/stdout`（宿主机文件系统） |
| 模板 stderr | `/data/log/template/<template-id>_0/stderr`（宿主机文件系统） |

::: tip 为什么需要挂载命名空间？
沙箱日志文件由 CubeShim 写入 bundle 目录，该目录仅在 Cubelet 的私有挂载命名空间内可见。`cubecli logs` 会在读取前自动重新进入该命名空间——你无需任何额外操作，只需在节点上直接运行该命令即可。
:::

## 范围与限制

- 这些日志仅记录 **init 进程**（容器内 PID 1）的输出。通过 `exec` 接口启动的进程输出需通过 E2B SDK 的 `on_stdout` / `on_stderr` 回调获取，详见 [E2B SDK 文档](https://e2b.dev/docs)。
- 日志转发需要 v0.4.0 及以上版本的 CubeShim。在更旧的部署上，日志文件将不存在。
- 日志目前不支持实时流式读取，暂无 `--follow` 标志。如需查看最新输出，请重新执行命令。
- 沙箱删除后，对应的日志文件会一并清除。

## 相关文档

- [服务管理与日志](./service-management.md) — 宿主机服务日志、journalctl 及诊断包
- [模板检查与请求预览](./template-inspection-and-preview.md)
