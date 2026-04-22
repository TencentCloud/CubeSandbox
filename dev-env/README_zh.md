# Cube Sandbox 开发环境

[English](README.md)

> 一个用完即弃的虚拟机，用来在不污染宿主机的前提下开发 Cube Sandbox。

## 这是什么

一组 shell 脚本，在你的 Linux 宿主机上拉起一个一次性的 `OpenCloudOS 9`
虚拟机，并把 SSH 和 Cube API 端口转发回 localhost：

```text
SSH      : 127.0.0.1:10022 -> guest:22
Cube API : 127.0.0.1:13000 -> guest:3000
Cube HTTP: 127.0.0.1:11080 -> guest:80
Cube TLS : 127.0.0.1:11443 -> guest:443
```

适用场景：

- 在 Linux 笔记本上端到端体验 Cube Sandbox，不污染宿主机
- 修改本仓库代码后，在一个真实的 Cube Sandbox 安装里看到改动效果

**这不是生产部署方式**。生产请走
[`deploy/one-click/`](../deploy/one-click/)。

## 前置条件

- Linux x86_64 宿主机，已启用 KVM（存在 `/dev/kvm`）
- 宿主机开启了 nested virtualization（虚机内还要再起 MicroVM，必须能用
  `/dev/kvm`）
- 宿主机已安装：`qemu-system-x86_64`、`qemu-img`、`curl`、`ssh`、
  `scp`、`setsid`、`python3`、`rg`

快速自检：

```bash
ls -l /dev/kvm
cat /sys/module/kvm_intel/parameters/nested   # AMD 则是 kvm_amd，期望 Y / 1
```

## 快速上手

五步，按顺序执行。

### 第 1 步 &nbsp; 准备虚机镜像 &nbsp; *(一次性，约 10 分钟)*

```bash
./prepare_image.sh
```

下载 OpenCloudOS 9 云镜像，扩到 100G，并完成虚机内的初始化（扩根
文件系统、放宽 SELinux、修 PATH、安装登录 banner、安装 autostart
systemd unit）。完成后虚机自动关机。

只在首次搭建、或者删掉 `.workdir/` 之后再跑一次。

### 第 2 步 &nbsp; 启动虚机 &nbsp; *(终端 A)*

```bash
./run_vm.sh
```

QEMU 串口控制台挂在这个终端里。不要用 `Ctrl+a` 然后 `x` 直接退出 QEMU（相当于硬断电，可能导致异常）。请在另一个终端执行 `./login.sh` 登录 guest，在 guest 内执行 `poweroff` 正常关机；guest 关机后本终端里的 `run_vm.sh` 通常会随之结束。

### 第 3 步 &nbsp; 登录虚机 &nbsp; *(终端 B)*

```bash
./login.sh
```

直接进入 guest 内的 root shell，密码自动处理。

### 第 4 步 &nbsp; 在虚机内安装 Cube Sandbox &nbsp; *(每个新虚机一次)*

在第 3 步打开的 guest shell 里：

```bash
curl -sL https://github.com/tencentcloud/CubeSandbox/raw/master/deploy/one-click/online-install.sh | bash
```

跑完后应该能看到四个核心进程都活着（`network-agent`、`cubemaster`、
`cube-api`、`cubelet`）。

### 第 5 步 &nbsp; 验证 &nbsp; *(在虚机里)*

```bash
curl -sf http://127.0.0.1:3000/health && echo OK
```

看到 `OK` 就表示 Cube Sandbox 已经跑起来了。

## 让虚机重启后服务还在 &nbsp; *(一次性，强烈推荐)*

默认情况下 cube 组件是裸进程拉起的——虚机一重启就**不会**自动回来。
让 systemd 在每次开机时把它们带回来，**在宿主机**上跑（在第 5 步之后）：

```bash
./cube-autostart.sh            # 默认子命令：enable
```

会先交互确认，然后在 guest 内 enable `cube-sandbox-oneclick.service`。
之后每次开机都会自动跑 `up-with-deps.sh`，把 MySQL/Redis、cube-proxy、
coredns、network-agent、cubemaster、cube-api、cubelet 一并拉起。

其他子命令：

```bash
./cube-autostart.sh status     # 查看 is-enabled / is-active
./cube-autostart.sh disable    # 回退
```

## 开发：改代码、推到虚机、看效果

这才是 `dev-env/` 存在的真正价值。

在宿主机上改完代码后，一行命令把改动推进虚机：

```bash
./sync_to_vm.sh
```

它会按顺序做：

1. 在仓库根目录跑 `make all`
2. 在 guest 内把旧二进制原地 `mv` 成 `*.bak`（**只保留最近一份**备份）
3. 把新二进制 scp 到 `/usr/local/services/cubetoolbox/` 下对应路径
4. `systemctl restart cube-sandbox-oneclick.service`
5. 跑 `quickcheck.sh`；如果失败，**自动用 `.bak` 回滚**并重启

常用快捷方式：

```bash
# 跳过本地构建，直接用 _output/bin/ 里已有的产物
BUILD=0 ./sync_to_vm.sh

# 只同步指定组件
COMPONENTS="cubemaster cubelet" ./sync_to_vm.sh

# 推任意文件（不重启服务）
MODE=files FILES="./configs/foo.toml" REMOTE_DIR=/tmp ./sync_to_vm.sh

# 不走裸二进制，走官方 manual-release 流程
MODE=release ./sync_to_vm.sh
```

前置：第 4 步已完成；推荐先跑过 `./cube-autostart.sh`。

## 从虚机收日志

```bash
./copy_logs.sh
```

把 guest 内 `/data/log` 打包，放到 README 同目录下，文件名为
`data-log-<时间戳>.tar.gz`。

## 常见问题

| 现象 | 可能原因 | 解决方法 |
|------|---------|---------|
| 虚机内没有 `/dev/kvm` | 宿主机未开启 nested KVM | 在宿主机启用 nested virtualization，再重启虚机 |
| `./login.sh` 连不上 | 虚机还没启动，或宿主机 10022 端口被占用 | 确认 `./run_vm.sh` 还在运行，或换 `SSH_PORT` |
| 虚机里 `df -h /` 还是很小 | `prepare_image.sh` 没走完自动扩容 | 查看 `.workdir/qemu-serial.log`，然后把 `internal/grow_rootfs.sh` scp 进去手动跑一次 |
| 宿主机 13000 / 11080 / 11443 端口被占 | 本机有别的服务在用这些 dev-env 转发端口 | 用 `CUBE_API_PORT=23000 CUBE_PROXY_HTTP_PORT=21080 CUBE_PROXY_HTTPS_PORT=21443 ./run_vm.sh` |
| 虚机重启后 cube 组件没了 | 还没开启 autostart | 跑一次 `./cube-autostart.sh` |
| `sync_to_vm.sh` 自动回滚了 | `quickcheck` 用新二进制失败 | 看 guest 里 `/data/log/`，修 bug 后重新跑 `sync_to_vm.sh` |

## 参考

### 文件清单

```text
dev-env/
├── README.md / README_zh.md
├── prepare_image.sh        # 第 1 步
├── run_vm.sh               # 第 2 步
├── login.sh                # 第 3 步
├── cube-autostart.sh       # enable / disable / status systemd autostart unit
├── sync_to_vm.sh           # 开发循环
├── copy_logs.sh            # 拉 /data/log
└── internal/               # 由 prepare_image.sh 传进虚机执行
    ├── grow_rootfs.sh         # 扩根文件系统到 qcow2 虚拟大小
    ├── setup_selinux.sh       # SELinux 切 permissive（兼容 docker bind mount）
    ├── setup_path.sh          # /usr/local/{sbin,bin} 加进 PATH
    ├── setup_banner.sh        # /etc/profile.d/ 登录 banner
    └── setup_autostart.sh     # 安装 cube-sandbox-oneclick.service（不 enable）
```

生成的 qcow2、pid 文件、串口日志都放在 `.workdir/`。

### 环境变量

#### `prepare_image.sh`

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `AUTO_BOOT` | `1` | 启动虚机做 guest 内初始化。`0` 跳过（只下载 + 扩容）。 |
| `SETUP_AUTOSTART` | `1` | 安装 systemd autostart unit（**不**自动 enable）。`0` 跳过。 |
| `IMAGE_URL` | OpenCloudOS 9 | 覆盖源 qcow2 URL。 |
| `TARGET_SIZE` | `100G` | qcow2 最终虚拟大小。 |
| `SSH_PORT` | `10022` | 宿主机转发到 guest 22 的端口。 |

#### `run_vm.sh`

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `VM_MEMORY_MB` | `8192` | guest 内存。 |
| `VM_CPUS` | `4` | guest vCPU 数。 |
| `SSH_PORT` | `10022` | 宿主机 → guest SSH。 |
| `CUBE_API_PORT` | `13000` | 宿主机 → guest Cube API。 |
| `CUBE_PROXY_HTTP_PORT` | `11080` | 宿主机 → guest CubeProxy HTTP（`guest:80`）。 |
| `CUBE_PROXY_HTTPS_PORT` | `11443` | 宿主机 → guest CubeProxy HTTPS（`guest:443`）。 |
| `REQUIRE_NESTED_KVM` | `1` | 宿主机未开 nested KVM 时拒绝启动。`0` 跳过（沙箱跑不起来）。 |

#### `login.sh`

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `LOGIN_AS_ROOT` | `1` | `0` 保持普通用户身份。 |

#### `cube-autostart.sh`

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `ASSUME_YES` | `0` | `1` 跳过交互确认。 |
| `STOP_NOW` | `1` | 仅 `disable`：`0` 只在下次开机不起，不停掉当前进程。 |
| `UNIT_NAME` | `cube-sandbox-oneclick.service` | 覆盖 unit 名。 |

子命令：`enable`（默认）、`disable`、`status`。

#### `sync_to_vm.sh`

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `MODE` | `binaries` | `binaries` / `release` / `files`。 |
| `BUILD` | `1` | `0` 跳过 `make all` / `make manual-release`。 |
| `RESTART` | `1` | `0` 不在远端 `systemctl restart`。 |
| `COMPONENTS` | （全部） | `binaries` 模式：只推这些组件。 |
| `FILES` | — | `files` 模式：要 scp 的路径。 |
| `REMOTE_DIR` | `/tmp` | `files` 模式：远端落点目录。 |

#### `copy_logs.sh`

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `REMOTE_LOG_DIR` | `/data/log` | guest 内要打包的目录。 |
| `OUTPUT_DIR` | `dev-env/` | 宿主机上 tarball 的落点。 |

### 通用 SSH 覆盖（所有脚本都吃）

```bash
VM_USER=opencloudos VM_PASSWORD=opencloudos SSH_HOST=127.0.0.1 SSH_PORT=10022
```

## 说明

这个目录是**开发环境**。设计上就是单节点、密码登录、用完即弃，
请不要拿来跑真实业务。
