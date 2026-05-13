# 快速开始

四步完成 Cube Sandbox 的完整部署，无需本地构建。

下面的流程会引导你在腾讯云购买一台普通云服务器，通过 PVM 启用 KVM，然后在这台服务器上安装并体验 Cube Sandbox。

⚠️请严格按照文档操作，这样能让你在几分钟内快速体验到CubeSandbox！

::: tip 已经有支持 KVM 的服务器？
如果你已经有一台开启了 KVM 的 x86_64 Linux 服务器（物理机或裸金属服务器），可以直接参阅[裸金属 / 物理机部署](./bare-metal-deploy.md)，跳过 PVM 安装步骤。
:::

## 前置条件

- **x86_64** 架构的云服务器（普通云服务器即可，无需 `/dev/kvm`）
- 有 **root 权限**
- 可访问互联网（用于下载发布包、拉取 Docker 镜像）

## 第一步：购买云服务器并安装 PVM 内核

### 购买云服务器

在腾讯云购买一台 **x86_64** 架构的云服务器，无特殊要求。

**操作系统推荐选择 OpenCloudOS 9**（RPM 系）。Cube Sandbox 的 PVM 宿主机内核基于 OpenCloudOS 内核构建，选用 OpenCloudOS 9 可获得最佳兼容性，且无需处理发行版差异。Ubuntu / Debian / CentOS 等其他主流发行版同样支持。

| 配置 | CPU | 内存 | 磁盘 |
| --- | --- | --- | --- |
| 功能体验 | ≥ 4 核 | ≥ 8 GB | ≥ 300 GB |
| 推荐 | 32 核 | 64 GB | ≥ 300 GB |

::: warning 以 root 身份执行所有操作
本文档中的所有命令均需在 **root** 用户下执行。请先切换到 root：

```bash
sudo su root
```

:::

### 安装 PVM 宿主机内核

前往 [CubeSandbox GitHub Releases](https://github.com/TencentCloud/CubeSandbox/releases) 页面，打开最新包含 PVM 内核附件的 Release，**在对应附件上右键 → 复制链接地址**，然后用 `wget` 下载。

根据你的 Linux 发行版选择对应格式：

#### RPM 系（OpenCloudOS、RHEL、CentOS、TencentOS、Fedora）

在 Release 附件列表中找到 `kernel-*cube.pvm.host*.x86_64.rpm`，右键复制下载链接：

```bash
# 将下面的 URL 替换为你从 Releases 页面右键复制的实际下载链接
wget "<kernel rpm 下载链接>"

# 若宿主机已有更高版本内核，--oldpackage 跳过版本号比较
rpm -ivh --oldpackage kernel-*.rpm
```

设置 PVM 内核为默认启动项：

```bash
# 查看已安装内核列表，找到 PVM 内核对应的序号
grubby --info=ALL | grep -E "^kernel|^index"

# 将 <index> 替换为上面输出中 PVM 内核对应的数字
grubby --set-default-index=<index>

# 确认设置生效
grubby --default-kernel
```

配置内核启动参数：

```bash
curl -sL https://cnb.cool/CubeSandbox/CubeSandbox/-/git/raw/master/deploy/pvm/grub/host_grub_config.sh | bash
```

#### DEB 系（Ubuntu、Debian）

在 Release 附件列表中找到 `linux-image-*cube.pvm.host*_amd64.deb`，右键复制下载链接：

```bash
# 将下面的 URL 替换为你从 Releases 页面右键复制的实际下载链接
wget "<linux-image deb 下载链接>"

dpkg -i linux-image-*cube.pvm.host*.deb
```

设置 PVM 内核为默认启动项：

```bash
# 查看已安装的内核列表，确认 PVM 内核版本字符串
ls /boot/vmlinuz-*

# 将 GRUB 默认启动项指向 PVM 内核（将下面的内核版本替换为上一步看到的实际版本字符串）
KVER="$(ls /boot/vmlinuz-*cube.pvm.host* | sed 's|/boot/vmlinuz-||' | tail -1)"
sed -i "s|^GRUB_DEFAULT=.*|GRUB_DEFAULT=\"Advanced options for Ubuntu>Ubuntu, with Linux ${KVER}\"|" \
  /etc/default/grub
```

配置内核启动参数（脚本内部会调用 `update-grub` 使上述设置生效）：

```bash
curl -sL https://cnb.cool/CubeSandbox/CubeSandbox/-/git/raw/master/deploy/pvm/grub/host_grub_config.sh | bash
```

### 重启并验证

```bash
reboot
```

重启后，确认已进入 PVM 内核并加载 KVM 模块：

```bash
# 确认内核版本
uname -r
# 期望输出包含：cube.pvm.host

# 加载 PVM KVM 模块
modprobe kvm_pvm

# 确认模块已加载
lsmod | grep kvm
# 期望输出中包含 kvm_pvm
```

设置开机自动加载 `kvm_pvm` 模块：

```bash
echo 'kvm_pvm' > /etc/modules-load.d/kvm-pvm.conf
```

::: details 什么是 PVM？（技术原理）
PVM（Pagetable-based Virtual Machine）是一种**基于页表的嵌套虚拟化框架**，构建于 KVM 之上。与传统嵌套虚拟化不同，PVM 不依赖宿主 hypervisor 向 guest 暴露 Intel VT-x / AMD-V 等硬件虚拟化扩展，而是在 guest 内核层通过共享内存区域和影子页表（shadow page table）来完成特权级切换与内存虚拟化，对宿主 hypervisor 完全透明。

腾讯云已在生产环境大规模部署 PVM 实例，可靠性经过充分验证，并将改进成果开源至 [OpenCloudOS 内核](https://gitee.com/OpenCloudOS/OpenCloudOS-Kernel.git)。

完整的 PVM 部署说明请参阅 [PVM 部署](./pvm-deploy.md)。
:::

## 第二步：安装

以 root 身份执行：

```bash
curl -sL https://cnb.cool/CubeSandbox/CubeSandbox/-/git/raw/master/deploy/one-click/online-install.sh | CUBE_PVM_ENABLE=1 MIRROR=cn bash
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
  --image cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  --writable-layer-size 1G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe 49999
```

> **镜像仓库说明：** 国内优先使用 `cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest`；境外访问推荐使用 `cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest`。

然后，执行下面的这行命令，监控构建进度：

```bash
cubemastercli tpl watch --job-id <job_id>
```

⚠️ 注意：由于镜像比较大，下载、解压、模板制作过程可能比较久，请耐心等待。


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
export SSL_CERT_FILE="/root/.local/share/mkcert/rootCA.pem"
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
- [裸金属 / 物理机部署](./bare-metal-deploy.md) — 已有支持 KVM 的机器直接部署
- [多机集群部署](./multi-node-deploy.md) — 扩展到多台机器
- [HTTPS 证书与域名解析](./https-and-domain.md) — TLS 配置选项
- [鉴权](./authentication.md) — 启用 API 鉴权

## 附录：从源码构建

以上步骤使用的是预构建发布包。如果需要自定义组件、使用特定 commit 或参与开发贡献，可以自行构建发布包。完整说明请参阅[本地构建部署](./self-build-deploy.md)。
