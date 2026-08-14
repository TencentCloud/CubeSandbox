# 命令行工具

CubeSandbox 随宿主机安装包提供多种运维和排障命令行工具。一键安装会把它们安装到宿主机；Kubernetes 部署会把 `cubecli`、`cube-runtime` 等节点本地工具放进组件镜像或节点 toolbox 中。

这些工具应只在可信运维机器上使用。它们绕过面向用户的 CubeAPI 体验，能够直接查看或修改集群、节点和运行时状态。

## 工具总览

| 工具 | 运行位置 | 访问对象 | 主要用途 |
|------|----------|----------|----------|
| `cubemastercli` | 控制节点、跳板机，或任何能访问 CubeMaster 的机器 | CubeMaster HTTP API，默认端口 `8089` | 集群级 sandbox、模板、快照、volume 运维 |
| `cubeopscli` | 控制节点、跳板机，或任何能访问 CubeOps 的机器 | CubeOps HTTP API，默认端口 `3010` | 节点列表、隔离/解除隔离、删除节点 |
| `cubecli` | 运行 Cubelet 和 containerd 的计算节点 | 本地 Cubelet/containerd 状态 | 单节点 sandbox/container 查看、容器 shell、日志、存储清理、本地运行时排障 |
| `cube-runtime` | 承载目标 sandbox MVM 的计算节点 | 本地 CubeShim hybrid-vsock/debug console | 登录 guest MVM 或执行底层 VM snapshot 辅助操作 |

一键安装会为 `cube-runtime`、`containerd-shim-cube-rs`、`cubecli`、`cubemastercli` 和 `cubeopscli` 创建 `/usr/local/bin` 软链接。`cubemastercli` 和 `cubeopscli` 包含在发布包中，Terraform 跳板机也会安装它们。

## `cubemastercli`

`cubemastercli` 是集群级管理 CLI。它访问 CubeMaster，因此除非运行位置的默认地址正好可用，通常都需要指定 `--address` 和 `--port`。

```bash
cubemastercli --address <cubemaster-host> --port 8089 --help
cubemastercli --address <cubemaster-host> --port 8089 version
```

常用集群检查：

```bash
# 查看 CubeMaster 已知的所有 sandbox。
cubemastercli --address <cubemaster-host> --port 8089 list --all

# 查看单个 sandbox。
cubemastercli --address <cubemaster-host> --port 8089 info --sandboxid <sandbox-id>
```

模板操作：

```bash
# 查看模板列表。
cubemastercli --address <cubemaster-host> --port 8089 tpl ls

# 查看模板元数据和各节点副本状态。
cubemastercli --address <cubemaster-host> --port 8089 tpl info <template-id>

# 新增计算节点后，在指定节点重新构建或分发模板。
cubemastercli --address <cubemaster-host> --port 8089 tpl redo \
  --template-id <template-id> \
  --node <node-id-or-host>

# 从 OCI 镜像创建模板。
cubemastercli --address <cubemaster-host> --port 8089 tpl create-from-image \
  --image <registry>/<repo>:<tag> \
  --writable-layer-size 1G \
  --expose-port 49983 \
  --probe 49983
```

破坏性操作需要谨慎执行：

```bash
# 通过 CubeMaster 删除一个 sandbox。
cubemastercli --address <cubemaster-host> --port 8089 cubebox destroy <sandbox-id>
```

多节点部署和模板分发背景见[多节点集群](./multi-node-deploy.md)。

## `cubeopscli`

`cubeopscli` 是节点管理 CLI。它访问 CubeOps，因此除非运行位置的默认地址正好可用，通常都需要指定 `--address` 和 `--port`。

```bash
cubeopscli --address <cubeops-host> --port 3010 node list
cubeopscli --address <cubeops-host> --port 3010 node isolate <node-id>
cubeopscli --address <cubeops-host> --port 3010 node unisolate <node-id>
cubeopscli --address <cubeops-host> --port 3010 node delete <node-id>
```

删除节点要求节点**已隔离且无沙箱**；批量删除时单个节点失败不会中断后续节点，命令最终返回非零退出码。`delete` 的别名是 `rm`；用 `--force` 可在无法校验沙箱清单时强制删除（仍要求先隔离）。

节点隔离与删除详细用法见[隔离节点](./node-isolation.md)。

## `cubecli`

`cubecli` 是计算节点本地工具。除非命令明确指定远端 Cubelet 地址，否则应在目标 sandbox 所在计算节点上运行。

```bash
cubecli --help
cubecli version
```

常用节点本地检查：

```bash
# 查看本机 Cubelet 上的 sandbox。
cubecli cubebox ls

# 按 sandbox ID 过滤本机 Cubelet 的 sandbox 列表。
cubecli cubebox ls --sandbox <sandbox-id>

# 查看 containerd container 元数据。
cubecli container info <container-id>

# 查看 sandbox 或模板 stdout/stderr 日志。
cubecli logs <sandbox-id>
cubecli logs --stderr <sandbox-id>

# 查看 Cubelet 记录的本地存储 volume。
cubecli storage ls

# 先 dry-run 检查本地孤儿存储，再决定是否清理。
cubecli storage cleanup --dry-run

# 查看 Cubelet network runtime 的 tap 状态。需要 Cubelet toolbox 配置，并会提示确认。
cubecli network ls
```

进入 sandbox 容器/rootfs 视角：

```bash
cubecli exec -it <sandbox-id> bash
```

这个命令通过本地容器运行时创建 exec 进程，适合检查用户进程、文件系统、环境变量、命令执行行为和容器级日志。它不是登录 guest MVM。

`unsafe` 命令只应在明确理解本地节点影响范围时使用：

```bash
# 示例：只删除当前节点上的所有本地 sandbox。
cubecli unsafe rm --all
```

在多节点集群中，`cubecli` 只覆盖命令所在节点。集群级操作优先使用 `cubemastercli`。

## `cube-runtime`

`cube-runtime` 是 CubeShim workspace 中的底层运行时辅助工具。运维上最常用的是通过 debug console 进入 sandbox MVM。

```bash
cube-runtime --help
cube-runtime login --help
```

登录 sandbox MVM：

```bash
cube-runtime login <sandbox-id>
```

`login` 会连接 sandbox 的本地 hybrid-vsock 路径，再进入 debug console 端口。默认 debug console 端口是 `1026`，默认连接超时是 `10` 秒。

```bash
cube-runtime login <sandbox-id> --port 1026 --timeout 10
```

当需要 guest VM 视角时使用 `cube-runtime login`，例如排查 guest 内核状态、guest 网络接口、agent 状态、挂载点或 MVM 级 pause/resume 行为。

`cube-runtime snapshot` 是底层 snapshot 工作流使用的命令，通常由 Cubelet/CubeMaster 的上层流程调用。除非正在排查运行时内部问题，否则应优先使用 `cubemastercli` 提供的模板和快照操作。

## 如何选择工具

如果问题是集群级的，使用 `cubemastercli`：

- 哪些节点健康？
- 哪些模板副本 ready？
- 某个 sandbox 在哪个节点上？
- 新增节点后如何 redo 模板？
- 如何通过控制面删除 sandbox？

如果问题是节点本地的，使用 `cubecli`：

- 这个 sandbox 是否在本机 Cubelet 上？
- 能否进入 sandbox 容器？
- 本地容器日志是什么？
- 是否有孤儿本地存储 volume？
- Cubelet 看到的本地 tap/network 状态是什么？

如果问题发生在 MVM 内部，使用 `cube-runtime`：

- 能否进入 guest debug console？
- guest 内核或 VM 级网络看到的状态是什么？
- 问题是否位于 container/rootfs 层之下？

## 安全注意事项

- 不要把密钥、API key、镜像仓库凭据或私有 endpoint 粘贴进 shell history。
- 收集 issue 证据时，优先使用支持的 `--json` 输出；对外分享前先脱敏。
- 多节点集群中，先用 `cubemastercli info --sandboxid <sandbox-id>` 确认 sandbox 所在节点，再到该计算节点执行 `cubecli` 或 `cube-runtime`。
- `cubecli unsafe ...` 和破坏性 `cubemastercli` 命令属于运维变更，不是只读诊断。
