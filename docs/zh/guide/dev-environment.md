# 开发环境（QEMU 虚拟机）

如果你没有一台独占的 bare-metal 服务器，只有一台笔记本或者云主机，但仍
然想体验或开发 Cube Sandbox，可以直接在宿主机里起一台**一次性的
OpenCloudOS 9 虚机**，把 Cube Sandbox 装在虚机里跑。

仓库根目录下的 `dev-env/` 已经把整个流程脚本化了：一次性的镜像准备、
虚机启动、自动登录三件套。

只需要三条命令，你就能获得一个CubeSandbox体验环境。

## 前置条件

本文的开发环境脚本需要跑在下面这三种宿主机之一：

- **Windows 上的 WSL 2**（需要 Windows 11 22H2+，并在 WSL 里启用嵌套虚拟化）
- **Linux 物理机**
- **已开启嵌套虚拟化的 Linux 虚拟机**（云主机 / 本地虚机均可）

三者共同要求：宿主机能正常使用 KVM —— `/dev/kvm` 存在且可读写。

Cube Sandbox 在虚机内还要继续用 KVM 起 MicroVM，所以宿主机必须支持
嵌套虚拟化，否则虚机里拿不到可用的 `/dev/kvm`，沙箱创建会失败。

更细的软件依赖、KVM 自检与启用方法见下文『宿主机自检』章节。

## 快速上手

先克隆仓库，进入 `dev-env/`：

```bash
git clone https://github.com/tencentcloud/CubeSandbox.git
cd CubeSandbox/dev-env
```

一共三条命令。前两条在同一个终端执行，第三条在**新终端**里执行。

### 第一步：准备镜像（仅首次）

```bash
./prepare_image.sh
```

脚本会从腾讯镜像站下载官方 OpenCloudOS 9 qcow2，并完成初始化操作。

一个镜像只需要跑一次——除非你删掉了 `.workdir/`，或者想重新生成一份
干净镜像。

### 第二步：启动虚机

```bash
./run_vm.sh
```

QEMU 串口会挂在当前终端。想关机按 `Ctrl+a` 然后 `x`。

### 第三步：登录虚机（新开一个终端）

```bash
./login.sh
```

`login.sh` 会登录到虚拟机内（root 权限），因为 Cube Sandbox 的部署需要
在 root 下进行。

### 在虚机内安装 Cube Sandbox

进入 root shell 之后，直接执行标准的一键安装：

```bash
curl -sL https://github.com/tencentcloud/CubeSandbox/raw/master/deploy/one-click/online-install.sh | bash
```

::: tip 国内环境建议走腾讯云镜像
```bash
curl -sL https://cnb.cool/CubeSandbox/CubeSandbox/-/git/raw/master/deploy/one-click/online-install.sh | MIRROR=cn bash
```
:::

安装完成后按常规 [快速开始](./quickstart) 在虚拟机内创建模板、跑第一个沙箱即可。

---

以下是对上面流程的补充说明与排查手册，正常情况下可以先跳过。

## 适用场景与边界

- 想要一个干净的 OpenCloudOS 9 环境快速体验 Cube Sandbox。
- 只有笔记本 / 云主机（已开启 KVM 和 nested virtualization），没有物理服务器。
- 想在虚机里迭代 Cube Sandbox，不污染宿主机。

::: warning 不是生产部署方式
这里明确是**开发 / 体验**环境。生产部署请走
[快速开始](./quickstart) 或 [多机集群部署](./multi-node-deploy)，在
bare-metal 机器上执行。
:::

## 宿主机自检

宿主机需要的软件依赖：

- Linux x86_64，已启用 KVM（存在 `/dev/kvm`）
- 开启了 nested virtualization
- 已安装 `qemu-system-x86_64`、`qemu-img`、`curl`、`ssh`、`scp`、`setsid`

快速自检：

```bash
ls -l /dev/kvm

# Intel
cat /sys/module/kvm_intel/parameters/nested
# AMD
cat /sys/module/kvm_amd/parameters/nested
```

如果 `nested` 返回 `N` 或 `0`，请先在宿主机开启它。以 Intel 为例：

```bash
echo 'options kvm_intel nested=1' | sudo tee /etc/modprobe.d/kvm.conf
sudo modprobe -r kvm_intel && sudo modprobe kvm_intel
```

## 宿主机 ↔ 虚机端口映射

`run_vm.sh` 启动时会自动配置下面这两条转发：

| 宿主机 | 虚机 | 用途 |
|--------|------|------|
| `127.0.0.1:10022` | `:22` | 虚机 SSH |
| `127.0.0.1:13000` | `:3000` | Cube Sandbox 兼容 E2B 的 API |

## prepare_image.sh 在虚机内做了什么

- 把根分区和根文件系统撑满整个 100 GB 磁盘。
- 把 SELinux 切成 `permissive`（运行时 + `/etc/selinux/config`）。
  Cube Sandbox 的 MySQL 容器会把 `/docker-entrypoint-initdb.d` 以 bind
  mount 方式挂进容器，如果 SELinux enforcing 加上 `container-selinux`
  策略生效，容器进程会被拒绝，mysql 容器反复重启。
- 把 `/usr/local/{sbin,bin}` 加到登录 shell 的 `PATH` 和 sudo 的
  `secure_path`，这样无论登录 shell 还是 `sudo cmd`，都能直接调用
  `cubemastercli` 等二进制。
- 往 `/etc/profile.d/cubesandbox-banner.sh` 写入登录 banner，之后每次
  登录都会看到 `Welcome to the Cube Sandbox development environment!`。

## 常用环境变量

三个脚本都支持用环境变量覆盖默认值：

```bash
# 本次只下载并扩容 qcow2，不跑 guest 内的自动扩容。
AUTO_BOOT=0 ./prepare_image.sh

# 启动虚机时换更多资源 / 换 Cube API 端口。
VM_MEMORY_MB=16384 VM_CPUS=8 CUBE_API_PORT=23000 ./run_vm.sh

# 不强制要求 nested KVM（只想把系统起来看看，不跑沙箱）。
REQUIRE_NESTED_KVM=0 ./run_vm.sh

# 登录时留在普通用户，不切 root。
LOGIN_AS_ROOT=0 ./login.sh
```

`run_vm.sh` 的默认配置：4 CPU、8192 MB 内存，SSH 转发
`127.0.0.1:10022`，Cube API 转发 `127.0.0.1:13000`（虚机内 `:3000`）。

## 重置 / 清理

- 想重置虚机状态：先停掉 `run_vm.sh`，删掉 `dev-env/.workdir/` 目录，
  再重跑 `./prepare_image.sh`。
- 开发环境本身就是一次性的。一旦虚机里装的东西乱了，重建即可。

## 常见问题

| 现象 | 可能原因 | 解决方法 |
|------|---------|---------|
| 虚机内没有 `/dev/kvm` | 宿主机未开启 nested KVM | 在宿主机启用 nested virtualization 后重启虚机 |
| `./login.sh` 连不上 | 虚机还没启动，或宿主机 `10022` 端口被占 | 确认 `./run_vm.sh` 还在运行，或通过 `SSH_PORT` 换端口 |
| `cube-sandbox-mysql` 反复重启且报 `Permission denied` | 虚机里 SELinux 还是 enforcing | 重跑 `./prepare_image.sh`；或在虚机里执行 `setenforce 0 && sed -i 's/^SELINUX=enforcing/SELINUX=permissive/' /etc/selinux/config && docker restart cube-sandbox-mysql` |
| `df -h /` 仍然很小 | 虚机内自动扩容没走完 | 看 `.workdir/qemu-serial.log`，再把 `internal/grow_rootfs.sh` scp 进去手动跑一次 |
| 宿主机 `13000` 端口被占 | 本机有别的服务 | 用 `CUBE_API_PORT=23000 ./run_vm.sh`，并相应调整 `E2B_API_URL` |

## 目录结构

```text
dev-env/
├── prepare_image.sh   # 一次性：下载 + 扩容 qcow2 + 虚机内初始化
├── run_vm.sh          # 日常：启动虚机
├── login.sh           # 日常：SSH 登录并切到 root
├── internal/          # prepare_image.sh 自动传进虚机执行的辅助脚本
│   ├── grow_rootfs.sh
│   ├── setup_selinux.sh
│   ├── setup_path.sh
│   └── setup_banner.sh
├── README.md
└── README_zh.md
```

短版说明见仓库的
[`dev-env/README_zh.md`](https://github.com/tencentcloud/CubeSandbox/tree/master/dev-env/README_zh.md)。
