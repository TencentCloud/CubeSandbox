# 快速开始

三步完成 Cube Sandbox 的完整部署，无需本地构建。

::: tip 没有 bare-metal 机器？
如果只有笔记本或云主机（已开启 KVM + nested virtualization），先按
[开发环境（QEMU 虚机）](./dev-environment) 起一台一次性的 OpenCloudOS 9
虚机，剩下的快速开始步骤都在虚机里执行即可。
:::

## 前置条件

- **裸金属 Linux 服务器**（x86_64），已启用 KVM（`/dev/kvm` 存在）
- **Docker** 已安装并正常运行
- 可访问互联网（用于下载发布包和拉取 Docker 镜像）

## 第一步：安装

以 root 身份（或使用 `sudo`）在目标机上执行：

```bash
curl -sL https://github.com/tencentcloud/CubeSandbox/raw/master/deploy/one-click/online-install.sh | bash
```

::: tip 节点 IP 自动探测
安装脚本会自动从 `eth0` 网卡探测节点 IP。如果你的主网卡名称不同，或者需要指定特定 IP，可通过环境变量显式传入：

```bash
CUBE_SANDBOX_NODE_IP=<你的节点IP> bash <(curl -sL https://github.com/tencentcloud/CubeSandbox/raw/master/deploy/one-click/online-install.sh)
```
:::

::: tip 国内加速镜像
国内网络下载慢，可加上 `MIRROR=cn` 从 CDN 拉取发布包：

```bash
curl -sL https://github.com/tencentcloud/CubeSandbox/raw/master/deploy/one-click/online-install.sh | MIRROR=cn bash
```
:::

::: details 安装了哪些组件
- E2B 兼容 REST API 监听在 `3000` 端口
- CubeMaster、Cubelet、network-agent、CubeShim 作为宿主机进程运行
- MySQL 和 Redis 通过 Docker Compose 管理
- CubeProxy 提供 TLS（mkcert）和 CoreDNS 域名路由（`cube.app`）
:::

安装完成后，安装器会把 `cubemastercli` 和 `cubecli` 软链接到 `/usr/local/bin`。

## 第二步：制作模板

安装完成后，`cubemastercli` 已加入系统 PATH。使用预构建镜像创建代码解释器模板：

```bash
cubemastercli tpl create-from-image \
  --image ccr.ccs.tencentyun.com/ags-image/sandbox-code:latest \
  --writable-layer-size 1G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe 49983
```

监控构建进度，等待状态变为 `READY`：

```bash
cubemastercli tpl watch --job-id <job_id>
```

记录输出中的**模板 ID**，下一步会用到。

完整的模板创建流程和更多参数说明，请参阅[从 OCI 镜像制作模板](./tutorials/template-from-image)。

## 第三步：运行第一段 Agent 代码

安装 Python SDK：

```bash
pip install e2b-code-interpreter
```

设置环境变量：

```bash
export E2B_API_URL="http://127.0.0.1:3000"
export E2B_API_KEY="dummy"
export CUBE_TEMPLATE_ID="<你的模板ID>"
export SSL_CERT_FILE="$(mkcert -CAROOT)/rootCA.pem"
```

| 变量 | 说明 |
|------|------|
| `E2B_API_URL` | 将 E2B SDK 请求指向本地 Cube Sandbox，而非 E2B 官方云服务 |
| `E2B_API_KEY` | SDK 强制非空校验，本地部署填任意字符串即可 |
| `CUBE_TEMPLATE_ID` | 第二步获取的模板 ID |
| `SSL_CERT_FILE` | mkcert 签发的 CA 根证书路径，沙箱 HTTPS 连接需要 |

在隔离沙箱中运行代码：

```python
import os
from e2b_code_interpreter import Sandbox  # 直接使用 E2B SDK！

# CubeSandbox 在底层无缝接管了所有的请求
with Sandbox.create(template=os.environ["CUBE_TEMPLATE_ID"]) as sandbox:
    result = sandbox.run_code("print('Hello from Cube Sandbox, safely isolated!')")
    print(result)
```

也可以执行 Shell 命令和操作文件：

```python
import os
from e2b_code_interpreter import Sandbox

with Sandbox.create(template=os.environ["CUBE_TEMPLATE_ID"]) as sandbox:
    # 执行 Shell 命令
    result = sandbox.commands.run("echo hello cube")
    print(result.stdout)

    # 读取沙箱内文件
    content = sandbox.files.read("/etc/hosts")
    print(content)
```

更多端到端示例，请参阅[示例项目](./tutorials/examples)。

## 下一步

- [从 OCI 镜像制作模板](./tutorials/template-from-image) — 自定义沙箱运行环境
- [多机集群部署](./multi-node-deploy) — 扩展到多台机器
- [CubeProxy TLS 配置](./cubeproxy-tls) — TLS 配置选项
- [鉴权](./authentication) — 启用 API 鉴权

## 附录：从源码构建

以上步骤使用的是预构建发布包。如果需要自定义组件、使用特定 commit 或参与开发贡献，可以自行构建发布包。完整说明请参阅[本地构建部署](./self-build-deploy)。
