# 开发环境（QEMU 虚拟机）

手头只有一台**本地机器**（Linux 笔记本 / 工作站 / WSL 2），没有独立的云
服务器，但仍然想体验、或者直接上手改 Cube Sandbox？`dev-env/` 会在本机起
一台**一次性的 OpenCloudOS 9 虚机**，把 Cube Sandbox 装在虚机里跑，宿主机
保持干净。

三条命令即可上手：

```bash
cd CubeSandbox/dev-env
./create_vm.sh    # ① 创建虚机（仅首次，约 10 分钟）
./run_vm.sh       # ② 在终端 A 启动虚机
./login.sh        # ③ 在终端 B 登录，拿到 root shell
```

::: tip 先确认：你该用 dev-env 吗？
- **已经有云服务器** → 不用往下看了。直接走[快速开始](./quickstart.md)的
  PVM 部署：在一台普通云服务器（不需要 `/dev/kvm`）上装完整的 Cube Sandbox，
  比在本机再套一层虚机更快、也更贴近生产，细节见 [PVM 部署](./pvm-deploy.md)。
- **只有一台本地机器**（能开启 KVM 与嵌套虚拟化）→ 继续往下走。这正是
  `dev-env/` 要解决的场景：在虚机里随便编译、安装、折腾，不污染宿主机。
:::

::: danger 不是生产部署方式
`dev-env/` 是单机、密码认证、用完即弃的**开发 / 体验**环境。生产部署请走
[快速开始](./quickstart.md) 或 [多机集群部署](./multi-node-deploy.md)，在
bare-metal 机器上执行。
:::

## 前置条件

| 要求 | 说明 |
|------|------|
| 宿主机能使用 KVM | `/dev/kvm` 存在且可读写 |
| 已开启嵌套虚拟化 | Cube Sandbox 在虚机内还要再起一层 MicroVM，否则沙箱创建会失败 |
| 已安装 qemu、ssh 等命令 | `qemu-system-x86_64`（ARM64 上为 `qemu-system-aarch64`）、`qemu-img`、`curl`、`ssh`、`scp`、`setsid`、`python3` |

`dev-env/` 面向的是**本地**环境：**WSL 2（Windows 11 22H2+）**、**Linux
物理机**，或**本机已开启嵌套虚拟化的 Linux 虚机**。如果你的开发机是云服务器，
请改用[快速开始](./quickstart.md)的 PVM 部署，不必再套这一层虚机。

快速自检：

```bash
ls -l /dev/kvm
cat /sys/module/kvm_intel/parameters/nested   # AMD 则是 kvm_amd，期望 Y / 1
```

`/dev/kvm` 存在、`nested` 为 `Y`/`1`，就可以直接往下走了。

::: details 自检没过，或者想知道 ARM64 额外要装什么？
见文末 **附录 A：宿主机自检与 nested virtualization**。
:::

## 第 1 步：创建虚机（仅首次）

```bash
cd CubeSandbox/dev-env
./create_vm.sh
```

脚本会下载官方 OpenCloudOS 9 qcow2（约 800 MB）、把磁盘扩到 100 GB、
启动一次虚机完成初始化，然后干净关机。看到下面的输出就表示成功了：

```text
[create_vm][OK] All done:
[create_vm][OK]   1. Image downloaded
[create_vm][OK]   2. qcow2 resized to 100G
[create_vm][OK]   3. VM booted and guest root filesystem expanded
[create_vm][OK]   4. Guest SELinux set to permissive
[create_vm][OK]   5. VM powered off cleanly
[create_vm][INFO] Next steps:
[create_vm][INFO]   1. ./run_vm.sh    # boot the dev VM (terminal A)
[create_vm][INFO]   2. ./login.sh     # log in from another terminal (terminal B)
```

只需要跑一次。之后再想重建一台干净虚机，删掉 `dev-env/.workdir/` 重跑即可。

::: details 这十分钟里，虚机内被改了什么？
只有两处：**根文件系统撑满 100 GB**，以及**把 SELinux 调成 `permissive`**。
其他都没有动 —— 没有登录 banner、不改 `PATH` 与 `secure_path`、不装
systemd 自启服务，也没有同步二进制 / 收日志的辅助脚本。
细节见文末 **附录 B**。
:::

## 第 2 步：启动虚机（终端 A）

```bash
./run_vm.sh
```

QEMU 的串口控制台会挂在这个终端里，日志形如：

```text
[run_vm][INFO] Booting OpenCloudOS 9 VM
[run_vm][INFO]   Login user : opencloudos
[run_vm][INFO]   SSH        : ssh -p 10022 opencloudos@127.0.0.1
```

默认只转发 SSH（宿主机 `127.0.0.1:10022`）。需要访问虚机内的其他端口，
见下文「日常操作 · 访问虚机内的服务」。

::: warning 关机千万别用 `Ctrl+a` 然后 `x`
那相当于硬断电，可能损坏虚机状态。请在另一个终端用 `./login.sh` 登录后，
在虚机内执行 `poweroff`；虚机正常关机后，`run_vm.sh` 自己会退出。
:::

## 第 3 步：登录虚机（终端 B）

```bash
./login.sh
```

密码由脚本自动处理，登录后**直接是 root shell**（安装软件、起服务都需要
root）。SSH 端口、用户、密码要改的话见 **附录 C**。

## 在虚机里装你需要的东西

guest 就是一台普通的 OpenCloudOS 9 机器：`dnf`、`curl`、`docker` 都能直接
用。以 Cube Sandbox 为例，在虚机内执行官方一键安装：

```bash
curl -sL https://github.com/tencentcloud/CubeSandbox/raw/master/deploy/one-click/online-install.sh | bash
```

::: tip 国内环境建议走腾讯云镜像
```bash
curl -sL https://cnb.cool/CubeSandbox/CubeSandbox/-/git/raw/master/deploy/one-click/online-install.sh | MIRROR=cn bash
```
:::

安装完成后，按常规[快速开始](./quickstart.md)在虚机内创建模板、跑第一个
沙箱即可。

## 日常操作

### 传文件（宿主机 ↔ 虚机）

SSH 已转发到 `127.0.0.1:10022` 且允许密码登录（密码 `opencloudos`），
宿主机上直接用 `scp` / `rsync` / `ssh` 即可。

**宿主机 → 虚机**

```bash
# 单个文件
scp -P 10022 ./local-file opencloudos@127.0.0.1:/tmp/

# 整个目录（递归）
scp -P 10022 -r ./local-dir opencloudos@127.0.0.1:/tmp/

# 大目录增量同步，只传改动过的文件
rsync -avz -e 'ssh -p 10022' ./_output/bin/ opencloudos@127.0.0.1:/tmp/bin/
```

::: tip `scp` 登录的是普通用户，写不了系统目录
`opencloudos` 不是 root，`/usr/local/bin` 这类目录要先落到 `/tmp`，再用
`sudo` 安装：

```bash
scp -P 10022 ./cubelet opencloudos@127.0.0.1:/tmp/
ssh -p 10022 opencloudos@127.0.0.1 \
  'sudo install -m 0755 /tmp/cubelet /usr/local/bin/cubelet'
```
:::

**虚机 → 宿主机**

```bash
# 单个文件拉到当前目录
scp -P 10022 opencloudos@127.0.0.1:/data/log/cubelet.log ./

# 整个目录拉下来
scp -P 10022 -r opencloudos@127.0.0.1:/data/log ./guest-logs/

# 或者把命令输出直接写到宿主机文件
ssh -p 10022 opencloudos@127.0.0.1 'sudo journalctl -u cubelet --no-pager' > cubelet-journal.log
```

### 访问虚机内的服务

默认只转发 SSH，有两种补法：

**启动时按需转发**（推荐，长期使用）——`EXTRA_FORWARDS` 接受空格分隔的
`宿主机端口:虚机端口`，统一绑定在宿主机 `127.0.0.1`：

```bash
# Cube API -> 宿主机 13000，CubeProxy HTTPS -> 宿主机 11443
EXTRA_FORWARDS="13000:3000 11443:443" ./run_vm.sh
```

条目格式非法时，脚本会在 QEMU 启动前直接报错退出。

**临时开一条隧道**（不改启动参数）：

```bash
ssh -N -L 13000:127.0.0.1:3000 -p 10022 opencloudos@127.0.0.1
```

### 关机与重启

```bash
# 虚机内
poweroff

# 宿主机上重新启动同一台虚机（虚机内装的东西都还在）
./run_vm.sh
```

---

## 附录

下面是补充说明、参考表格与排查手册。按主线走通之后，遇到问题再来查即可。

### 附录 A：宿主机自检与 nested virtualization

宿主机需要的软件依赖：

- Linux x86_64 或 aarch64（ARM64），已启用 KVM（存在 `/dev/kvm`）
- 开启了 nested virtualization
- 已安装 `qemu-system-x86_64`（ARM64 上为 `qemu-system-aarch64`）、`qemu-img`、`curl`、`ssh`、`scp`、`setsid`、`python3`

::: details aarch64（ARM64）额外依赖
ARM64 上虚机使用 QEMU 的 `virt` 机型并以 UEFI 固件启动，因此还需安装
EDK2/AAVMF 固件（`QEMU_EFI.fd`，例如 `qemu-efi-aarch64` 包）。脚本会自动
检测宿主机架构，必要时可用 `TARGET_ARCH` 覆盖。
:::

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

### 附录 B：create_vm.sh 在虚机里做了什么

只做两件事，都发生在一次性的首次启动里：

- 把根分区和根文件系统撑满整个 100 GB 磁盘。
- 把 SELinux 切成 `permissive`（运行时 + `/etc/selinux/config`）。
  Cube Sandbox 的 MySQL 容器会把 `/docker-entrypoint-initdb.d` 以 bind
  mount 方式挂进容器；如果 SELinux enforcing 加上 `container-selinux`
  策略生效，容器进程会被拒绝，mysql 容器反复重启。

除这两项之外**都不自动化**：没有登录 banner、不改 `PATH` 与 `secure_path`、
不装 systemd 自启单元，也没有二进制同步和收日志的辅助脚本。虚机里需要
什么自己装，传文件用 `scp` / `rsync`。

### 附录 C：端口映射与环境变量

#### 端口映射

`run_vm.sh` 始终转发 SSH，其余端口通过 `EXTRA_FORWARDS` 按需追加：

| 宿主机 | 虚机 | 用途 |
|--------|------|------|
| `127.0.0.1:10022` | `:22` | 虚机 SSH |
| *（由 `EXTRA_FORWARDS` 指定）* | *（自选）* | 任何你想访问的端口，例如用 `13000:3000` 访问 Cube Sandbox 兼容 E2B 的 API |

#### 环境变量

::: details `create_vm.sh`
| 变量 | 默认值 | 说明 |
|------|--------|------|
| `IMAGE_URL` | OpenCloudOS 9.6 | 覆盖镜像下载地址。 |
| `IMAGE_PATH` | `.workdir/<镜像名>` | 虚机磁盘镜像的完整路径。 |
| `TARGET_SIZE` | `100G` | qcow2 最终虚拟大小。 |
| `SSH_PORT` | `10022` | 转发到 guest 22 的宿主机端口。 |
| `VM_USER` / `VM_PASSWORD` | `opencloudos` | 初始化脚本使用的 guest 凭据。 |
| `FORCE_KILL_ON_EXIT` | `0` | 失败时强制杀掉仍在运行的 QEMU，而不是留下它供排查。 |
:::

::: details `run_vm.sh`
| 变量 | 默认值 | 说明 |
|------|--------|------|
| `VM_MEMORY_MB` | `8192` | guest 内存。 |
| `VM_CPUS` | `4` | guest vCPU 数。 |
| `SSH_PORT` | `10022` | 宿主机 -> guest SSH。 |
| `EXTRA_FORWARDS` | *(空)* | 除 SSH 外额外转发的 `宿主机端口:虚机端口` 列表（空格分隔）。 |
| `REQUIRE_NESTED_KVM` | `1` | 宿主机没开 nested KVM 时拒绝启动。设为 `0` 可跳过（沙箱跑不起来）。 |
| `VM_BACKGROUND` | `0` | 设为 `1` 时用 `-daemonize` 后台启动，不占用串口控制台。 |
:::

::: details `login.sh`
| 变量 | 默认值 | 说明 |
|------|--------|------|
| `LOGIN_AS_ROOT` | `1` | 设为 `0` 时留在普通用户，不切 root。 |
:::

常用组合：

```bash
# 给虚机更多资源（默认 4 CPU / 8192 MB）
VM_MEMORY_MB=16384 VM_CPUS=8 ./run_vm.sh

# 额外把 Cube API 与 CubeProxy HTTPS 转发到宿主机
EXTRA_FORWARDS="13000:3000 11443:443" ./run_vm.sh

# 不强制要求 nested KVM（只想把系统起来看看，不跑沙箱）
REQUIRE_NESTED_KVM=0 ./run_vm.sh

# 登录后留在普通用户，不切 root
LOGIN_AS_ROOT=0 ./login.sh

# 创建更大的磁盘镜像（默认 100G）
TARGET_SIZE=200G ./create_vm.sh
```

所有脚本都支持用 `VM_USER` / `VM_PASSWORD` / `SSH_HOST` / `SSH_PORT`
覆盖 SSH 连接参数。

### 附录 D：dev-env 里的 cubecow 存储

Cubelet 默认使用 reflink-only 的 `cubecow` 存储后端。dev 虚机只需要
`data_path` 所在文件系统支持 reflink（例如 `xfs -m reflink=1` 或 Btrfs），
不再需要额外裸盘或 LVM / dm-thin 工具链。
`[plugins."io.cubelet.internal.v1.storage".cow.*]` 的默认配置会把 reflink
卷落在 `<data_path>/../cubecow-reflink` 目录下。

### 附录 E：重置 / 清理

```bash
# 1. 停掉正在运行的虚机（终端 A 里 Ctrl+C 之后，或在虚机内 poweroff）
# 2. 删掉虚机磁盘与状态
rm -rf dev-env/.workdir
# 3. 重新创建一台干净虚机
./create_vm.sh
```

开发环境本身就是一次性的：虚机里装乱了，重建即可。

### 附录 F：常见问题

| 现象 | 可能原因 | 解决方法 |
|------|---------|---------|
| 虚机内没有 `/dev/kvm` | 宿主机未开启 nested KVM | 在宿主机启用 nested virtualization 后重启虚机 |
| `./login.sh` 连不上 | 虚机还没启动，或宿主机 `10022` 端口被占 | 确认 `./run_vm.sh` 还在运行，或通过 `SSH_PORT` 换端口 |
| `cube-sandbox-mysql` 反复重启且报 `Permission denied` | 虚机里 SELinux 还是 enforcing | 在虚机里执行 `sudo setenforce 0 && sudo sed -i 's/^SELINUX=enforcing/SELINUX=permissive/' /etc/selinux/config && sudo docker restart cube-sandbox-mysql` |
| `df -h /` 仍然很小 | 虚机内扩容没走完 | 看 `.workdir/qemu-serial.log`，再 `scp -P 10022 internal/grow_rootfs.sh opencloudos@127.0.0.1:/tmp/`，在虚机里 `sudo` 手动跑一次 |
| `EXTRA_FORWARDS` 的端口被占 | 宿主机上已有服务占用该端口 | 换一个宿主机端口，例如 `EXTRA_FORWARDS="23000:3000"` |
| 写了非法的 `EXTRA_FORWARDS` 直接启动失败 | 条目不是 `宿主机端口:虚机端口` | 用空格分隔的数字对，例如 `EXTRA_FORWARDS="13000:3000"` |

### 附录 G：目录结构

```text
dev-env/
├── create_vm.sh       # 一次性：下载 + 扩容 + 首次启动时初始化虚机
├── run_vm.sh          # 日常：启动虚机
├── login.sh           # 日常：SSH 登录并切到 root
├── internal/          # create_vm.sh 传进虚机执行的辅助脚本
│   ├── grow_rootfs.sh
│   └── setup_selinux.sh
├── README.md
└── README_zh.md
```

更简短的说明见仓库里的
[`dev-env/README_zh.md`](https://github.com/tencentcloud/CubeSandbox/tree/master/dev-env)。
