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
