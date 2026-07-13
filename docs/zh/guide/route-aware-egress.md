# 路由感知出网

默认情况下，CubeSandbox 保持原有出网行为：CubeVS 会把沙箱出站包直接送到节点主网卡。这条路径简单，适合单网卡节点，但无法让 Linux 路由表选择其他出口设备，例如第二块网卡、GRE 隧道、VXLAN 设备、VPN 接口或策略路由路径。

可选的 **cube-router** 模式只改变宿主机侧的出网路径。沙箱网络策略、DNS allow-list 学习、CubeEgress L7 代理、端口映射的语义保持不变。

## 什么时候启用

当沙箱流量需要遵循宿主机路由，而不是固定从主网卡发出时，可以启用 cube-router。常见场景包括：

- 节点有多块网卡，不同目的 CIDR 需要走不同出口。
- 某些目标只能通过 GRE、VXLAN、WireGuard 或内部 VPN 等隧道设备访问。
- 希望支持 route-aware sandbox egress，但不想修改宿主机全局默认路由。

如果主网卡直出已经满足需求，保持关闭即可。为了兼容升级，默认不启用。

## 工作方式

关闭 cube-router 时，CubeVS 使用原有的直接 SNAT 路径：

```mermaid
flowchart LR
    sandbox[Sandbox] --> tap[TAP]
    tap --> fromCube[CubeVS from_cube]
    fromCube --> nic[主网卡]
```

启用 cube-router 时，CubeVS 先把出站包归一化到一个内部 NAT IP，再注入到名为 `cube-router` 的内部 dummy 设备。随后由 Linux 内核完成正常的 forwarding、conntrack、路由查询和最终 MASQUERADE：

```mermaid
flowchart LR
    sandbox[Sandbox] --> tap[TAP]
    tap --> fromCube[CubeVS from_cube]
    fromCube --> router[cube-router]
    router --> kernel[Linux 路由表]
    kernel --> egress[选中的出口设备]
```

这意味着 CubeSandbox 不需要理解每一种网络设备类型。只要 Linux 能把流量路由到该设备，沙箱出网就可以使用它。

端口映射仍然绑定在节点主网卡上，不会暴露到所有可能的出网设备。

## 安装时启用

在运行 one-click 安装或升级前，在 `deploy/one-click/.env` 中设置：

```bash
CUBE_SANDBOX_CUBE_ROUTER_ENABLE=1
# 可选。留空时会从 CUBE_SANDBOX_NETWORK_CIDR 派生地址。
CUBE_SANDBOX_CUBE_ROUTER_CIDR=
```

如果 `CUBE_SANDBOX_CUBE_ROUTER_CIDR` 为空，CubeSandbox 会从沙箱 CIDR 末尾保留两个可用 IP。默认沙箱 CIDR 是 `192.168.0.0/18`，对应为：

| 地址 | 用途 |
| --- | --- |
| `192.168.63.253/32` | `cube-router` 设备 IP |
| `192.168.63.254` | 进入宿主机路由前使用的内部 SNAT IP |

如果指定自定义 cube-router CIDR，它必须是对齐的私有 IPv4 CIDR，掩码范围为 `/16` 到 `/30`。CubeSandbox 使用 `.1` 作为 router IP，`.2` 作为内部 SNAT IP：

```bash
CUBE_SANDBOX_CUBE_ROUTER_ENABLE=1
CUBE_SANDBOX_CUBE_ROUTER_CIDR=10.254.0.0/24
```

自定义 CIDR 不能和宿主机已有路由或接口地址冲突。

首次启用 cube-router 时，已有沙箱的外访存量连接会中断；应用重连后会按新的 cube-router 路径正常建立连接。

## 验证方式

安装完成后，先检查设备、路由和 NAT 规则：

```bash
ip addr show cube-router
ip route | grep cube-router
iptables -t nat -S POSTROUTING | grep MASQUERADE
```

创建一个具备出网权限的沙箱，然后访问一个应当走非主网卡路由的目标：

```bash
ip route get <destination-ip>
tcpdump -ni cube-router host <destination-ip>
tcpdump -ni <egress-device> host <destination-ip>
```

正常情况下，能先在 `cube-router` 上看到包，再在宿主机路由选中的出口设备上看到包。最终出口设备上的包源地址应当是 MASQUERADE 后的宿主机出口 IP。

## 注意事项

- cube-router 不改变沙箱的 `allow_out`、`deny_out` 或 L7 `rules` 语义。
- CubeEgress 仍然处理由 CubeVS 和宿主机 TPROXY 规则选中的 HTTP/HTTPS 流量。
- 不要为了测试 cube-router 修改宿主机全局默认路由。建议为待验证目标添加更精确的 host route 或策略路由。
- 如需关闭该功能，设置 `CUBE_SANDBOX_CUBE_ROUTER_ENABLE=0`，然后走正常升级/重装流程，让生成配置和宿主机网络状态重新收敛。

## 示例 1：GRE Remote 作为公网网关

这个示例把默认出网路径切到 GRE 隧道，由 GRE remote 节点转发访问公网。`10.0.0.0/8`、`172.16.0.0/12`、`192.168.0.0/16` 等私网流量仍然走 Cube 节点物理网卡。

如果你是通过 SSH 远程操作服务器，替换默认路由前需要先为 GRE underlay 对端和 SSH 客户端保留物理网卡路由。

示例拓扑：

| 角色 | 示例值 |
| --- | --- |
| Cube 节点 underlay IP | `203.0.113.10` |
| Cube 节点物理网卡 | `eth0` |
| Cube 节点物理网关 | `203.0.113.1` |
| GRE remote underlay IP | `198.51.100.20` |
| GRE tunnel 名称 | `natgre` |
| Cube 侧 GRE IP | `169.254.100.1/30` |
| Remote 侧 GRE IP | `169.254.100.2/30` |
| GRE remote 公网出口网卡 | `eth0` |

云厂商安全组或防火墙需要放通两个 underlay IP 之间的 GRE 流量，GRE 是 IP protocol `47`。

在 GRE remote 节点执行：

```bash
#!/usr/bin/env bash
set -euo pipefail

CUBE_UNDERLAY_IP="203.0.113.10"
REMOTE_UNDERLAY_IP="198.51.100.20"
REMOTE_EGRESS_NIC="eth0"
TUNNEL_NAME="natgre"
REMOTE_TUNNEL_CIDR="169.254.100.2/30"

modprobe ip_gre
ip tunnel del "${TUNNEL_NAME}" 2>/dev/null || true
ip tunnel add "${TUNNEL_NAME}" mode gre local "${REMOTE_UNDERLAY_IP}" remote "${CUBE_UNDERLAY_IP}" ttl 255
ip addr replace "${REMOTE_TUNNEL_CIDR}" dev "${TUNNEL_NAME}"
ip link set "${TUNNEL_NAME}" up mtu 1476

sysctl -w net.ipv4.ip_forward=1
iptables -t nat -C POSTROUTING -s 169.254.100.0/30 -o "${REMOTE_EGRESS_NIC}" -j MASQUERADE 2>/dev/null \
  || iptables -t nat -A POSTROUTING -s 169.254.100.0/30 -o "${REMOTE_EGRESS_NIC}" -j MASQUERADE
```

在 Cube 节点执行：

```bash
#!/usr/bin/env bash
set -euo pipefail

CUBE_UNDERLAY_IP="203.0.113.10"
REMOTE_UNDERLAY_IP="198.51.100.20"
UNDERLAY_NIC="eth0"
UNDERLAY_GW="203.0.113.1"
TUNNEL_NAME="natgre"
CUBE_TUNNEL_CIDR="169.254.100.1/30"
SSH_CLIENT_IP=""

modprobe ip_gre
ip tunnel del "${TUNNEL_NAME}" 2>/dev/null || true
ip tunnel add "${TUNNEL_NAME}" mode gre local "${CUBE_UNDERLAY_IP}" remote "${REMOTE_UNDERLAY_IP}" ttl 255
ip addr replace "${CUBE_TUNNEL_CIDR}" dev "${TUNNEL_NAME}"
ip link set "${TUNNEL_NAME}" up mtu 1476

# 固定 GRE underlay 路径，避免默认路由切到 GRE 后，GRE 外层包也被送回 GRE。
ip route replace "${REMOTE_UNDERLAY_IP}/32" via "${UNDERLAY_GW}" dev "${UNDERLAY_NIC}" src "${CUBE_UNDERLAY_IP}"

# 私网流量仍然走物理网卡，而不是 GRE 公网网关。请根据你的内网实际拓扑调整。
ip route replace 10.0.0.0/8 via "${UNDERLAY_GW}" dev "${UNDERLAY_NIC}" src "${CUBE_UNDERLAY_IP}"
ip route replace 172.16.0.0/12 via "${UNDERLAY_GW}" dev "${UNDERLAY_NIC}" src "${CUBE_UNDERLAY_IP}"
ip route replace 192.168.0.0/16 via "${UNDERLAY_GW}" dev "${UNDERLAY_NIC}" src "${CUBE_UNDERLAY_IP}"

# 如果通过 SSH 远程操作，建议把 SSH_CLIENT_IP 设置为你的 SSH 客户端源 IP，
# 避免替换默认路由后 SSH 会话断开。
if [[ -n "${SSH_CLIENT_IP}" ]]; then
  ip route replace "${SSH_CLIENT_IP}/32" via "${UNDERLAY_GW}" dev "${UNDERLAY_NIC}" src "${CUBE_UNDERLAY_IP}"
fi

# 默认出网路径切到 GRE remote 网关。
ip route replace default dev "${TUNNEL_NAME}"
```

预期路由表片段：

```bash
default dev natgre scope link
10.0.0.0/8 via 203.0.113.1 dev eth0 src 203.0.113.10
172.16.0.0/12 via 203.0.113.1 dev eth0 src 203.0.113.10
192.168.0.0/16 via 203.0.113.1 dev eth0 src 203.0.113.10
198.51.100.20 via 203.0.113.1 dev eth0 src 203.0.113.10
169.254.100.0/30 dev natgre proto kernel scope link src 169.254.100.1
```

在 Cube 节点验证：

```bash
ip addr show natgre
ip route get 1.1.1.1
ip route get 10.1.2.3
tcpdump -ni cube-router 'host 1.1.1.1 or host 169.254.100.2'
tcpdump -ni natgre 'host 1.1.1.1 or host 169.254.100.2'
tcpdump -ni eth0 'proto gre or host 198.51.100.20 or net 10.0.0.0/8'
```

随后创建一个 `allow_out` 包含隧道对端、公网目标和待验证内网目标的沙箱，例如 `169.254.100.2/32`、`1.1.1.1/32` 和 `10.1.2.3/32`。正常情况下，公网流量会先出现在 `cube-router`，再进入 `natgre`，最后由 GRE remote 节点做 NAT 后访问公网；私网流量会先出现在 `cube-router`，随后从物理网卡发出。

## 示例 2：双网卡，一个访问公网，一个访问内网

这个示例让宿主机默认路由走一块网卡访问公网，同时让私网流量走另一块物理网卡。沙箱流量进入 `cube-router` 后，会按同一套路由表选择出口。

示例拓扑：

| 角色 | 示例值 |
| --- | --- |
| 公网出口网卡 | `eth0`，`10.206.0.4` |
| 公网网关 | `10.206.0.1` |
| 内网出口网卡 | `eth1`，`10.206.0.15` |
| 内网网关 | `10.206.0.1` |
| 公网目标 | `1.1.1.1` |
| 内网目标 | `10.50.0.12` |

在 Cube 节点配置路由：

```bash
# 公网默认流量走 eth0。
ip route replace default via 10.206.0.1 dev eth0 src 10.206.0.4

# 私网流量走 eth1。
ip route replace 10.0.0.0/8 via 10.206.0.1 dev eth1 src 10.206.0.15
ip route replace 172.16.0.0/12 via 10.206.0.1 dev eth1 src 10.206.0.15
ip route replace 192.168.0.0/16 via 10.206.0.1 dev eth1 src 10.206.0.15
```

预期路由表片段：

```bash
default via 10.206.0.1 dev eth0 src 10.206.0.4
10.0.0.0/8 via 10.206.0.1 dev eth1 src 10.206.0.15
172.16.0.0/12 via 10.206.0.1 dev eth1 src 10.206.0.15
192.168.0.0/16 via 10.206.0.1 dev eth1 src 10.206.0.15
```

测试沙箱前，先确认宿主机路由选择符合预期：

```bash
ip route get 1.1.1.1
ip route get 10.50.0.12
```

第一条应该选择 `eth0`，第二条应该选择 `eth1`。

创建一个 `allow_out` 包含两个目标的沙箱，例如 `1.1.1.1/32` 和 `10.50.0.12/32`，然后在沙箱里执行：

```bash
ping -c 4 1.1.1.1
ping -c 4 10.50.0.12
```

在 Cube 节点抓包验证：

```bash
tcpdump -ni cube-router 'host 1.1.1.1 or host 10.50.0.12'
tcpdump -ni eth0 'host 1.1.1.1 or host 10.50.0.12'
tcpdump -ni eth1 'host 1.1.1.1 or host 10.50.0.12'
```

预期结果：

- 访问 `1.1.1.1` 的流量先出现在 `cube-router`，随后出现在 `eth0`。
- 访问 `10.50.0.12` 的流量先出现在 `cube-router`，随后出现在 `eth1`。
- 最终物理网卡上的源 IP 是 MASQUERADE 后的对应网卡 IP。
