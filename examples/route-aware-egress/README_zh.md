# Route-Aware Egress 示例

这些示例演示启用 cube-router 后，如何让 Sandbox 出站流量跟随宿主机 Linux 路由表，而不是固定从主网卡发出。

这两个示例的宿主机网络环境依赖较强，因此 Python 脚本只负责创建 Sandbox 并在 Sandbox 内产生流量。GRE tunnel、双网卡路由、云厂商安全组、远端网关 NAT 等配置需要用户根据自己的环境提前准备。

## 前置条件

- CubeSandbox 已安装并运行。
- 已启用 cube-router：

```bash
CUBE_SANDBOX_CUBE_ROUTER_ENABLE=1
```

- 已有可用模板 ID。
- 安装 Python 依赖：

```bash
pip install -r requirements.txt
```

- 配置本地环境变量：

```bash
cp .env.example .env
# 编辑 .env
```

## 示例一：双网卡

适用于 Cube 节点有两块网卡的场景：

- `eth0` 通过默认路由访问公网。
- `eth1` 通过更精确的 host route 访问某个内网目标。

宿主机示例配置：

```bash
ip route get 1.1.1.1
ip route replace 10.206.0.12/32 via 10.206.0.1 dev eth1 src 10.206.0.15
ip route get 10.206.0.12
```

`.env` 示例：

```bash
export PUBLIC_TARGET_IP="1.1.1.1"
export PUBLIC_TARGET_TCP_PORT="53"
export SECONDARY_NIC_TARGET_IP="10.206.0.12"
# 可选：当内网目标有 TCP 服务时填写。
export SECONDARY_NIC_TARGET_TCP_PORT=""
export PRIMARY_NIC_NAME="eth0"
export SECONDARY_NIC_NAME="eth1"
```

运行：

```bash
python dual_nic.py
```

预期结果：

- Sandbox 能连接 `PUBLIC_TARGET_IP:PUBLIC_TARGET_TCP_PORT`。
- Sandbox 会向 `SECONDARY_NIC_TARGET_IP` 发送 UDP 探测包，通过宿主机
  `tcpdump` 验证该流量是否走第二块网卡。
- 如果设置了 `SECONDARY_NIC_TARGET_TCP_PORT`，脚本还会验证内网目标的 TCP
  可达性。
- `tcpdump -ni cube-router` 能看到两类流量。
- 公网目标流量最终从 `eth0` 发出。
- 内网目标流量最终从 `eth1` 发出。

## 示例二：GRE Tunnel Gateway

适用于需要将部分 Sandbox 出站流量送入 GRE tunnel，并由 GRE remote 节点作为网关继续访问目标网络或公网的场景。

宿主机环境假设：

- Cube 节点已有 GRE 设备，例如 `natgre`。
- Cube 节点 GRE 地址为 `169.254.100.1`。
- GRE remote 地址为 `169.254.100.2`。
- 宿主机路由已经把目标 Sandbox 出站流量送入 `natgre`。
- GRE remote 节点已开启转发和 NAT。

常用检查命令：

```bash
ip addr show natgre
ip route get 169.254.100.2
tcpdump -ni cube-router 'host 169.254.100.2 or host 1.1.1.1'
tcpdump -ni natgre 'host 169.254.100.2 or host 1.1.1.1'
```

`.env` 示例：

```bash
export GRE_TUNNEL_NAME="natgre"
export GRE_REMOTE_TUNNEL_IP="169.254.100.2"
export GRE_INTERNET_TARGET_IP="1.1.1.1"
export GRE_INTERNET_TARGET_TCP_PORT="53"
export GRE_UNDERLAY_REMOTE_IP="<remote-node-public-or-private-ip>"
```

运行：

```bash
python gre_tunnel_gateway.py
```

预期结果：

- Sandbox 会向 GRE remote tunnel IP 发送 UDP 探测包，通过 `tcpdump` 验证该
  流量是否进入 GRE tunnel。
- Sandbox 能通过 GRE remote gateway 连接
  `GRE_INTERNET_TARGET_IP:GRE_INTERNET_TARGET_TCP_PORT`。
- 抓包能看到流量先经过 `cube-router`，再进入 GRE 设备，最后走 GRE underlay。

## 排查

| 现象 | 可能原因 | 处理方式 |
| --- | --- | --- |
| `missing required environment variable` | `.env` 仍是占位符 | 填写真实目标 IP |
| Sandbox 访问不了内网目标 | 宿主机路由没有选中第二块网卡 | 在 Cube 节点执行 `ip route get <target>` |
| Sandbox 能访问 GRE peer 但不能访问公网 | GRE remote 未开启转发或 NAT | 检查 remote 节点 `ip_forward` 和 NAT 规则 |
| `cube-router` 上没有包 | cube-router 未启用，或 Sandbox 是旧网络配置下创建的 | 启用 cube-router 后重新创建 Sandbox |
| `ping: Operation not permitted` | Sandbox 镜像没有给 `ping` 授予 `CAP_NET_RAW` | 使用本示例里的 TCP/UDP socket 探测，不依赖 `ping` |
