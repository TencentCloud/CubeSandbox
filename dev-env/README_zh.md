# Cube Sandbox 开发环境

[English](README.md)

一个可丢弃的 `OpenCloudOS 9` 虚机，用来开发和体验 `Cube Sandbox`，避免污染宿主机。

虚机镜像来自腾讯镜像站官方 OpenCloudOS 云镜像，磁盘已扩到 100G，
并把 SSH 和 Cube API 端口映射到宿主机：

```text
SSH      : 127.0.0.1:2222 -> guest:22
Cube API : 127.0.0.1:3000 -> guest:3000
```

## 前置条件

- Linux x86_64 宿主机，已启用 KVM（存在 `/dev/kvm`）
- 宿主机开启了 nested virtualization（虚机内跑 Cube Sandbox MicroVM 必需）
- 已安装 `qemu-system-x86_64`、`qemu-img`、`curl`、`ssh`、`scp`、`setsid`

快速检查：

```bash
ls -l /dev/kvm
cat /sys/module/kvm_intel/parameters/nested   # AMD 则是 kvm_amd
```

## 启动开发环境

一共三步。前两步在同一个终端执行，第三步在**新终端**里执行。

### 1. 准备镜像（仅首次）

```bash
./prepare_image.sh
```

会下载 OpenCloudOS 9 qcow2、扩到 100G，并完成虚机内的一系列初始化
（扩根文件系统、SELinux、PATH、登录 banner 等），完成后自动关机。

只需要在首次搭建、或者你删掉了 `.workdir/` 之后再跑一次。

### 2. 启动虚机

```bash
./run_vm.sh
```

QEMU 串口控制台会挂在这个终端里。需要关机时按 `Ctrl+a` 然后 `x`。

### 3. 登录虚机（新开一个终端）

```bash
./login.sh
```

会自动以 `opencloudos` 用户 SSH 进去，并通过 `sudo -i` 直接切到 root shell。
密码自动处理，不需要每次手输 `opencloudos`。

如果只想以普通用户登录、不切 root：

```bash
LOGIN_AS_ROOT=0 ./login.sh
```

## 文件说明

```text
dev-env/
├── README.md
├── README_zh.md
├── prepare_image.sh   # 一次性：下载 + 扩容 qcow2 + 虚机内扩根文件系统 + 放宽 SELinux + 安装 banner
├── run_vm.sh          # 日常：启动虚机
├── login.sh           # 日常：SSH 登录并切到 root
├── internal/          # 由 prepare_image.sh 自动传进虚机执行的辅助脚本
│   ├── grow_rootfs.sh     # 把 guest 根文件系统扩到 qcow2 的虚拟磁盘大小
│   ├── setup_selinux.sh   # 把 guest SELinux 切成 permissive（兼容 docker bind mount）
│   ├── setup_path.sh      # 把 /usr/local/{sbin,bin} 加到登录 PATH 以及 sudo 的 secure_path
│   └── setup_banner.sh    # 安装 /etc/profile.d/ 下的登录 banner
└── .gitignore         # 忽略 .workdir/
```

生成的镜像、pid 文件、串口日志都放在 `.workdir/`。

## 在虚机里安装 Cube Sandbox

登录后在虚机里执行官方一键安装：

```bash
curl -sL https://github.com/tencentcloud/CubeSandbox/raw/master/deploy/one-click/online-install.sh | bash
```

确认虚机内可以正常使用 KVM（沙箱能跑的必要条件）：

```bash
ls -l /dev/kvm
egrep -c '(vmx|svm)' /proc/cpuinfo
```

由于宿主机 `:3000` 已经转发到 guest `:3000`，在宿主机这边可以直接把 SDK 指向开发环境：

```bash
export E2B_API_URL="http://127.0.0.1:3000"
export E2B_API_KEY="dummy"
export CUBE_TEMPLATE_ID="<template-id>"
```

## 常用变量

三个脚本都支持环境变量覆盖：

```bash
# 本次只下载并扩容 qcow2，不进 guest 做自动扩容。
AUTO_BOOT=0 ./prepare_image.sh

# 启动虚机时用更多资源、换个 Cube API 端口。
VM_MEMORY_MB=16384 VM_CPUS=8 CUBE_API_PORT=13000 ./run_vm.sh

# 不强制要求 nested KVM（只想把系统启动起来看看，不跑沙箱）。
REQUIRE_NESTED_KVM=0 ./run_vm.sh

# 登录时不切 root，保留为普通用户。
LOGIN_AS_ROOT=0 ./login.sh
```

## 常见问题

| 现象 | 可能原因 | 解决方法 |
|------|---------|---------|
| 虚机内没有 `/dev/kvm` | 宿主机未开启 nested KVM | 在宿主机启用 nested virtualization，再重启虚机 |
| `./login.sh` 连不上 | 虚机还没启动，或宿主机 2222 端口被占用 | 确认 `./run_vm.sh` 还在运行，或换 `SSH_PORT` |
| 虚机里 `df -h /` 还是很小 | `prepare_image.sh` 没走完自动扩容 | 查看 `.workdir/qemu-serial.log`，然后把 `internal/grow_rootfs.sh` scp 进去手动跑一次 |
| 宿主机 3000 端口被占用 | 本机有别的服务在用 | 用 `CUBE_API_PORT=13000 ./run_vm.sh` |

## 说明

这个目录是 **Cube Sandbox 的开发环境**，用来开发和体验，**不是**生产部署方式。
