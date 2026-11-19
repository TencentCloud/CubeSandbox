# macOS（Apple Silicon）+ Lima 部署

> **适用场景：** 在 Apple Silicon Mac 上运行单节点 Cube Sandbox，用于本地开发或测试。服务器部署请参考[快速开始](./quickstart.md)。

Cube Sandbox 需要带 KVM 的 Linux。M3 及更新的 Mac 上，Apple 的 Virtualization.framework 可以为 Linux 虚拟机开启嵌套虚拟化，虚拟机里就有 `/dev/kvm`，可以原样运行 arm64 一键安装包。沙箱和服务器上一样，是从快照恢复的 KVM 微虚拟机，唯一的区别是多了一层虚拟化。

```
macOS ──vz（嵌套虚拟化）──▶ Ubuntu 24.04 arm64 虚拟机 ──KVM──▶ Cube 微虚拟机
  :13000 ─────────────────▶ CubeAPI :3000
  :18080 ─────────────────▶ CubeProxy :80   (Host: <port>-<sandboxID>.cube.app)
```

脚本位于 [`deploy/macos-lima/`](https://github.com/TencentCloud/CubeSandbox/tree/master/deploy/macos-lima)。

## 前置条件

- **Apple Silicon M3 或更新的芯片，macOS 15 或更新。** M1、M2 不支持嵌套虚拟化，虚拟机里不会有 `/dev/kvm`。
- **Lima 2.x**（`brew install lima`）。用低于 macOS 15 SDK 构建的 Lima 1.0.x 即使在支持的硬件上也会拒绝 `nestedVirtualization`。
- 虚拟机约需 16 GiB 内存和 120 GiB 磁盘（稀疏文件）。安装程序要求虚拟机内存不少于 7.5 GB。

## 启动

在 Mac 上，从本仓库的检出目录运行：

```bash
deploy/macos-lima/up.sh
```

脚本会依次执行以下步骤：

1. 按 `lima.yaml` 创建虚拟机：vz 加 `nestedVirtualization`，8 vCPU、16 GiB，CubeAPI 和 CubeProxy 分别转发到宿主机的 13000 和 18080 端口。
2. 用 `vm-setup.sh` 准备虚拟机：Docker、挂在 `/data/cubelet` 的 XFS（reflink）loop 文件，以及构建环境的配置。
3. 用 `build-bundle.sh` 从已提交的 `HEAD` 构建 arm64 一键安装包。它使用与 arm64 发布任务相同的内核、guest 镜像和 builder 镜像版本。首次构建约 30 分钟，之后约 5 分钟。
4. 用 `install.sh` 安装，所用设置见下文 [Lima 相关设置](#lima-相关设置)。

如果想安装已发布的 arm64 安装包、跳过构建：

```bash
deploy/macos-lima/up.sh --bundle https://github.com/TencentCloud/CubeSandbox/releases/download/<tag>/cube-sandbox-one-click-<version>-arm64.tar.gz
```

再次运行 `up.sh` 会原地升级；加 `--reinstall` 会先清空 Cube 的数据。在中国大陆网络下，设置 `CUBE_LIMA_CN_MIRRORS=1`，虚拟机内的构建会从镜像源获取 Go 模块和 crates。

## 创建模板和沙箱

进入虚拟机（`limactl shell cube`），按[快速开始](./quickstart.md)创建模板。镜像需要支持多架构（arm64），就绪探测选 envd（49983 端口）：

```bash
cubemastercli tpl create-from-image \
  --image cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  --writable-layer-size 1G --expose-port 49999 --expose-port 49983 --probe 49983
cubemastercli tpl watch --job-id <job_id>
```

为什么不探测 49999：构建模板时虚拟机是冷启动，就绪探测大约只有 30 秒。嵌套虚拟化下，49999 端口上的代码解释器可能来不及在这段时间内启动，模板构建就会因 `:49999/health` 连接被拒绝而失败。envd 则在虚拟机启动后立刻可用。

在 Mac 上，任何兼容 E2B 的 SDK 都可以连接 `E2B_API_URL=http://127.0.0.1:13000`。沙箱端口通过 CubeProxy 访问：`127.0.0.1:18080`，请求头为 `Host: <port>-<sandboxID>.cube.app`。

`bench.sh` 依次测量沙箱创建、经 CubeProxy 就绪和删除的耗时：

```bash
limactl shell cube -- bash ~/cube/src/deploy/macos-lima/bench.sh <template_id> 10
```

## Lima 相关设置

`install.sh` 会自动应用以下设置：

- `CUBE_SANDBOX_NETWORK_CIDR=10.88.0.0/20`：Lima 的用户网络（`192.168.5.0/24`）落在默认的 `192.168.0.0/18` 之内，安装程序会拒绝重叠的网段。
- `no_proxy` 包含节点 IP：Lima 会把宿主机的 `http_proxy` 传进虚拟机，而安装程序的就绪检查必须直连本机端点。

## M5 Pro 实测

宿主机为 M5 Pro，macOS 26.6.2，Lima 2.2.0；虚拟机为 8 vCPU、16 GiB。

| 步骤 | 耗时 |
| --- | --- |
| 冷启动到 agent 就绪（仅构建模板时发生） | 通常 9–12 秒，最长观测到 38 秒 |
| 从 `sandbox-code:latest` 构建模板（镜像已缓存） | 约 55 秒 |
| 创建沙箱（快照恢复），探测 envd | 1.0–1.6 秒 |
| 删除 | 约 0.7 秒 |

## 限制

- 虚拟机是 arm64 架构，模板镜像必须包含 `linux/arm64`。
- 嵌套虚拟化下冷启动约 10 秒或更久。快照恢复很快，所以这只影响模板构建。
- 仅适用于开发和测试：单节点，位于 Lima 的 NAT 之后。
