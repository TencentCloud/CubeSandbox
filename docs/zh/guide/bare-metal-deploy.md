# 裸金属 / 物理机部署

> **适用场景：** 已有支持 KVM 的 x86_64 或 aarch64 Linux 机器（`/dev/kvm` 可用），例如物理机、裸金属服务器、或已开启嵌套虚拟化的云服务器。
>
> 如果你用的是**普通云服务器**（`/dev/kvm` 不可用），无需裸金属 —— 通过 PVM 即可在普通云服务器上启用 KVM，请参阅[快速开始](./quickstart.md)。注意：PVM 仅在 x86_64 上可用；aarch64 必须使用原生暴露 `/dev/kvm` 的宿主机。

## 前置条件

- **x86_64** 或 **aarch64** 架构的 Linux 机器（参见下方 [aarch64 注意事项](#aarch64-注意事项)）
- `/dev/kvm` 存在且可读写（`ls -la /dev/kvm`）
- 有 **root 权限**
- **Docker** 已安装并正常运行
- 可访问互联网（用于下载发布包、拉取 Docker 镜像）
- 内存 ≥ 8 GB，磁盘空余 ≥ 50 GB

### aarch64 注意事项

Cube Sandbox 已提供 **aarch64 初步支持**。运行时仍然要求宿主机上有 `/dev/kvm`（不能通过 PVM 替代），并且需要将支撑组件的容器镜像替换为多架构标签，以便自动拉取 arm64 镜像。

在使用 cube-sandbox-one-click 的 `install.sh` 之前，先把下述变量追加到 `.env`：

```bash
cat <<EOF >> .env
CUBE_SANDBOX_MYSQL_IMAGE="mysql:8.0"
CUBE_SANDBOX_REDIS_IMAGE="redis:7-alpine"
CUBE_PROXY_COREDNS_IMAGE="coredns/coredns:1.14.2"
WEB_UI_IMAGE="openresty/openresty:1.21.4.1-6-alpine-fat"
EOF
```

`env.example` 中的默认标签指向 `cube-sandbox-image.tencentcloudcr.com/opensource/...` 镜像源，目前仅提供 x86_64 manifest。上面这些 Docker Hub 上游标签是多架构 manifest，会自动解析为正确的 arm64 镜像。

**已覆盖的 aarch64 测试机器：**

- HUAWEI 鲲鹏 920（裸金属 Linux）

> 目前 aarch64 测试只覆盖 HUAWEI 鲲鹏 920。其他 arm64 平台（包括其他云厂商的 Arm 裸金属、Apple Silicon 通过 [lima-vm](https://lima-vm.io/) 等嵌套虚拟化方案）原则上同样可用，但尚未经过官方测试，欢迎反馈。

**aarch64 上已知的能力差异：**

- **不支持 PVM。** PVM（基于页表的虚拟机）是 x86_64 专属能力，目前**没有移植到 aarch64 的计划**。aarch64 必须运行在原生暴露 `/dev/kvm` 的宿主机上（裸金属 Arm 服务器、Arm 云裸金属，或任何已将 ARM 虚拟化扩展透出到内核的嵌套虚拟化环境）。

::: warning 以 root 身份执行所有操作
本文档中的所有命令均需在 **root** 用户下执行。请先切换到 root：

```bash
sudo su root
```

:::

## 第一步：安装

以 root 身份执行：

```bash
curl -sL https://cnb.cool/CubeSandbox/CubeSandbox/-/git/raw/master/deploy/one-click/online-install.sh | MIRROR=cn bash
```

::: details 安装了哪些组件
- E2B 兼容 REST API 监听在 `3000` 端口
- CubeMaster、Cubelet、network-agent、CubeShim 作为宿主机进程运行
- MySQL 和 Redis 通过 Docker Compose 管理
- CubeProxy 提供 TLS（mkcert）和 CoreDNS 域名路由（`cube.app`）
:::

## 第二步：制作模板

安装完成后，使用预构建镜像创建代码解释器模板：

```bash
cubemastercli tpl create-from-image \
  --image cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  --writable-layer-size 1G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe 49999
```

> **镜像仓库说明：** 国内优先使用 `cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest`；境外访问推荐使用 `cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest`。

然后监控构建进度：

```bash
cubemastercli tpl watch --job-id <job_id>
```

⚠️ 注意：由于镜像比较大，下载、解压、模板制作过程可能比较久，请耐心等待。

等待上述命令结束，模板状态变为 `READY`。

记录输出中的**模板 ID** (`template_id`)，下一步会用到。

完整的模板创建流程和更多参数说明，请参阅[从 OCI 镜像制作模板](./tutorials/template-from-image.md)。

## 第三步：运行第一段 Agent 代码

安装 Python SDK：

```bash
yum install -y python3 python3-pip
pip config set global.index-url https://mirrors.ustc.edu.cn/pypi/simple

pip install e2b-code-interpreter
```

设置环境变量：

```bash
export E2B_API_URL="http://127.0.0.1:3000"
export E2B_API_KEY="dummy"
export CUBE_TEMPLATE_ID="<你的模板ID>"
export SSL_CERT_FILE="/root/.local/share/mkcert/rootCA.pem"
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

更多端到端示例，请参阅[示例项目](./tutorials/examples.md)。

## 下一步

- [从 OCI 镜像制作模板](./tutorials/template-from-image.md) — 自定义沙箱运行环境
- [多机集群部署](./multi-node-deploy.md) — 扩展到多台机器
- [HTTPS 证书与域名解析](./https-and-domain.md) — TLS 配置选项
- [鉴权](./authentication.md) — 启用 API 鉴权
