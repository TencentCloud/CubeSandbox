# 计算面镜像升级 Runbook

本文说明如何通过 **升级 `cube-node` 镜像 + Helm / DaemonSet 滚动重建 Pod**，在不销毁存量沙箱的前提下升级计算节点上的 `cubelet` / `network-agent` 等控制组件。

## 原理（对齐一键包）

一键包升级是：停 systemd 服务（`KillMode=process` 不杀 shim）→ `rm -rf` 旧安装目录 → 拷新二进制 → 重启。存量 `containerd-shim-cube-rs` / microVM 靠「进程持有已 unlink 的 inode + 宿主机文件系统从不卸载」继续跑。

Chart 侧等价条件：

1. **toolbox 落在 hostPath** `/usr/local/services/cubetoolbox`（与一键包 `INSTALL_PREFIX` 相同；由 `stage-toolbox` initContainer 每次启动把镜像内**软件包目录**同步到该 hostPath）。与 one-click `install.sh` 一样做**选择性覆盖**，保留运行期目录：`cube-snapshot`、`cubebox_os_image`、`cubeletmnt`、`cube-vs`（不会整棵 `rm -rf` toolbox）。
2. 主容器把该 hostPath 挂回容器内 `/usr/local/services/cubetoolbox`，cubelet / shim 路径与一键包一致。
3. **shim / VMM socket 目录落在 hostPath**（不能用容器 ephemeral `/run`）：
   - 容器内 `/run/containerd` ← 宿主机 `hostPaths.runContainerd`（默认 `/data/cubelet/run/containerd`）。`containerd-shim` 的 `SOCKET_ROOT` 硬编码为 `/run/containerd`，`LoadExistingShims` 通过 `bootstrap.json`/`address` 里的 `unix:///run/containerd/s/{hash}` dial 存量 shim。**禁止**把宿主机真实的 `/run/containerd`（kubelet/containerd 占用）挂进来。
   - 容器内 `/run/vc` ← 宿主机 `hostPaths.runVc`（默认 `/data/cubelet/run/vc`）。CubeShim `VM_PATH=/run/vc/vm/`，含 `chapi` / `cube.sock` 等。
4. **`/data/cubelet/state` 必须落在 hostPath 磁盘上，不能被 cubelet 默认的 state tmpfs 盖住**。cubelet `mountTmpfsDir` 会在私有 mount NS 里给 state 挂 500Mi tmpfs；Pod 删除后该 NS/tmpfs 消失，`bootstrap.json`/`address` 随之丢失，即使 shim 进程与 socket 仍在也无法 `LoadExistingShims`。Chart 在启动 entrypoint 前对 `/data/cubelet/state` 做 `mount --bind`，使 `mountTmpfsDir` 直接跳过。
5. Pod 删除时 `preStop` / entrypoint `cleanup` **只停 cubelet 与 network-agent**，不碰 shim。
6. 新 Pod 启动后 cubelet `LoadExistingShims` + `RecoverAllCubebox`、network-agent `recover()` 重连存量沙箱。
7. **绝不**在升级路径调用 `InitHost` / `cubecli unsafe init`（会 Destroy 全部沙箱）。

```mermaid
sequenceDiagram
  participant Helm as helm_upgrade
  participant DS as cube_node_DaemonSet
  participant Old as old_cube_node_Pod
  participant Shim as shim_microVM
  participant New as new_cube_node_Pod
  participant Host as hostPath_toolbox

  Helm->>DS: bump images.node tag
  DS->>Old: RollingUpdate maxUnavailable=1
  Old->>Old: preStop TERM cubelet/network-agent only
  Note over Shim: stays alive on host cgroup
  Old-->>DS: Pod deleted (overlay unmounted)
  Note over Host: host /usr/local/services/cubetoolbox unlinked; shim keeps old inodes
  DS->>New: create Pod
  New->>Host: stage-toolbox rm+cp new binaries onto hostPath
  New->>New: start network-agent recover + cubelet LoadExistingShims
  New->>Shim: reconnect ttrpc
```

## 升级步骤

### 1. 准备新镜像

构建并推送新的 `cube-node`（以及如需一并升级的 `cube-egress` 等）镜像，更新 `runtime-values.yaml` 中的 tag：

```yaml
images:
  node:
    repository: ccr.ccs.tencentyun.com/cubesandbox-chart/cube-node
    tag: v0.5.1   # 示例
```

### 2. Helm 升级

```bash
helm upgrade cube ./deploy/kubernetes/chart \
  -n cube-system \
  -f ./deploy/kubernetes/chart/runtime-values.yaml \
  --wait \
  --timeout 90m
```

默认 `cubeNode.updateStrategy` 为：

```yaml
updateStrategy:
  type: RollingUpdate
  rollingUpdate:
    maxUnavailable: 1
```

一次只重建一台计算节点上的 `cube-node` Pod。

### 3. 观察滚动

```bash
kubectl get ds -n cube-system cube-node -w
kubectl get pods -n cube-system -l app.kubernetes.io/component=cube-node -o wide -w
```

单节点 Ready 条件（readiness exec）：

- cubelet `:9999` 监听
- network-agent `/readyz` 通过
- `/data/cubelet/cubelet.sock` 与 `/tmp/cube/network-agent-grpc.sock` 存在

### 4. 验证沙箱仍在

```bash
kubectl exec -n cube-system deploy/cube-cubemastercli -- \
  sh -lc 'cubemastercli --address "$CUBEMASTERCLI_ADDRESS" --port "$CUBEMASTERCLI_PORT" list --all -w'

# 在计算节点上确认 shim pid 在 Pod 重建前后不变（升级前先记下）
kubectl exec -n cube-system <cube-node-pod> -c cube-node -- \
  sh -lc 'pgrep -af containerd-shim-cube-rs; ls -l /proc/$(pgrep -n containerd-shim-cube-rs)/exe'
```

## 手动逐节点（OnDelete）

若希望完全人工控制节奏：

```yaml
cubeNode:
  updateStrategy:
    type: OnDelete
```

然后：

```bash
helm upgrade ...   # 只更新 DaemonSet 模板，不自动删 Pod
kubectl -n cube-system delete pod -l app.kubernetes.io/component=cube-node \
  --field-selector spec.nodeName=<node>
# 等 Ready 后再删下一台
```

## 升级窗口内新建沙箱

旧 Pod 已停、新 Pod 尚未 Ready 的短暂窗口内，CubeMaster 心跳可能尚未过期（默认约 40s），仍可能把**新**沙箱调度到该节点；创建会失败并由 CubeMaster **自动 reschedule** 到其他节点。存量沙箱不受影响。当前不提供控制面 cordon；若新建延迟敏感，可后续加节点摘流。

## 卸载时额外清理

`helm uninstall` 不会删除 hostPath。除 `/data/cubelet` 等外，还需按需清理：

```bash
sudo rm -rf /usr/local/services/cubetoolbox
```

## 相关文档

- [ARCHITECTURE.md](ARCHITECTURE.md) — Big Pod 与启动流程
- [FAQ.md](FAQ.md) G1 — 升级是否中断沙箱
- 一键包对照：`deploy/one-click/install.sh` upgrade 路径、`cube-sandbox-cubelet.service` 的 `KillMode=process`
