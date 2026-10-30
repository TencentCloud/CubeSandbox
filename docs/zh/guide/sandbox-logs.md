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
| 沙箱 stdout | `/data/cubelet/log/<sandbox-id>/stdout` |
| 沙箱 stderr | `/data/cubelet/log/<sandbox-id>/stderr` |
| 模板 stdout | `/data/log/template/<template-id>_0/stdout`（宿主机文件系统） |
| 模板 stderr | `/data/log/template/<template-id>_0/stderr`（宿主机文件系统） |

旧版本可能将沙箱日志保存在 Cubelet 的 bundle 挂载命名空间中；`cubecli logs` 也支持读取这些文件。

## 延迟清理沙箱日志

延迟清理默认关闭。部署更新后的 Cubelet 和 CubeShim，并在 Cubelet 的 `config.toml` 中配置：

```toml
[plugins."io.cubelet.internal.v1.cubebox"]
  sandbox_log_cleanup_enabled = true
  sandbox_log_retention = "24h"
  sandbox_log_cleanup_interval = "1m"
```

两个时间参数必须为正数。修改配置后需重启 Cubelet。配置由新启动的 shim 继承，已经运行的 shim 继续使用启动时的配置；关闭功能后，之前开启时启动的 shim 产生的日志可能保留，需要运维人员处理。

Cubelet 确认沙箱运行进程退出后开始计算保留时间。后台扫描在保留期结束且沙箱元数据和 shim 均已消失时清理标记过的日志目录。暂停的沙箱、未标记的日志以及运行状态无法确认的日志会保留。Cubelet 重启后，保留期限仍由日志目录中的标记记录。关闭功能时不会启动扫描，shim 保持原有删除行为。

该功能只保留 CubeShim 转发到宿主机的 init stdout/stderr，不采集仅写在沙箱内部的文件。日志保存在节点本地，会占用磁盘；如需更长时间保存或防止节点损坏导致丢失，请监控磁盘并将日志转存到其他位置。

缺少有效清理标记或命名空间的日志目录不会自动清理，包括开启功能前遗留的日志和旧格式标记的日志；人工清理前应先检查。

信息日志 `sandbox log directory missing` 表示没有可标记的日志目录。沙箱可能在生成 init 日志前失败，也可能由旧版 shim 删除了目录；请检查部署的 shim 版本和沙箱生命周期日志。开启延迟清理时，显式指定的沙箱 ID 必须有效且不能有前后空格。同 ID 的并发操作可能使 Create 等待，或在请求超时后返回可重试的状态错误。

## 范围与限制

- 这些日志仅记录 **init 进程**（容器内 PID 1）的输出。通过 `exec` 接口启动的进程输出需通过 E2B SDK 的 `on_stdout` / `on_stderr` 回调获取，详见 [E2B SDK 文档](https://e2b.dev/docs)。
- 日志转发需要 v0.4.0 及以上版本的 CubeShim。在更旧的部署上，日志文件将不存在。
- 日志目前不支持实时流式读取，暂无 `--follow` 标志。如需查看最新输出，请重新执行命令。
- 默认在沙箱删除后清除日志文件；延迟清理需要主动开启。

## 相关文档

- [服务管理与日志](./service-management.md) — 宿主机服务日志、journalctl 及诊断包
- [模板检查与请求预览](./template-inspection-and-preview.md)
