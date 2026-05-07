# PVM 部署

> **适用场景：** 云服务器上 `/dev/kvm` 不可用（云服务商屏蔽了嵌套虚拟化）。如果你的机器已经支持 KVM，直接参阅[快速开始](./quickstart.md)或[本地构建部署](./self-build-deploy.md)即可。

得益于 PVM，您可以在**普通的云服务器**上部署 Cube Sandbox，所有沙箱实例均运行在 PVM 支持的 Micro-VM 中。

与标准部署相比，PVM 部署只额外增加两个步骤：

1. 安装 PVM 宿主机内核并重启
2. 安装 Cube Sandbox 时传入 `CUBE_PVM_ENABLE=1`

::: tip 生产就绪
腾讯云已在生产环境大规模部署 PVM 实例，可靠性经过充分验证，并将改进成果开源至 [OpenCloudOS 内核](https://gitee.com/OpenCloudOS/OpenCloudOS-Kernel.git)。
:::

::: details 什么是 PVM？（技术原理）
PVM（Pagetable-based Virtual Machine）是一种**基于页表的嵌套虚拟化框架**，构建于 KVM 之上。与传统嵌套虚拟化不同，PVM 不依赖宿主 hypervisor 向 guest 暴露 Intel VT-x / AMD-V 等硬件虚拟化扩展，而是在 guest 内核层通过共享内存区域和影子页表（shadow page table）来完成特权级切换与内存虚拟化，对宿主 hypervisor 完全透明。

PVM 最初由论文 [《PVM: Efficient Shadow Paging for Deploying Secure Containers in Cloud-native Environment》](https://dl.acm.org/doi/10.1145/3600006.3613158) 提出。腾讯云在此基础上进行了大量功能与性能改进、bugfix，并将相关工作开源至 [OpenCloudOS 内核](https://gitee.com/OpenCloudOS/OpenCloudOS-Kernel.git)，供社区使用。数年来，我们已在腾讯云生产环境部署了大量 PVM 实例，其可靠性已经过生产验证。
:::

## 前置条件

- **x86_64** 架构的 Linux 服务器（云 VM 或物理机均可）
- 有 **root 权限**
- **无需 `/dev/kvm`**（安装 PVM 内核后由 PVM 提供 KVM 能力）
- 其余要求与[快速开始](./quickstart.md#前置条件)相同（内存 ≥ 8 GB、XFS 或可用于 `/data/cubelet` 的分区等）

::: warning 以 root 身份执行所有操作
本文档中的所有命令均需在 **root** 用户下执行。请先切换到 root：

```bash
sudo su root
```

之后的所有命令直接在 root shell 中运行。
:::

## 第一步：安装 PVM 宿主机内核

前往 [CubeSandbox GitHub Releases](https://github.com/TencentCloud/CubeSandbox/releases) 页面，打开最新包含 PVM 内核附件的 Release，**在对应附件上右键 → 复制链接地址**，然后用 `wget` 下载。

根据你的 Linux 发行版选择对应格式：

### RPM 系（OpenCloudOS、RHEL、CentOS、TencentOS、Fedora）

在 Release 附件列表中找到以下文件，右键复制下载链接：

- `kernel-*cube.pvm.host*.x86_64.rpm`（内核主包）

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

### DEB 系（Ubuntu、Debian）

在 Release 附件列表中找到以下文件，右键复制下载链接：

- `linux-image-*cube.pvm.host*_amd64.deb`（内核主包）

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

::: tip 已经安装过 Cube Sandbox？
如果你已通过一键部署安装了 Cube Sandbox 普通版，**无需重新安装全套服务**。重启进入 PVM 内核后直接跳到[第二步](#第二步-安装-cube-sandbox-启用-pvm)即可。
:::

## 第二步：安装 Cube Sandbox（启用 PVM）

::: tip 为什么需要 `CUBE_PVM_ENABLE=1`？
发布包中包含两份 guest 内核：普通版（`vmlinux`）和 PVM 版（`vmlinux-pvm`）。`CUBE_PVM_ENABLE=1` 告知安装脚本将 PVM guest 内核覆盖安装为运行时使用的 `vmlinux`。不设置此变量则默认使用普通 guest 内核，PVM 不会生效。
:::

### 方式一：在线安装（推荐）

在线安装脚本将发布包下载到临时目录再执行，安装目录中无 `.env` 文件，因此直接在命令前加 `CUBE_PVM_ENABLE=1` 即可生效：

```bash
# 国内访问（推荐，从 CNB 拉取脚本）
curl -sL https://cnb.cool/CubeSandbox/CubeSandbox/-/git/raw/master/deploy/one-click/online-install.sh \
  | CUBE_PVM_ENABLE=1 MIRROR=cn bash
```

```bash
# 境外访问（从 GitHub 拉取脚本）
curl -sL https://github.com/tencentcloud/CubeSandbox/raw/master/deploy/one-click/online-install.sh \
  | CUBE_PVM_ENABLE=1 bash
```

如需手动指定节点 IP（多网卡机器建议显式指定）：

```bash
curl -sL https://cnb.cool/CubeSandbox/CubeSandbox/-/git/raw/master/deploy/one-click/online-install.sh \
  | CUBE_PVM_ENABLE=1 MIRROR=cn bash -s -- --node-ip=<你的服务器 IP>
```

### 方式二：手动下载发布包

从 [GitHub Releases](https://github.com/TencentCloud/CubeSandbox/releases) 下载 `cube-sandbox-one-click-<sha>.tar.gz`，解压后直接传入环境变量安装：

```bash
tar -xzf cube-sandbox-one-click-<sha>.tar.gz
cd cube-sandbox-one-click-<sha>

# 解压目录中不含 .env，环境变量可直接生效
CUBE_PVM_ENABLE=1 ./install.sh
```

::: warning env 文件覆盖陷阱
若你按照开发文档执行了 `cp env.example .env`，目录中会存在 `.env` 文件（其中默认 `CUBE_PVM_ENABLE=0`）。安装脚本会 `source` 该文件，**覆盖**父 shell 传入的同名变量，导致 PVM 未被启用。

此时需要在运行安装脚本前修改 `.env`：

```bash
sed -i 's/^CUBE_PVM_ENABLE=0/CUBE_PVM_ENABLE=1/' .env
# 验证
grep CUBE_PVM_ENABLE .env   # 期望：CUBE_PVM_ENABLE=1

./install.sh
```
:::

### 确认安装成功

安装日志中应出现以下行：

```
[one-click] CUBE_PVM_ENABLE=1, installed PVM guest kernel as .../cube-kernel-scf/vmlinux
```

## 第三步：验证 PVM 环境

```bash
# 确认运行时配置中 PVM 已启用
grep CUBE_PVM_ENABLE /usr/local/services/cubetoolbox/.one-click.env
# 期望：CUBE_PVM_ENABLE=1

# 确认 KVM 设备可用
ls -la /dev/kvm

# 确认 PVM KVM 模块已加载
lsmod | grep kvm_pvm
```

## 第四步：制作模板并开始使用

PVM 环境就绪后，后续流程与标准部署完全一致。请参阅快速开始中的[第三步：制作模板](./quickstart.md#第三步制作模板)，完成模板创建并运行你的第一个沙箱。

## 常见问题

**Q1：安装日志显示 `using ordinary guest kernel`，而非 PVM guest kernel**

**A：** 大概率是 `.env` 文件中的 `CUBE_PVM_ENABLE=0` 覆盖了环境变量，参见上方[方式二的注意事项](#方式二-手动下载发布包)。

---

**Q2：`lsmod | grep kvm_pvm` 无输出，或 `/dev/kvm` 不存在**

**A：** 执行 `uname -r` 确认已重启进入 PVM 内核，内核版本应包含 `cube.pvm.host`。确认后手动执行 `modprobe kvm_pvm`。若仍失败，检查内核包是否正确安装：

```bash
# RPM 系
rpm -qa | grep cube.pvm

# DEB 系
dpkg -l | grep cube.pvm
```
