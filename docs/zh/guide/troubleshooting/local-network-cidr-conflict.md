---
title: 沙箱网段和局域网冲突导致创建模板超时
author: luzhixing12345
date: 2026-05-20
tags:
  - deployment
  - networking
  - one-click
lang: zh-CN
---

# 沙箱网段和局域网冲突导致创建模板超时

## 问题现象

创建模板在 `CREATING_TEMPLATE` 阶段失败：

```bash
cubemastercli run fail: template tpl-xx creation failed: context deadline exceeded
```

或者执行 cube-bench 压测时报错

```bash
~/CubeSandbox/examples/cube-bench$ ./bin/cube-bench -c 20 -n 200
...
╭────────────────────────────────────────────────╮ ╭────────────────────────────────────────────────────────────────────────────────────────────────╮
│  Live Stats                                    │ │  Recent Operations                                                                             │
│                                                │ │                                                                                                │
│   Completed    54 / 200                        │ │   #  74  ERR  create HTTP 500: {"code":500,"message":"CubeMas...                               │
│   Errors       20                              │ │   #  73  ERR  create HTTP 500: {"code":500,"message":"CubeMas...                               │
│   QPS          2.4 req/s                       │ │   #  72  ERR  create HTTP 500: {"code":500,"message":"CubeMas...                               │
│   Avg Create   1254 ms                         │ │   #  69  ERR  create HTTP 500: {"code":500,"message":"CubeMas...                               │
│   Avg Delete   864 ms                          │ │   #  70  ERR  create HTTP 500: {"code":500,"message":"CubeMas...                               │
│   Elapsed      29.1s                           │ │   #  71  ERR  create HTTP 500: {"code":500,"message":"CubeMas...                               │
│                                                │ │   #  68  ERR  create HTTP 500: {"code":500,"message":"CubeMas...                               │
╰────────────────────────────────────────────────╯ │   #  67  ERR  create HTTP 500: {"code":500,"message":"CubeMas...                               │
                                                   │                                                                                                │
                                                   ╰────────────────────────────────────────────────────────────────────────────────────────────────╯

create HTTP 500: {"code":500,"message":"CubeMaster returned error code 130595: context deadline exceeded"}
```

## 环境信息

- Cube Sandbox 版本：e29453
- 部署模式：裸金属或本地物理机部署
- 宿主机 OS / 内核：Ubuntu22.04 / Linux 6.6
- 相关组件：`Cubelet`、持久化 TAP 网卡

## 根因分析

在本地物理机或裸金属部署时，Cube 会创建大量持久化 TAP 网卡，名称类似 `z192.168.0.x` 或 `z192.168.1.x`。

其默认配置位于 `Cubelet/config/config.toml`，默认创建 500 个 `192.168.0.0/18` 网段下的 TAP 网卡

```toml
[plugins]
  [plugins."io.cubelet.internal.v1.network"]
    object_dir = "/usr/local/services/cubetoolbox/cube-vs/network"
    eth_name = "eth0"
    tap_init_num = 500
    cidr = "192.168.0.0/18"
```

Cube 默认沙箱 CIDR 是 `192.168.0.0/18`，如果宿主机局域网也使用相关网段，例如 `192.168.1.x`，那么沙箱地址会和真实局域网重叠，导致路由和端口探测异常。

执行 ./bin/cube-bench -c 20 -n 200 压测后发现报错，查看 Cubelet 日志 `/data/log/Cubelet/Cubelet-req.log`

```bash
$ rg 'PortBindingFailed|probe \\[|Create fail|RunCubeSandboxRequest|sandboxIP|port_mappings]' /data/log/Cubelet/Cubelet-req.log | jq
{
  "CalleeEndpoint": "",
  "CalleeAction": "Create",
  "Action": "Create",
  "InstanceId": "16157c528b224e9eacc6307a2af5705e",
  "RequestId": "069f93aa-927d-4436-aac0-cc0aa8a89ca9",
  "@timestamp": "2026-05-20T10:10:16.896601536-04:00",
  "InstanceType": "cubebox",
  "Callee": "cubebox",
  "Version": "release",
  "CodeLine": "",
  "FunctionType": "cubebox",
  "Caller": "cubebox-service",
  "Namespace": "default",
  "RetCode": 0,
  "LogContent": "[cubebox] fail:PortBindingFailed The initialization timeout or detecting 192.168.1.40 port failed.",
  "LocalIp": "192.168.1.123",
  "Module": "Cubelet",
  "LogLevel": "ERROR"
}
{
  "InstanceId": "",
  "Callee": "workflow",
  "RetCode": 130459,
  "Namespace": "default",
  "Module": "Cubelet",
  "@timestamp": "2026-05-20T10:10:16.896681441-04:00",
  "LogLevel": "ERROR",
  "CalleeEndpoint": "",
  "FunctionType": "cubebox",
  "Version": "release",
  "InstanceType": "cubebox",
  "LogContent": "Create fail:requestID:\"069f93aa-927d-4436-aac0-cc0aa8a89ca9\"
  ret:{
    ret_code:PortBindingFailed
    ret_msg:\"The initialization timeout or detecting 192.168.1.40 port failed.\"} 
    sandboxID:\"16157c528b224e9eacc6307a2af5705e\"
    sandboxIP:\"192.168.1.40\" 
    port_mappings:{container_port:49983 host_port:20588}
    port_mappings:{container_port:49999 host_port:20589}",
  "Caller": "cubebox-service",
  "CalleeAction": "Create",
  "Action": "Create",
  "LocalIp": "192.168.1.123",
  "CodeLine": "",
  "RequestId": "069f93aa-927d-4436-aac0-cc0aa8a89ca9"
}
```

如果宿主机会同时存在两条相关路由

```bash
$ ip route
192.168.0.0/18 dev cube-dev proto kernel scope link src 192.168.0.1
192.168.1.0/24 dev enp56s0f0 proto kernel scope link src 192.168.1.123 metric 100
```

192.168.1.0/24 比 /18 更精确，所以访问 192.168.1.40 时可能走真实物理网卡 enp56s0f0，而不是 Cube 的 cube-dev/TAP 路径，导致 Cubelet 探测沙箱端口时连不到真正的沙箱

## 解决方案

先停止服务

```bash
sudo systemctl stop 'cube-sandbox-*.target'
```

修改配置把 Cubelet 的网络 CIDR 改成不和宿主机局域网冲突的网段，例如改为 `172.31.64.0/18`：

```bash
sudo sed -i 's#cidr = "192.168.0.0/18"#cidr = "172.31.64.0/18"#' \
  /usr/local/services/cubetoolbox/Cubelet/config/config.toml
```

删除旧的持久化 TAP 网卡和 cube-dev 网卡

```bash
sudo ip link delete cube-dev 2>/dev/null || true
ip tuntap show | awk -F: '/^z[0-9]+\./{print $1}' \
  | xargs -r -n1 -I{} sudo ip tuntap del dev {} mode tap
```

重启服务

```bash
sudo systemctl start 'cube-sandbox-*.target'
```

网段不再和宿主机局域网重叠后，重新创建模板，成功。

## 相关：调整沙箱网段时的 CIDR 冲突（残留 `cube-dev`）

停止 Cube Sandbox 服务并不会删除 `cube-dev` dummy 网卡和持久化的 `z<ip>` TAP 设备，它们会一直残留，直到 reboot 或手动清理。用相同网段重装会自动复用现有 `cube-dev`，无需处理；但**调整网段**时，若新 CIDR 与残留的 `cube-dev` 网络重叠，预检会拦截并给出明确指引：

```
[one-click] ERROR: CUBE_SANDBOX_NETWORK_CIDR '192.168.0.0/17' overlaps an existing cube-dev network (192.168.0.0/18).

  Changing the sandbox CIDR on a host that already has a cube network is
  disruptive: the old cube-dev and the persistent z* TAP devices are left
  stale. A reboot alone is NOT enough -- the systemd target is enabled and
  network-agent rebuilds the old network from config.toml on boot.

  To change the CIDR, fully reset the cube network first:
    sudo systemctl stop 'cube-sandbox-*.target'
    sudo ip link delete cube-dev 2>/dev/null || true
    ip tuntap show | awk -F: '/^z[0-9]+\./{print $1}' \
      | xargs -r -n1 -I{} sudo ip tuntap del dev {} mode tap
  then re-run install with the new CIDR.

  Or keep the existing CIDR (192.168.0.0/18) to reuse the current network.

  To bypass this check (not recommended), set:
    CUBE_SANDBOX_NETWORK_CIDR_SKIP_CONFLICT_CHECK=1
```

仅 reboot 并不够 —— systemd target 是 enabled 的，开机后 `network-agent` 会按 `config.toml` 重建 `cube-dev` 和 TAP 设备。按报错中给出的步骤做确定性清理，再用新的 `CUBE_SANDBOX_NETWORK_CIDR` 重新安装。若新网段不与残留 `cube-dev` 重叠，预检放行，`network-agent` 会把 `cube-dev` 协调到新网段。
