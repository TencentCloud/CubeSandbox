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

`cubecli` 随 Cubelet 一同构建，并在一键部署时自动安装。请**直接在计算节点上执行**；它优先读取宿主机日志，仅在回退到旧版 bundle 路径时进入 Cubelet 挂载命名空间。

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
| 沙箱 stdout | `/data/cubelet/log/<sandbox-id>/stdout`（宿主机文件系统） |
| 沙箱 stderr | `/data/cubelet/log/<sandbox-id>/stderr`（宿主机文件系统） |
| 模板 stdout | `/data/log/template/<template-id>_0/stdout`（宿主机文件系统） |
| 模板 stderr | `/data/log/template/<template-id>_0/stderr`（宿主机文件系统） |

旧版沙箱可能将日志保存在 Cubelet 挂载命名空间内的 `/data/cubelet/state/io.containerd.runtime.v2.task/default/<sandbox-id>/` bundle 路径；`cubecli` 会自动回退到该路径。

## 销毁后保留日志

默认情况下，沙箱清理时会删除日志。需要保留时，请在复现问题前，**在每个计算节点**创建空标记文件：

```bash
sudo install -d -m 0755 /etc/cube-sandbox
sudo touch /etc/cube-sandbox/retain-sandbox-logs
```

清理时，CubeShim 会把日志目录移动到 `/data/cubelet/log-archive/<sandbox-id>/<segment-id>/logs/`。每次清理（包括暂停和恢复）使用独立分段，避免覆盖以前的输出；shim 异常后由独立删除进程清理时也使用同一标记。如果归档失败，原目录会保留并报告错误。归档目录和原目录必须在同一文件系统中，才能通过重命名移动。`cubecli logs` 读取运行中的路径，归档后的 `stdout` 和 `stderr` 请直接在节点上查看。

归档没有自动过期或大小限制。请配置磁盘告警，将运行中及归档日志发送到独立存储，再管理节点和远端的保留期限。删除标记只会恢复之后清理操作的默认删除行为，不会删除已有归档。归档属于尽力保留：尚未从 guest 转发出的内容以及节点磁盘损坏时的内容无法保证保留。

这里仅保留默认 RPC 转发路径下容器 init 的 stdout/stderr。可选的 passfd I/O 使用不同的输出路径，不在本归档功能范围内。若使用 `cube-entrypoint.sh`，请在 envd 启动前于镜像或模板设置 `ENVD_LOG_FILE=-`，以便包含 envd 输出。自动注入 envd 的包装脚本则写入 guest 内的 `/var/log/envd.log`，需要另行配置或采集。exec 子任务的输出仍需通过 SDK 单独采集。避免记录环境变量值、请求体等敏感信息。

## 范围与限制

- 这些日志仅记录 **init 进程**（容器内 PID 1）的输出。通过 `exec` 接口启动的进程输出需通过 E2B SDK 的 `on_stdout` / `on_stderr` 回调获取，详见 [E2B SDK 文档](https://e2b.dev/docs)。
- 日志转发需要 v0.4.0 及以上版本的 CubeShim。在更旧的部署上，日志文件将不存在。
- 日志目前不支持实时流式读取，暂无 `--follow` 标志。如需查看最新输出，请重新执行命令。
- 默认在沙箱删除时清除日志；如上所述，可选择启用节点本地保留。

## 相关文档

- [服务管理与日志](./service-management.md) — 宿主机服务日志、journalctl 及诊断包
- [模板检查与请求预览](./template-inspection-and-preview.md)
