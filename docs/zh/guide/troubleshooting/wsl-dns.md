---
title: WSL2 上两个卡住一键安装与沙箱数据面的 DNS 问题
author: Tantanovo
date: 2026-08-03
tags:
  - deployment
  - networking
  - dns
  - wsl
lang: zh-CN
---

# WSL2 上两个卡住一键安装与沙箱数据面的 DNS 问题

在 Windows 上试用 Cube Sandbox，WSL2 是比较方便的选择，
[快速开始](../quickstart.md) 也把它列为支持的平台。有两个 DNS 问题是 WSL 特有的，
现有文档没有覆盖。它们出现在不同阶段、现象也完全不同，因此下面分开记录。

两个问题都是在真实的单机部署上复现并解决的，版本信息见[环境信息](#环境信息)。

## 问题现象

### 问题 1：安装前 preflight 直接 exit 3

`online-install.sh` 会立刻退出：

```text
[online-install] ERROR: DNS setup requires resolvectl or NetworkManager.
```

退出码是 `3`。此时还没有下载任何东西，也没有修改系统。

### 问题 2：沙箱能创建，但 `run_code` 解析失败

安装成功、控制面完全正常（沙箱一秒左右就创建好了），但第一次数据面调用就失败：

```text
[1] sandbox created in 0.99s
    sandbox_id: 9e0b35c6183f4e969e1a6a8f6bdc5629     <- 控制面正常
httpx.ConnectError: [Errno -2] Name or service not known   <- 数据面失败
```

报错里既没有 DNS 也没有 `cube.app`，很难往这个方向联想。它也可能出现在
**昨天还正常**的环境上，因为触发条件是重启 WSL，而不是你改了什么配置。

## 环境信息

- Cube Sandbox 版本：v0.6.0（`cubemastercli` `8721dd15`，构建于 2026-07-24）
- 部署模式：one-click 单机（control 与 compute 同机）
- 宿主机 OS / 内核：WSL2 上的 Ubuntu 24.04.4 LTS，`6.18.33.2-microsoft-standard-WSL2`，glibc 2.39
- 相关组件：`online-install.sh` preflight、CoreDNS（`cube-sandbox-coredns`）、CubeProxy、`deploy/one-click/scripts/systemd/dns-host-route-up.sh`

WSL 上其他前置条件当时都已满足，不属于本文范围：`/dev/kvm` 存在（较新版本的 WSL
默认开启嵌套虚拟化）、`/data/cubelet` 是带 `reflink=1` 的 loopback XFS
（见 [#311](https://github.com/TencentCloud/CubeSandbox/issues/311)）、
`/sys/fs/bpf` 已挂载、cgroup v2 的 `cpu` 控制器已暴露。

## 根因分析

### 问题 1：WSL 上既没有 `resolvectl` 也没有 NetworkManager

preflight 要求至少存在一个受支持的解析器管理组件
（`deploy/one-click/online-install.sh`）：

```bash
# DNS check (requires resolvectl or NetworkManager loaded status)
if ! command -v resolvectl >/dev/null 2>&1; then
  if command -v systemctl >/dev/null 2>&1; then
    nm_load_state="$(systemctl show -p LoadState --value NetworkManager 2>/dev/null || true)"
    if [[ "${nm_load_state}" != "loaded" ]]; then
      echo "[online-install] ERROR: DNS setup requires resolvectl or NetworkManager." >&2
      exit 3
    fi
  ...
```

这个检查本身是对的，前置条件也写在了
[自建部署](../self-build-deploy.md)（「DNS 路由：`systemd-resolved`（推荐）或
`NetworkManager + dnsmasq`」）。但在 WSL 上仍然容易卡住，有两个原因：

- WSL 上的 Ubuntu 默认安装**两者都没有**。`systemd-resolved` 没装，
  NetworkManager 也不存在，因此第一个分支就失败了。
- 报错提示的是**命令名** `resolvectl`，而这个命令是由 **`systemd-resolved` 包**
  提供的。包名没有出现在报错里，而 `apt install resolvectl` 并不存在，
  所以下一步该做什么并不明显。

### 问题 2：WSL 每次启动都会重写 `/etc/resolv.conf`

E2B 兼容 SDK 访问沙箱使用的是形如 `<port>-<sandboxId>.cube.app` 的独立域名。
由于 sandboxId 每次都变，客户端必须支持 `*.cube.app` 的通配解析；
一键安装自带的 CoreDNS 负责提供这个能力，`deploy/one-click/scripts/systemd/dns-host-route-up.sh`
会把 `cube.app` 查询路由到它。两条 DNS 后端路径下，面向客户端的 nameserver 都是
`169.254.254.53`（dummy link 地址）：`systemd-resolved` 路径下脚本通过 `resolvectl`
把这个地址挂到 `cube-dns0` 链路上；`dnsmasq` 回退路径下脚本把同一个地址写进
`/etc/resolv.conf`。`127.0.0.54` 只是 CoreDNS 的内部 loopback 绑定，
`dnsmasq` 会把 `cube.app` 查询转发给它，但它**不会出现在 `/etc/resolv.conf` 里**。
完整的寻址方案见 [HTTPS 与域名](../https-and-domain.md)。

而 WSL 默认在每次启动时重新生成 `/etc/resolv.conf`。在 `dnsmasq` 回退路径下，
这会静默地丢掉安装器写入的 nameserver；在 `systemd-resolved` 路径下，
路由挂在 `cube-dns0` dummy link 上，WSL 重启时链路被拆除，同样会丢。
无论哪种情况，客户端都会失去通配解析，于是：

- 控制面照常工作，因为它走 `127.0.0.1:3000`，不需要域名解析。
- 数据面失败，因为 `*.cube.app` 已经解析不出来了。

这也解释了为什么昨天还正常的环境今天会挂：触发条件是重启 WSL，而不是配置变更。

## 解决方法

### 问题 1：安装 `systemd-resolved` 以提供 `resolvectl`

```bash
sudo apt-get update
sudo apt-get install -y systemd-resolved
sudo systemctl enable --now systemd-resolved

command -v resolvectl   # 应当输出一个路径
```

之后重新执行 `online-install.sh`。如果你的发行版上没有 `systemd-resolved`，
文档给出的替代方案是 `NetworkManager + dnsmasq`；只要 `NetworkManager` 的
`LoadState=loaded`，preflight 同样会通过。

### 问题 2：先让 WSL 不再重写 resolv.conf，再写入

两步都要做，只做其中一步无法在重启后保留。

```bash
# 1. 让 WSL 不再自动生成 resolv.conf
sudo tee -a /etc/wsl.conf >/dev/null <<'EOF'

[network]
generateResolvConf = false
EOF

# 2. resolv.conf 通常是指向 /run 的符号链接，写入前需要先删掉
sudo rm -f /etc/resolv.conf
sudo tee /etc/resolv.conf >/dev/null <<'EOF'
nameserver 169.254.254.53
nameserver 223.5.5.5
EOF
```

这里有两个细节值得注意：

- `/etc/resolv.conf` 一般是指向 `/run/...` 的**符号链接**，
  透过符号链接写入不会保留，因此必须先删除。
- 第二行要保留一个上游解析器。如果只写 CoreDNS 地址，
  `*.cube.app` 能解析，但普通上网会断。

要写入的 nameserver 是 `169.254.254.53`，与走哪条 DNS 后端无关：它是
`systemd-resolved` 与 `dnsmasq` 两条路径都暴露给客户端的 dummy link 地址。
（`127.0.0.54` 只是 CoreDNS 的内部 loopback 绑定，`dnsmasq` 会把 `cube.app`
查询转发给它；它不是面向客户端的解析器，**不要写进 `/etc/resolv.conf`** ——
loopback 地址在 Docker 容器内不可达，而这正是引入 dummy link 地址要解决的问题。）
当前生效的地址可以从运行中的 Corefile 确认。

继续之前先把两个方向都验证一遍：

```bash
# 通配沙箱域名必须能解析
dig +short +tcp +timeout=3 foo.cube.app @169.254.254.53

# 普通解析也必须正常
getent hosts github.com
```

执行过 `wsl --shutdown` 之后，再确认文件是否还在：

```bash
cat /etc/resolv.conf
```

### 如果不想改宿主机 DNS

项目本身提供了绕开通配 DNS 的方式，在 WSL 上也很实用：

- **基于路径的访问** —— 通过
  `http://<cube-proxy-host>:<http-port>/sandbox/<sandbox-id>/<container-port>/`
  访问沙箱，不需要配置 DNS 与证书。适合 HTTP API；单页应用更适合用基于域名的方式，
  因为 CubeProxy 不会改写 HTML body。
- **`examples/e2b-dev-sidecar`** —— 一个本地代理，拦截 SDK 的数据面请求，
  改写 `Host` 头后转发给 CubeProxy。既不需要通配 DNS，也不需要信任自签证书。

两种方式都见 [HTTPS 与域名](../https-and-domain.md)。

## 关于 WSL 重启的提醒

有几项状态不会在 `wsl --shutdown` 后保留，且都会在下次运行时产生容易误判的报错：

- `/data/cubelet` 上的 loopback XFS 不会自动挂回，启动服务前需要先挂上
  （见 [#311](https://github.com/TencentCloud/CubeSandbox/issues/311)）。
- 除非按上文设置了 `generateResolvConf = false`，`/etc/resolv.conf` 会被重新生成。
- `systemd-resolved` 路径下的 `cube-dns0` dummy link 及其 `~cube.app` 路由
  也会随 WSL 重启消失，直到服务单元重启；`dnsmasq` 回退路径下，
  link 和 `server=/cube.app/...` 转发规则同理。

## 参考资料

- 相关文档：[快速开始](../quickstart.md)、
  [自建部署](../self-build-deploy.md)、
  [HTTPS 与域名](../https-and-domain.md)
- 相关 issue：[#311](https://github.com/TencentCloud/CubeSandbox/issues/311)
  （WSL 的 XFS loopback 方案）、
  [#411](https://github.com/TencentCloud/CubeSandbox/issues/411)
  （在 WSL2 上运行 Cube Sandbox）
- 在处理 [#644](https://github.com/TencentCloud/CubeSandbox/issues/644) 期间验证；
  部署证据见 [#1238](https://github.com/TencentCloud/CubeSandbox/pull/1238)
