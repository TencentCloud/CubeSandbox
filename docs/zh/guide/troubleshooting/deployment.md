---
title: 部署相关排障
lang: zh-CN
---

# 部署相关排障

| 标题 | 描述 | 相关 Issues |
| --- | --- | --- |
| `/data/cubelet` 必须是 XFS（reflink） | `cubelet` 把 `/data/cubelet` 作为容器可写层的存储目录，依赖 XFS 的 reflink 特性。在 Ubuntu / Debian / WSL 等 ext4 根盘的环境上部署，one-click 前置检查会以 `not XFS` 报错退出。Workaround：用 loopback `.img` 格式化为 XFS 后挂到 `/data/cubelet`；生产建议挂独立 XFS 数据盘（100–300 GiB）；新装机器推荐 OpenCloudOS 9 / RHEL 系。 | [#311](https://github.com/TencentCloud/CubeSandbox/issues/311), [#245](https://github.com/TencentCloud/CubeSandbox/issues/245) |
| 沙箱网段和局域网冲突导致创建模板超时 | one-click 部署默认沙箱网段是 `192.168.0.0/18`。如果宿主机局域网也使用 `192.168.1.x`，Cube 可能给沙箱分配到和真实局域网重叠的 IP 导致模板创建或端口探测以 `context deadline exceeded` 失败。将 Cubelet CIDR 改成不冲突的网段，并在重启前清理旧 TAP 网卡和 `cube-dev`。 | [指南](./local-network-cidr-conflict.md) |
| 调整沙箱网段时的 CIDR 冲突（残留 cube-dev） | 停服后 `cube-dev` 网卡和 `z*` TAP 设备会残留；调整 `CUBE_SANDBOX_NETWORK_CIDR` 时若新网段与残留 `cube-dev` 重叠，预检会拦截并提示确定性清理（仅 reboot 不够）。相同网段重装会自动复用，不受影响。 | [指南](./local-network-cidr-conflict.md#调整沙箱网段时的-cidr-冲突残留-cube-dev) |
| Ubuntu 上 cgroup v2 没启用 `cpu` controller，cubelet CPU quota 不生效 | Ubuntu / Debian 云镜像默认不会把 cgroup v2 的 `cpu` controller 委托到子 cgroup，且 `multipathd` 的 RT 线程会让 `+cpu` 写入返回 `Invalid argument`。详细复现和修复见 issue。 | [#366](https://github.com/TencentCloud/CubeSandbox/issues/366) |
| bpffs 未挂载 | Cubelet 内嵌网络运行时会在 `/sys/fs/bpf` 下固定 eBPF 程序和 map；因此该目录不是 `bpf` 文件系统时，安装器会在修改系统前退出。 | [指南](#bpffs-未挂载) |

## bpffs 未挂载

Cubelet 内嵌网络运行时依赖 `/sys/fs/bpf` 挂载 `bpffs`（bpf 文件系统）以固定（pin）eBPF 程序与 Map。在 WSL2 或部分默认未挂载 `bpffs` 的极简 Linux 环境中部署时，one-click 预检会拦截并在修改系统前退出。

### 1. 确认内核支持

先确认当前运行的内核是否支持 `bpf` 文件系统：

```bash
grep -w bpf /proc/filesystems
```

若命令无任何输出，说明当前内核未启用 eBPF/bpffs 支持（缺少 `CONFIG_BPF_SYSCALL`），需升级或更换支持 eBPF 的内核后再试。

### 2. 挂载 bpffs

若内核支持，以 root 权限创建目录（若不存在）并执行挂载：

```bash
mkdir -p /sys/fs/bpf
mount -t bpf bpf /sys/fs/bpf
```

### 3. 持久化配置（可选）

如需重启后保持挂载，可在 `/etc/fstab` 中添加如下配置：

```text
bpf /sys/fs/bpf bpf defaults 0 0
```
