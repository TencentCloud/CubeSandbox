# 快速开始

四步完成 Cube Sandbox 的完整部署，无需本地构建。

下面的流程会在你的开发机（ WSL / Linux 机器）上起一台**一次性的 Linux 虚机**，然后在这台虚机里安装并体验 Cube Sandbox。
请严格按照文档操作，这样能让你在几分钟内快速体验到CubeSandbox！

::: tip 已经有裸金属服务器？
如果你已经有一台开启了 KVM 的 x86_64 Linux 物理服务器，可以**跳过第一步**，直接在那台机器上执行第二步的安装命令。
:::

## 前置条件

宿主机满足下列任一条件即可：

- **Windows 上的 WSL 2**（Windows 11 22H2+，并在 WSL 里启用嵌套虚拟化）
- **已开启嵌套虚拟化的 Linux 虚拟机**（VMWare启动Ubuntu 22.04，并且在虚拟机CPU设置那里，启用 “Virtualize Intel VT-x/EPT or AMD-V/RVI”）
- **x86_64 Linux 物理机**
- **x86_64 裸金属 Linux 服务器** 

通用要求：

1. Linux环境能正常使用 KVM（`/dev/kvm` 存在且可读写）
2. Linux环境中，**Docker、QEMU** 已安装并正常运行
3. 可访问互联网（用于克隆仓库、下载发布包、拉取 Docker 镜像）

## 第一步：启动开发虚机

克隆仓库并进入 `dev-env/`：

```bash
git clone https://github.com/tencentcloud/CubeSandbox.git
# 如果您的环境无法访问github，请执行：
# git clone https://cnb.cool/CubeSandbox/CubeSandbox.git


cd CubeSandbox/dev-env
```

一共三条命令。前两条在同一个终端执行，第三条在**新终端**里执行。

> 在执行命令之前，请确保您的Linux机器上已经安装qemu、qemu-img、ripgrep

```bash
./prepare_image.sh   # 仅首次：下载并初始化 OpenCloudOS 9 镜像
./run_vm.sh          # 启动虚机；保持此终端不关（Ctrl+a x 关机）
```

在新终端里：

```bash
cd CubeSandbox/dev-env
./login.sh           # 以 root 登录虚机
```

接下来所有步骤都在**虚机内**执行 —— `login.sh` 会直接把你送到虚机的
root shell，Cube Sandbox 就装在这里。

关于宿主机自检（嵌套 KVM、依赖软件）、端口映射、环境变量覆盖和常见
问题，请参阅[开发环境（QEMU 虚机）](./dev-environment.md)。

## 第二步：安装

在**开发虚机内**以 root 身份执行：

```bash
curl -sL https://cnb.cool/CubeSandbox/CubeSandbox/-/git/raw/master/deploy/one-click/online-install.sh | MIRROR=cn bash
```

::: details 安装了哪些组件
- E2B 兼容 REST API 监听在 `3000` 端口
- CubeMaster、Cubelet、network-agent、CubeShim 作为宿主机进程运行
- MySQL 和 Redis 通过 Docker Compose 管理
- CubeProxy 提供 TLS（mkcert）和 CoreDNS 域名路由（`cube.app`）
:::


## 第三步：制作模板

安装完成后，使用预构建镜像创建代码解释器模板：

```bash
cubemastercli tpl create-from-image \
  --image ccr.ccs.tencentyun.com/ags-image/sandbox-code:latest \
  --writable-layer-size 1G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe 49999
```

然后，执行下面的这行命令，监控构建进度：

```bash
cubemastercli tpl watch --job-id <job_id>
```

⚠ 注意：由于镜像比较大，下载、解压、模板制作过程可能比较久，请耐心等待。


等待上述命令结束，模板状态变为 `READY`。

记录输出中的**模板 ID** (`template_id`)，下一步会用到。

完整的模板创建流程和更多参数说明，请参阅[从 OCI 镜像制作模板](./tutorials/template-from-image.md)。

## 第四步：运行第一段 Agent 代码

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
export SSL_CERT_FILE="$(mkcert -CAROOT)/rootCA.pem"
```

| 变量 | 说明 |
|------|------|
| `E2B_API_URL` | 将 E2B SDK 请求指向本地 Cube Sandbox，而非 E2B 官方云服务 |
| `E2B_API_KEY` | SDK 强制非空校验，本地部署填任意字符串即可 |
| `CUBE_TEMPLATE_ID` | 第三步获取的模板 ID |
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
- [CubeProxy TLS 配置](./cubeproxy-tls.md) — TLS 配置选项
- [鉴权](./authentication.md) — 启用 API 鉴权

## 附录：从源码构建

以上步骤使用的是预构建发布包。如果需要自定义组件、使用特定 commit 或参与开发贡献，可以自行构建发布包。完整说明请参阅[本地构建部署](./self-build-deploy.md)。
