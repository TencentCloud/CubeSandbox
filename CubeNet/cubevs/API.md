# CubeVS API 文档

## 项目概述

**CubeVS** 是一个 Go 语言库，用于管理虚拟机网络子系统。这是一个**纯 Go 库**，不提供 HTTP、REST 或 gRPC 服务接口，而是通过公开的 Go 函数供其他应用程序使用。

## 公共 API 列表

### 1. TAP 设备管理 (tap.go)

TAP 设备是虚拟网络接口，用于虚拟机网络通信。

| 函数 | 签名 | 说明 | 行号 |
|------|------|------|------|
| `ListTAPDevices()` | `func ListTAPDevices() ([]TAPDevice, error)` | 列出所有 TAP 设备 | 13 |
| `AddTAPDevice()` | `func AddTAPDevice(ifindex uint32, ip net.IP, id string, version uint32, _ MVMOptions) error` | 添加新 TAP 设备 | 40 |
| `DelTAPDevice()` | `func DelTAPDevice(ifindex uint32, ip net.IP) error` | 删除 TAP 设备 | 80 |
| `GetTAPDevice()` | `func GetTAPDevice(ifindex uint32) (*TAPDevice, error)` | 根据 ifindex 获取 TAP 设备信息 | 111 |

### 2. 端口映射 (port.go)

管理主机和虚拟机之间的端口映射。

| 函数 | 签名 | 说明 | 行号 |
|------|------|------|------|
| `AddPortMapping()` | `func AddPortMapping(ifindex uint32, listenPort uint16, hostPort uint16) error` | 添加端口映射 | 11 |
| `DelPortMapping()` | `func DelPortMapping(ifindex uint32, listenPort uint16, hostPort uint16) error` | 删除端口映射 | 49 |
| `ListPortMapping()` | `func ListPortMapping() (map[uint16]MVMPort, error)` | 列出所有端口映射 | 81 |
| `GetPortMapping()` | `func GetPortMapping(ifindex uint32, listenPort uint16) (uint16, error)` | 获取指定端口的主机端口 | 128 |

### 3. SNAT 源地址转换 (snat.go)

配置网络地址转换中使用的 IP 地址。

| 函数 | 签名 | 说明 | 行号 |
|------|------|------|------|
| `SetSNATIPs()` | `func SetSNATIPs(ips []*SNATIP) error` | 设置 SNAT 所使用的 IP 列表 | 35 |

### 4. 会话管理 (reaper.go)

管理和追踪网络会话生命周期。

| 函数 | 签名 | 说明 | 行号 |
|------|------|------|------|
| `StartSessionReaper()` | `func StartSessionReaper() <-chan Event` | 启动会话清理 goroutine，周期性移除过期会话 | 160 |

### 5. BPF 过滤器与初始化

| 函数 | 签名 | 说明 | 位置 |
|------|------|------|------|
| `Init()` | `func Init(params Params) error` | 使用指定参数初始化 CubeVS 库 | miscs.go:116 |
| `AttachFilter()` | `func AttachFilter(ifindex uint32) error` | 将 BPF TC 过滤器挂接到 TAP 设备入站方向 | reaper.go:156 |

## 数据结构

### 公开数据结构

| 类型 | 位置 | 说明 |
|------|------|------|
| `TAPDevice` | cubevs.go:35 | TAP 设备信息 (IP、ID、Ifindex) |
| `Params` | cubevs.go:17 | 初始化参数配置 |
| `MVMPort` | cubevs.go:63 | 端口映射结构体 |
| `SNATIP` | snat.go:21 | SNAT IP 信息 |
| `SessionKey` | reaper.go:103 | 会话键 (用于出站追踪) |
| `NATSession` | reaper.go:113 | NAT 会话数据 |
| `IngressSessionValue` | reaper.go:144 | 入站会话追踪数据 |
| `Event` | reaper.go:98 | 事件通知结构体 |
| `TCDirection` | cubevs.go:52 | TC 过滤器方向 (入站/出站) |

### 内部数据结构

| 类型 | 位置 | 说明 |
|------|------|------|
| `mvmMetadata` | cubevs.go:44 | BPF map 值 - MVM 元数据 |
| `snatIP` | snat.go:26 | BPF map 值 - SNAT IP |
| `tcpConntrackState` | reaper.go:25 | TCP 连接追踪状态枚举 |

## eBPF 集成

### BPF Map 名称

| Map 名称 | 常量 | 位置 | 说明 |
|---------|------|------|------|
| `ifindex_to_mvmmeta` | `MapNameIfindexToMVMMetadata` | cubevs.go:78 | 映射 ifindex 到 MVM 元数据 |
| `mvmip_to_ifindex` | `MapNameMVMIPToIfindex` | cubevs.go:79 | 映射 MVM IP 到 ifindex |
| `remote_port_mapping` | `MapNameRemotePortMapping` | cubevs.go:80 | 主机端口到虚拟机映射 |
| `local_port_mapping` | `MapNameLocalPortMapping` | cubevs.go:81 | 虚拟机端口到主机映射 |
| `ingress_sessions` | `MapNameIngressSessions` | reaper.go:15 | 入站会话追踪 |
| `egress_sessions` | `MapNameEgressSessions` | reaper.go:16 | 出站会话追踪 |
| `snat_iplist` | (定义在 snat.go) | snat.go:12 | SNAT IP 列表 |

### BPF 程序

| 程序名称 | 常量 | 位置 | 说明 |
|---------|------|------|------|
| `from_envoy` | `programNameFromEnvoy` | cubevs.go:78 | cubegw0 上的 TC 出站过滤器 |
| `from_cube` | `programNameFromCube` | cubevs.go:79 | TAP 设备上的 TC 入站过滤器 |
| `from_world` | `programNameFromWorld` | cubevs.go:80 | eth0 上的 TC 入站过滤器 |

## 项目结构

```
/data/netdev/mvs/cubevs/
├── cubevs.go           - 核心库定义、类型、常量
├── tap.go              - TAP 设备管理 (4 个公开 API)
├── port.go             - 端口映射函数 (4 个公开 API)
├── snat.go             - SNAT IP 配置 (1 个公开 API)
├── reaper.go           - 会话管理和清理 (1 个公开 API)
├── tc.go               - TC 过滤器挂接 (内部辅助函数)
├── miscs.go            - 杂项函数 (Init() 和 AttachFilter())
├── map.go              - BPF map 加载工具
├── util.go             - 辅助函数 (IP 转换、字节操作)
├── go.mod              - Go 模块定义
├── go.sum              - Go 模块校验和
└── [BPF 对象文件]      - 编译的 eBPF 程序
```

## API 总结

**CubeVS** 提供以下功能：

- ✅ **TAP 设备管理** - 创建、删除和配置虚拟 TAP 设备
- ✅ **端口映射** - 管理主机和虚拟机之间的端口转发
- ✅ **SNAT 配置** - 配置源地址转换 IP 池
- ✅ **会话追踪** - 追踪和管理网络会话的生命周期
- ✅ **eBPF 集成** - 使用 BPF 进行高效的数据包处理和过滤
- ✅ **初始化接口** - 提供统一的初始化入口点

**总计：11 个公开 Go API 函数**

## 使用示例

### 初始化

```go
params := Params{
    // 配置参数
}
err := Init(params)
if err != nil {
    log.Fatal(err)
}
```

### 添加 TAP 设备

```go
ip := net.ParseIP("10.0.0.2")
err := AddTAPDevice(ifindex, ip, "vm-id-001", 1, MVMOptions{})
if err != nil {
    log.Fatal(err)
}
```

### 配置端口映射

```go
err := AddPortMapping(ifindex, 8080, 8080) // 将虚拟机端口 8080 映射到主机端口 8080
if err != nil {
    log.Fatal(err)
}
```

### 启动会话清理

```go
eventChan := StartSessionReaper()
for event := range eventChan {
    // 处理会话事件
}
```
