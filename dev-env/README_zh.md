# Cube Sandbox 开发环境

[English](README.md)

> 一个用完即弃的 OpenCloudOS 9 虚机，用来在不污染宿主机的
> 前提下折腾 Cube Sandbox。

## 这是什么

三个小脚本，在你的 Linux 宿主机上创建并驱动一台一次性的
`OpenCloudOS 9` 虚机，并把 SSH 转发回 localhost：

```text
SSH : 127.0.0.1:10022 -> guest:22
```

虚机内只做两件自动配置：把根盘撑到 100G，以及把 SELinux 调成
`permissive`。其余一切 —— 装 Cube Sandbox、装 Docker、装任何工具 ——
都由你自己在虚机里完成。

适用场景：

- 在 Linux 笔记本上端到端体验 Cube Sandbox，不污染宿主机
- 想要一台干净的 OpenCloudOS 9 机器，拿到 root shell 后随便装东西

**如果你已经有云服务器，就不需要 dev-env**：直接按
[快速开始](../docs/zh/guide/quickstart.md)的 PVM 部署方式，在一台普通云
服务器上装完整的 Cube Sandbox 即可。

**这不是生产部署方式**。生产请走
[`deploy/one-click/`](../deploy/one-click/)。

## 前置条件

- Linux x86_64 或 aarch64（ARM64）宿主机，已启用 KVM（存在 `/dev/kvm`）
- 宿主机开启了 nested virtualization（虚机内还要再起 MicroVM，必须能用
  `/dev/kvm`）
- 宿主机已安装：`qemu-system-x86_64`（ARM64 上为 `qemu-system-aarch64`）、
  `qemu-img`、`curl`、`ssh`、`scp`、`setsid`、`python3`
  - 在 aarch64 上虚机以 QEMU `virt` 机型和 UEFI 固件启动，因此还需要
    EDK2/AAVMF 固件（`QEMU_EFI.fd`，例如 `qemu-efi-aarch64` 包）。
    脚本会自动检测宿主机架构，必要时可用 `TARGET_ARCH` 覆盖。

快速自检：

```bash
ls -l /dev/kvm
cat /sys/module/kvm_intel/parameters/nested   # AMD 则是 kvm_amd，期望 Y / 1
```

## 快速上手

### 第 1 步 &nbsp; 创建虚机 &nbsp; *(一次性，约 10 分钟)*

```bash
./create_vm.sh
```

下载 OpenCloudOS 9 云镜像、扩到 100G、启动一次虚机完成虚机内的
根文件系统扩容和 SELinux permissive 设置，然后干净关机。

只在首次搭建、或者删掉 `.workdir/` 之后再跑一次。

### 第 2 步 &nbsp; 启动虚机 &nbsp; *(终端 A)*

```bash
./run_vm.sh
```

QEMU 串口控制台挂在这个终端里。不要用 `Ctrl+a` 然后 `x` 直接退出 QEMU
（相当于硬断电，可能导致异常）。请在另一个终端执行 `./login.sh` 登录
guest，在 guest 内执行 `poweroff` 正常关机；guest 关机后本终端里的
`run_vm.sh` 通常会随之结束。

### 第 3 步 &nbsp; 登录虚机 &nbsp; *(终端 B)*

```bash
./login.sh
```

直接进入 guest 内的 root shell，密码自动处理
（`opencloudos` / `opencloudos`）。

### 第 4 步 &nbsp; 在虚机里装你需要的东西 &nbsp; *(在 guest 内)*

guest 就是一台普通的 OpenCloudOS 9 机器，`dnf`、`curl`、`docker`
都能直接用。例如跑 Cube Sandbox 的一键安装脚本：

```bash
curl -sL https://github.com/tencentcloud/CubeSandbox/raw/master/deploy/one-click/online-install.sh | bash
```

国内环境建议走腾讯云镜像：

```bash
curl -sL https://cnb.cool/CubeSandbox/CubeSandbox/-/git/raw/master/deploy/one-click/online-install.sh | MIRROR=cn bash
```

## 在宿主机和虚机之间传文件

SSH 已转发到 `127.0.0.1:10022`，且虚机允许密码登录，因此宿主机上直接用
`scp` / `rsync` / `ssh` 即可。虚机密码是 `opencloudos`。

**宿主机 → 虚机**

```bash
# 单个文件
scp -P 10022 ./local-file opencloudos@127.0.0.1:/tmp/

# 整个目录（递归）
scp -P 10022 -r ./local-dir opencloudos@127.0.0.1:/tmp/

# 大目录增量同步（只传改动过的文件）
rsync -avz -e 'ssh -p 10022' ./_output/bin/ opencloudos@127.0.0.1:/tmp/bin/
```

`scp` / `rsync` 以普通用户 `opencloudos` 登录，无法直接写
`/usr/local/bin` 这类目录。先放到 `/tmp`，再用 `sudo` 落位：

```bash
scp -P 10022 ./cubelet opencloudos@127.0.0.1:/tmp/
ssh -p 10022 opencloudos@127.0.0.1 \
  'sudo install -m 0755 /tmp/cubelet /usr/local/bin/cubelet'
```

**虚机 → 宿主机**

```bash
# 单个文件拉到当前目录
scp -P 10022 opencloudos@127.0.0.1:/data/log/cubelet.log ./

# 整个目录拉下来
scp -P 10022 -r opencloudos@127.0.0.1:/data/log ./guest-logs/

# 或者把命令输出直接写到宿主机文件
ssh -p 10022 opencloudos@127.0.0.1 'sudo journalctl -u cubelet --no-pager' > cubelet-journal.log
```

**访问虚机内的服务**

默认只转发 SSH。想访问虚机内某个服务监听的端口，要么启动虚机时用
`EXTRA_FORWARDS`（见下节），要么临时开一条隧道：

```bash
ssh -N -L 13000:127.0.0.1:3000 -p 10022 opencloudos@127.0.0.1
```

## 追加虚机端口转发

`run_vm.sh` 默认只转发 SSH。`EXTRA_FORWARDS` 接受空格分隔的
`宿主机端口:虚机端口` 列表：

```bash
# 把 Cube API 转到宿主机 13000，CubeProxy HTTPS 转到 11443
EXTRA_FORWARDS="13000:3000 11443:443" ./run_vm.sh
```

所有转发都绑定在宿主机的 `127.0.0.1`。格式非法的条目会在 QEMU 启动前
直接报错退出。

## 参考

### 文件清单

```text
dev-env/
├── README.md / README_zh.md
├── create_vm.sh            # 第 1 步：下载镜像、扩容、首次启动初始化
├── run_vm.sh               # 第 2 步：启动虚机
├── login.sh                # 第 3 步：SSH 登录并切到 root
└── internal/               # create_vm.sh 传进虚机执行的脚本
    ├── grow_rootfs.sh         # 把根文件系统撑满 qcow2 虚拟大小
    └── setup_selinux.sh       # SELinux -> permissive（docker bind mount 需要）
```

生成的产物（qcow2、pid 文件、串口日志）都在 `.workdir/` 下。

### 环境变量

#### `create_vm.sh`

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `IMAGE_URL` | OpenCloudOS 9.6 | 覆盖镜像下载地址。 |
| `IMAGE_PATH` | `.workdir/<镜像名>` | 虚机磁盘镜像的完整路径。 |
| `TARGET_SIZE` | `100G` | qcow2 最终虚拟大小。 |
| `SSH_PORT` | `10022` | 转发到 guest 22 的宿主机端口。 |
| `VM_USER` / `VM_PASSWORD` | `opencloudos` | 初始化脚本使用的 guest 凭据。 |
| `FORCE_KILL_ON_EXIT` | `0` | 失败时强制杀掉仍在运行的 QEMU，而不是留下它供排查。 |

#### `run_vm.sh`

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `VM_MEMORY_MB` | `8192` | guest 内存。 |
| `VM_CPUS` | `4` | guest vCPU 数。 |
| `SSH_PORT` | `10022` | 宿主机 -> guest SSH。 |
| `EXTRA_FORWARDS` | *(空)* | 除 SSH 外额外转发的 `宿主机端口:虚机端口` 列表（空格分隔）。 |
| `REQUIRE_NESTED_KVM` | `1` | 宿主机没开 nested KVM 时拒绝启动。设为 `0` 可跳过（沙箱跑不起来）。 |
| `VM_BACKGROUND` | `0` | 设为 `1` 时用 `-daemonize` 后台启动，不占用串口控制台。 |

#### `login.sh`

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `LOGIN_AS_ROOT` | `1` | 设为 `0` 时留在普通用户，不切 root。 |

### 通用 SSH 覆盖（所有脚本都吃）

```bash
VM_USER=opencloudos VM_PASSWORD=opencloudos SSH_HOST=127.0.0.1 SSH_PORT=10022
```

## 重置 / 清理

先停掉正在运行的 `run_vm.sh`，删掉 `dev-env/.workdir/`，再重跑
`./create_vm.sh`。开发环境本身就是一次性的，虚机里装乱了重建即可。

## 常见问题

| 现象 | 可能原因 | 解决方法 |
|------|---------|---------|
| 虚机内没有 `/dev/kvm` | 宿主机未开启 nested KVM | 在宿主机启用 nested virtualization 后重启虚机 |
| `./login.sh` 连不上 | 虚机还没启动，或宿主机 `10022` 端口被占 | 确认 `./run_vm.sh` 还在运行，或通过 `SSH_PORT` 换端口 |
| `df -h /` 仍然很小 | 虚机内扩容没走完 | 看 `.workdir/qemu-serial.log`，再 `scp -P 10022 internal/grow_rootfs.sh opencloudos@127.0.0.1:/tmp/`，在虚机里 `sudo` 手动跑一次 |
| `cube-sandbox-mysql` 反复重启且报 `Permission denied` | 虚机里 SELinux 还是 enforcing | 在虚机里执行 `sudo setenforce 0 && sudo sed -i 's/^SELINUX=enforcing/SELINUX=permissive/' /etc/selinux/config && sudo docker restart cube-sandbox-mysql` |
| `EXTRA_FORWARDS` 的端口被占 | 宿主机上已有服务占用该端口 | 换一个宿主机端口，例如 `EXTRA_FORWARDS="23000:3000"` |

## 说明

这个目录是**开发环境**：单机、密码认证、用完即弃。不要用它承载真实业务。
