# Network Agent 配置指南

本文档详细说明 Network Agent 的所有配置选项。

---

## 配置方式

Network Agent 支持三种配置方式，优先级从高到低:

1. **命令行参数** - 最高优先级
2. **Cubelet TOML 配置文件** - 中等优先级
3. **默认值** - 最低优先级

---

## 命令行参数

### 服务器配置

| 参数 | 默认值 | 描述 |
|------|--------|------|
| `--listen` | `unix:///tmp/cube/network-agent.sock` | HTTP API 监听地址 |
| `--grpc-listen` | `unix:///tmp/cube/network-agent-grpc.sock` | gRPC API 监听地址 |
| `--health-listen` | `127.0.0.1:19090` | 健康检查监听地址 |
| `--tap-fd-listen` | `unix:///tmp/cube/network-agent-tap.sock` | TAP FD 服务器监听地址 |

**监听地址格式:**
- Unix Socket: `unix:///path/to/socket.sock`
- TCP: `tcp://host:port` 或 `host:port`

### 网络配置

| 参数 | 默认值 | 描述 |
|------|--------|------|
| `--eth-name` | (必填) | 节点上行网络接口名称 |
| `--cidr` | `192.168.0.0/18` | TAP 沙箱 IP 地址池 |
| `--mvm-inner-ip` | `169.254.68.6` | Guest 内部可见 IP |
| `--mvm-mac-addr` | `20:90:6f:fc:fc:fc` | Guest MAC 地址 |
| `--mvm-gw-dest-ip` | `169.254.68.5` | Guest 网关 IP |
| `--mvm-gw-mac-addr` | `20:90:6f:cf:cf:cf` | Gateway MAC 地址 |
| `--mvm-mask` | `30` | Guest 网络掩码位数 |
| `--mvm-mtu` | `1500` | Guest MTU 大小 |

### 其他配置

| 参数 | 默认值 | 描述 |
|------|--------|------|
| `--cubelet-config` | (无) | Cubelet TOML 配置文件路径 |
| `--state-dir` | `/usr/local/services/cubetoolbox/network-agent/state` | 状态持久化目录 |
| `--host-proxy-bind-ip` | `127.0.0.1` | Host Proxy 绑定 IP |
| `--tap-init-num` | `0` | 预分配的 TAP 设备数量 |

---

## 完整命令行示例

### 最小配置

```bash
./network-agent --eth-name=eth0
```

### 完整配置

```bash
./network-agent \
    --listen=unix:///var/run/network-agent.sock \
    --grpc-listen=unix:///var/run/network-agent-grpc.sock \
    --health-listen=0.0.0.0:19090 \
    --tap-fd-listen=unix:///var/run/network-agent-tap.sock \
    --eth-name=eth0 \
    --cidr=192.168.0.0/18 \
    --mvm-inner-ip=169.254.68.6 \
    --mvm-mac-addr=20:90:6f:fc:fc:fc \
    --mvm-gw-dest-ip=169.254.68.5 \
    --mvm-gw-mac-addr=20:90:6f:cf:cf:cf \
    --mvm-mask=30 \
    --mvm-mtu=1500 \
    --state-dir=/var/lib/network-agent/state \
    --host-proxy-bind-ip=127.0.0.1 \
    --tap-init-num=10
```

### 使用 Cubelet 配置文件

```bash
./network-agent \
    --cubelet-config=/etc/cubelet/config.toml \
    --eth-name=eth0
```

---

## Cubelet TOML 配置

当使用 `--cubelet-config` 指定配置文件时，Network Agent 会从中读取相关配置。

### 配置文件结构

```toml
[network]
cidr = "192.168.0.0/18"

[network.proxy]
enabled = true
bind_ip = "127.0.0.1"
connection_timeout = "30s"

[network.mvm]
inner_ip = "169.254.68.6"
mac_addr = "20:90:6f:fc:fc:fc"
gw_dest_ip = "169.254.68.5"
gw_mac_addr = "20:90:6f:cf:cf:cf"
mask = 30
mtu = 1500

[network.tap]
init_num = 10

[network.state]
dir = "/var/lib/network-agent/state"

[network.exposed_ports]
ports = [22, 80, 443]
```

### 配置项说明

#### [network] 段

| 配置项 | 类型 | 描述 |
|--------|------|------|
| `cidr` | string | TAP 沙箱 IP 地址池 (CIDR 格式) |

#### [network.proxy] 段

| 配置项 | 类型 | 描述 |
|--------|------|------|
| `enabled` | bool | 是否启用 Host Proxy |
| `bind_ip` | string | Host Proxy 绑定 IP |
| `connection_timeout` | duration | 连接超时时间 |

#### [network.mvm] 段

| 配置项 | 类型 | 描述 |
|--------|------|------|
| `inner_ip` | string | Guest 内部可见 IP |
| `mac_addr` | string | Guest MAC 地址 |
| `gw_dest_ip` | string | Guest 网关 IP |
| `gw_mac_addr` | string | Gateway MAC 地址 |
| `mask` | int | 网络掩码位数 |
| `mtu` | int | MTU 大小 |

#### [network.tap] 段

| 配置项 | 类型 | 描述 |
|--------|------|------|
| `init_num` | int | 预分配的 TAP 设备数量 |

#### [network.state] 段

| 配置项 | 类型 | 描述 |
|--------|------|------|
| `dir` | string | 状态持久化目录 |

#### [network.exposed_ports] 段

| 配置项 | 类型 | 描述 |
|--------|------|------|
| `ports` | []int | 默认暴露的容器端口列表 |

---

## 配置详解

### 网络地址池 (CIDR)

`cidr` 参数定义了可分配给 TAP 设备的 IP 地址池。

**默认值**: `192.168.0.0/18`

**可用地址计算**:
- `/18` 网段包含 16,384 个地址
- 减去网络地址 (第一个)
- 减去网关地址 (第二个)
- 减去广播地址 (最后一个)
- **可用地址**: 16,381 个

**选择建议**:
- 根据节点预期的沙箱数量选择合适的网段大小
- 确保与宿主机其他网段不冲突

### TAP 设备池

`tap-init-num` 参数控制启动时预分配的 TAP 设备数量。

**默认值**: `0` (不预分配)

**作用**:
- 减少首次创建网络的延迟
- 预分配的 TAP 设备可立即使用

**权衡**:
- 增加内存和文件描述符占用
- 建议根据实际负载调整

### Guest 网络配置

所有沙箱内的 Guest 看到的网络配置相同:

```
┌─────────────────────────────────────┐
│            Guest (micro-VM)          │
│                                      │
│  eth0: 169.254.68.6/30              │
│  gateway: 169.254.68.5              │
│  MAC: 20:90:6f:fc:fc:fc             │
│  MTU: 1500                           │
└─────────────────────────────────────┘
```

**说明**:
- `mvm-inner-ip`: Guest 看到的自己的 IP
- `mvm-gw-dest-ip`: Guest 的默认网关
- `mvm-mask`: 网络掩码位数 (30 = /30 = 4 个地址)
- `mvm-mtu`: Guest 网络接口 MTU，默认 1500

### 状态持久化目录

`state-dir` 参数指定状态文件存储位置。

**默认值**: `/usr/local/services/cubetoolbox/network-agent/state`

**目录结构**:
```
{state-dir}/
├── sandbox-001.json
├── sandbox-002.json
└── ...
```

**注意事项**:
- 确保目录存在且有写权限
- 建议使用持久化存储
- 避免使用 tmpfs (重启后丢失)

### Host Proxy 绑定

`host-proxy-bind-ip` 控制端口映射的绑定地址。

**默认值**: `127.0.0.1`

**选项**:
- `127.0.0.1`: 仅本地访问 (更安全)
- `0.0.0.0`: 允许外部访问
- 特定 IP: 绑定到指定网卡

---

## 环境变量

Network Agent 目前不支持环境变量配置。所有配置必须通过命令行参数或 TOML 文件提供。

---

## 配置验证

启动时，Network Agent 会验证以下配置:

1. **eth-name**: 必须存在且是有效的网络接口
2. **cidr**: 必须是有效的 CIDR 格式
3. **state-dir**: 必须是可写目录
4. **监听地址**: 必须是有效的 Unix Socket 或 TCP 地址

**验证失败时，服务会拒绝启动并输出错误信息。**

---

## 运行时配置

以下配置可以在运行时通过 API 调整:

| 配置项 | API | 说明 |
|--------|-----|------|
| 端口映射 | ReconcileNetwork | 动态添加/删除端口映射 |

其他配置需要重启服务才能生效。

---

## 生产环境建议

### 推荐配置

```bash
./network-agent \
    --listen=unix:///var/run/network-agent/api.sock \
    --grpc-listen=unix:///var/run/network-agent/grpc.sock \
    --health-listen=127.0.0.1:19090 \
    --tap-fd-listen=unix:///var/run/network-agent/tap.sock \
    --eth-name=eth0 \
    --cidr=192.168.0.0/16 \
    --state-dir=/var/lib/network-agent/state \
    --tap-init-num=20 \
    --mvm-mtu=1500
```

### 性能调优

1. **TAP 池大小**: 根据预期并发设置 `--tap-init-num`
2. **CIDR 大小**: 预留足够的 IP 地址空间
3. **State 目录**: 使用 SSD 存储

### 安全建议

1. **Unix Socket 权限**: 限制 socket 文件的访问权限
2. **Host Proxy**: 绑定到 127.0.0.1 限制访问
3. **State 目录**: 限制目录的读写权限

---

## 故障排查

### 常见配置错误

#### 1. eth-name 不存在

```
Error: network interface "eth0" not found
```

**解决**: 确认接口名称，可使用 `ip link` 查看。

#### 2. CIDR 格式错误

```
Error: invalid CIDR: "192.168.0.0"
```

**解决**: 使用正确的 CIDR 格式，如 `192.168.0.0/18`。

#### 3. State 目录不可写

```
Error: state directory not writable: "/var/lib/network-agent/state"
```

**解决**: 创建目录并设置正确权限。

```bash
mkdir -p /var/lib/network-agent/state
chmod 755 /var/lib/network-agent/state
```

#### 4. Socket 地址冲突

```
Error: bind: address already in use
```

**解决**: 检查是否有其他进程占用该地址，或更换监听地址。
