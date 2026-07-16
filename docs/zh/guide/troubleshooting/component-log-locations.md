---
title: 如何查看 CubeSandbox 各组件日志（含 cubecli logs 与 guest kernel log）
author: ls-ggg
date: 2026-07-15
tags:
  - logging
  - cubelet
  - cubeshim
  - operations
lang: zh-CN
---

# 如何查看 CubeSandbox 各组件日志（含 cubecli logs 与 guest kernel log）

## 问题现象

CubeSandbox 由多个组件组成（CubeAPI、CubeMaster、Cubelet、CubeShim、Hypervisor/VMM、network-agent、cube-proxy），排障时经常搞不清楚"这条报错该去哪个日志文件里找"，也不知道 guest kernel 的启动日志在哪里查看，以及 Cubelet 默认日志级别较低（`warn`），复现问题时看不到足够细节。

本文汇总一键部署（systemd 托管）场景下，各组件日志的**确切路径**，并说明如何用 `cubecli logs` 查看沙箱/模板日志，如何在 guest kernel 日志里定位启动信息，以及如何给 Cubelet 开启 debug 级别日志。

## 环境信息

- Cube Sandbox 版本：v0.4.0 及以上（一键部署，systemd 托管）
- 部署模式：all-in-one / 多机集群，均适用
- 宿主机 OS / 内核：运行 Cubelet 的 Linux 宿主机
- 相关组件：CubeAPI、CubeMaster、Cubelet、CubeShim、Hypervisor(VMM)、network-agent、cube-proxy

## 根因分析

CubeSandbox 的日志分成三类，来源和查看方式完全不同：

1. **业务行为日志**：请求、调度、审计、VMM 创建过程等，各组件直接写文件到 `/data/log/<Module>/`，**不会**进 `journalctl`。
2. **启动期日志**：进程被 systemd 拉起到稳定运行（或失败退出）期间的 stdout/stderr，只能用 `journalctl -u <unit>` 查看。
3. **容器沙箱/模板日志**：沙箱内 init 进程（容器 PID 1）的 stdout/stderr，由 CubeShim 写入 Cubelet 的私有挂载命名空间内，必须用 `cubecli logs` 读取；模板构建日志则落在宿主机文件系统，无需进命名空间。

guest kernel（虚拟机内核）的启动日志本质上也是一种"容器 init 进程输出"：CubeShim 会把 guest kernel 通过 `console=hvc0` 输出的内容（包括 `Linux version ...` 之类的启动信息）转发进同一条 `cube-shim-req.log`，因此 **guest kernel log 直接在 CubeShim 的 log 里就能看到**，不需要额外工具。

Cubelet 默认日志级别较低（`warn`），常规运行时不会打印详细的调试信息；打开 debug 级别有两种方式：改动态配置文件热更新，或者用内置的 HTTP 接口临时切换（无需重启）。

## 解决方案

### 1. 各组件日志速查表

| 组件 | 日志类型 | 路径 | 查看方式 |
|---|---|---|---|
| CubeAPI | 业务请求日志（按天滚动） | `/data/log/CubeAPI/cube-api-YYYY-MM-DD.log` | `tail -F` |
| CubeMaster | 业务请求日志 | `/data/log/CubeMaster/cubemaster-req.log` | `tail -F` |
| CubeMaster | 启动期日志 | `/data/log/CubeMaster/cubemaster.log` | `tail -F` |
| Cubelet | 业务请求日志 | `/data/log/Cubelet/Cubelet-req.log` | `tail -F` |
| Cubelet | 统计/指标日志 | `/data/log/Cubelet/Cubelet-stat.log` | `tail -F` |
| Cubelet | 启动期日志 | — | `journalctl -u cube-sandbox-cubelet.service` |
| CubeShim | 业务请求日志（含 guest kernel 输出） | `/data/log/CubeShim/cube-shim-req.log` | `tail -F` |
| CubeShim | 统计日志 | `/data/log/CubeShim/cube-shim-stat.log` | `tail -F` |
| Hypervisor (VMM) | VMM 创建过程日志 | `/data/log/CubeVmm/vmm.log` | `tail -F` |
| network-agent | 业务请求日志 | `/data/log/network-agent/network-agent-req.log` | `tail -F` |
| cube-proxy | 访问/错误/sidecar 日志（容器内） | 容器内 `/data/log/cube-proxy/{access,error,sidecar}.log` | `docker exec <容器名> tail ...`（见下文） |
| 沙箱容器 | init 进程 stdout/stderr | `/data/cubelet/state/io.containerd.runtime.v2.task/default/<sandbox-id>/{stdout,stderr}`（Cubelet 挂载命名空间内） | `cubecli logs <sandbox-id>`（见下文） |
| 模板构建 | 构建容器 stdout/stderr | `/data/log/template/<template-id>_0/{stdout,stderr}`（宿主机文件系统） | `cubecli logs --tpl <template-id>` |

::: tip 业务日志不在 journalctl
和 `docs/zh/guide/service-management.md` 中说明的一致：以上业务/请求类日志**全部直接写到 `/data/log/<Module>/`**，`journalctl` 里只能看到进程启动/退出期间很少量的 stdout/stderr。
:::

### 2. 用 `cubecli logs` 查看沙箱 / 模板日志

`cubecli` 随 Cubelet 一同构建，一键部署时自动安装。沙箱日志文件位于 Cubelet 的私有挂载命名空间内，因此 `cubecli logs` **必须直接在计算节点上执行**，无法远程调用。

```bash
# 沙箱 stdout，最后 100 行（默认）
cubecli logs <sandbox-id>

# 沙箱 stderr，最后 100 行
cubecli logs --stderr <sandbox-id>

# 沙箱完整日志
cubecli logs --all <sandbox-id>

# 沙箱最后 N 行 / 前 N 行
cubecli logs --tail 50 <sandbox-id>
cubecli logs --head 20 <sandbox-id>

# 模板构建日志：直接读宿主机文件系统，跳过命名空间切换
cubecli logs --tpl <template-id>
cubecli logs --tpl --all --stderr <template-id>
```

参数说明：

| 参数 | 简写 | 说明 |
|---|---|---|
| `--tpl` | — | 把 id 当作模板 ID，从 `/data/log/template/<id>_0/` 读取，无需进命名空间 |
| `--stderr` | `-e` | 读取 stderr，默认读取 stdout |
| `--all` | `-a` | 输出全部行；不可与 `--tail`/`--head` 同时使用 |
| `--tail N` | `-t N` | 输出最后 N 行（默认 100） |
| `--head N` | `-H N` | 输出前 N 行 |

`cubecli logs` 只覆盖**容器 init 进程（PID 1）**的输出。通过 E2B `exec` 接口在沙箱内启动的子任务，其 stdout/stderr 需要用 E2B SDK 的 `on_stdout`/`on_stderr` 回调获取，不在本文范围内，详见 [沙箱日志](../sandbox-logs.md)。

沙箱删除后，对应日志文件会一并清除；日志转发需要 v0.4.0 及以上版本的 CubeShim。

### 3. Guest kernel 日志：直接在 CubeShim log 里看

CubeShim 在启动虚拟机时，通过内核参数 `console=hvc0` 把 guest 内核的控制台输出接管过来，和沙箱容器 init 进程日志一样，统一写进 CubeShim 的请求日志文件：

```bash
LC_ALL=C sudo grep -a -E "(<sandbox-id 或 InstanceId>|Linux version)" /data/log/CubeShim/cube-shim-req.log
```

`cube-shim-req.log` 是逐行 JSON，日志本体在 `LogContent` 字段里。典型的 guest 内核启动记录形如：

```json
{"Module":"Shim","InstanceId":"<sandbox-id>","ContainerId":"<sandbox-id>","Timestamp":"...","LogContent":"[    0.000000] Linux version 6.12.33-cube.sandbox.pvm.guest-... #2 SMP PREEMPT_DYNAMIC ...","FunctionType":""}
```

按 `InstanceId`（即 sandbox ID）过滤即可拿到对应沙箱完整的内核启动输出，包括设备探测、`EXT4-fs`、`rootfs` mount 等信息，无需额外工具或进入命名空间。

::: tip 为什么在这里，不在别处
`console=hvc0` 是虚拟机的串口控制台，内核所有 `printk` 输出都会走这条通路。CubeShim 把这条控制台流量直接转发进它自己的日志管道（和普通请求日志共用同一个 writer），所以查 guest kernel 日志不需要额外挂载或调试工具，直接 grep `cube-shim-req.log` 即可。
:::

### 4. cube-proxy 容器内日志

`cube-proxy` 是基于 OpenResty 的 nginx 容器，日志写在容器内部的 `/data/log/cube-proxy/`，**不在宿主机文件系统上**，需要 `docker exec` 进容器读取：

```bash
docker exec -it cube-proxy tail /data/log/cube-proxy/error.log
docker exec -it cube-proxy tail /data/log/cube-proxy/sidecar.log
docker exec -it cube-proxy tail /data/log/cube-proxy/access.log
```

容器名固定为 `cube-proxy`（由 systemd 用 `docker create` 创建）；也可以先用 `docker ps` 确认实际容器名/ID 后再替换。

| 文件 | 内容 |
|---|---|
| `access.log` | nginx 访问日志 |
| `error.log` | nginx 错误日志 |
| `sidecar.log` | cube-proxy sidecar 逻辑（鉴权/路由等）日志 |

### 5. 开启 Cubelet debug 级别日志

Cubelet 默认日志级别是 `warn`，排障时往往需要临时调高到 `debug` 才能看到足够的调试信息。有两种方式，二者都**不需要重启 Cubelet 进程**：

#### 方式一：改动态配置文件（持久生效，热加载）

Cubelet 会以 10 秒为周期轮询动态配置文件 `dynamicconf/conf.yaml`，检测到变化后自动热加载并调用日志级别更新逻辑。在 `common:` 段落下加一行 `log_level`：

```yaml
common:
  enable_pf_mode: false
  log_level: debug   # 新增这一行
  sandbox_exec_cmd_time_out: 5s
  ...
```

一键部署环境下该文件的默认路径是：

```
/usr/local/services/cubetoolbox/Cubelet/dynamicconf/conf.yaml
```

保存后等待约 10 秒（配置轮询周期），Cubelet 会自动切到 debug 级别，此后 `/data/log/Cubelet/Cubelet-req.log` 会打出更细粒度的记录。debug 级别可能使单次请求的日志量放大一个数量级，高吞吐场景下会带来磁盘 I/O 争用和 CPU 开销——开启 debug 期间建议关注 `/data/log/` 磁盘占用。排障结束后记得把这一行删掉或改回 `info`/`warn`，避免长期产生大量日志。

#### 方式二：调试端口临时切换（不改文件，进程重启后失效）

Cubelet 内置了一个 debug HTTP 端口（默认监听 `:9966`，对应 `config.toml` 里的 `[debug] address`），暴露了 `/debug/loglevel` 接口，可以直接调用来临时切换级别：

```bash
# 切到 debug
curl -X POST 'http://127.0.0.1:9966/debug/loglevel?level=debug'

# 排障结束后切回 info
curl -X POST 'http://127.0.0.1:9966/debug/loglevel?level=info'
```

这种方式立即生效、无需等待轮询周期，但**只在当前进程生命周期内有效**——Cubelet 重启后会恢复到配置文件里写的级别（默认 `warn`）。适合临时抓一段现场，而不想改配置文件的场景。

同一个 debug 端口下还挂载了 `pprof` 相关的 profiling 接口（`/debug/pprof/*`），性能问题排查时也可以使用。**安全提示：** pprof 会暴露 goroutine 栈、堆内存 profile 和源码片段等信息——请确保 `:9966` 仅监听 localhost，不要通过反向代理暴露，也不要绑定到 `0.0.0.0`。

::: warning 只对新启动的沙箱生效
无论用哪种方式切到 debug，**已经处于运行中的沙箱不会补出更详细的日志**——它们创建/启动阶段的日志早已按之前的级别打完，不会因为后来调高了级别而重新打印。要看到完整的 debug 级别日志，需要在切换级别**之后**重新创建/启动沙箱。因此排障时建议的顺序是：先切到 debug，再复现问题（新建沙箱触发问题场景），而不是对着一个已经在跑的沙箱切日志级别等日志变详细。
:::

## 参考资料

- 相关文档：
  - [沙箱日志](../sandbox-logs.md)
  - [服务管理与日志](../service-management.md)
- 相关代码：
  - `Cubelet/cmd/cubecli/commands/cubebox/logs.go`（`cubecli logs` 实现）
  - `Cubelet/services/server/server.go`（`/debug/loglevel` HTTP 接口）
  - `Cubelet/pkg/config/config.go`、`Cubelet/pkg/hotswap/file.go`（动态配置热加载）
  - `shim/src/log/mod.rs`（CubeShim 日志与 console 转发实现）
  - `shim/src/hypervisor/config.rs`（`console=hvc0` 内核参数配置）
