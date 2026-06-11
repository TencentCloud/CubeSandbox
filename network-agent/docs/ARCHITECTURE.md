# Network Agent 架构设计文档

## 项目概述

`network-agent` 是 Cube 基础设施项目的节点级网络编排组件，负责管理 micro-VM 沙箱的本地网络配置。该组件从 Cubelet 中迁移出网络编排功能，作为独立服务运行，并复用 `cubevs` 的本地网络执行能力。

### 核心职责

- 本地 TAP 设备创建和 `cubevs` 映射管理
- 主机端口代理到 Guest 服务
- 本地状态持久化和启动恢复
- 向 Cubelet 提供独立的网络管理接口

### 设计原则

1. **独立性**: 作为独立服务运行，通过 gRPC/HTTP 与 Cubelet 通信
2. **幂等性**: 所有操作都是幂等的，支持重复调用
3. **持久性**: 状态持久化到磁盘，支持故障恢复
4. **高效性**: TAP 设备池化，减少创建延迟

---

## 系统架构

### 整体架构图

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                              客户端 (Clients)                                 │
│                         Cubelet / 外部服务                                    │
└───────────────────────────┬─────────────────────────────────────────────────┘
                            │
         ┌──────────────────┼──────────────────┐
         │                  │                  │
         ▼                  ▼                  ▼
┌─────────────────┐ ┌─────────────────┐ ┌─────────────────┐
│   HTTP Server   │ │   gRPC Server   │ │   FD Server     │
│   (REST API)    │ │   (Protobuf)    │ │   (Unix Socket) │
│                 │ │                 │ │                 │
│ - /v1/network/* │ │ - NetworkAgent  │ │ - TAP FD 传递   │
│ - /healthz      │ │   Service       │ │                 │
│ - /readyz       │ │ - Health Check  │ │                 │
└────────┬────────┘ └────────┬────────┘ └────────┬────────┘
         │                   │                   │
         └───────────────────┼───────────────────┘
                             │
                             ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                           Service Layer                                      │
│                          (LocalService)                                      │
│                                                                              │
│  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐        │
│  │   IPAM      │  │  Port       │  │   TAP       │  │  Host       │        │
│  │  Allocator  │  │  Allocator  │  │   Pool      │  │  Proxy      │        │
│  └─────────────┘  └─────────────┘  └─────────────┘  └─────────────┘        │
└────────────────────────────┬────────────────────────────────────────────────┘
                             │
         ┌───────────────────┼───────────────────┐
         │                   │                   │
         ▼                   ▼                   ▼
┌─────────────────┐ ┌─────────────────┐ ┌─────────────────┐
│   CubeVS        │ │   Netlink       │ │   State Store   │
│   (eBPF)        │ │   (Linux)       │ │   (JSON Files)  │
│                 │ │                 │ │                 │
│ - TAP 注册      │ │ - 设备管理      │ │ - 持久化状态    │
│ - 端口映射      │ │ - 路由管理      │ │ - 故障恢复      │
│ - 包处理        │ │ - ARP 管理      │ │                 │
└─────────────────┘ └─────────────────┘ └─────────────────┘
```

### 分层架构

项目采用清晰的分层架构设计:

| 层级 | 目录 | 职责 |
|------|------|------|
| API 层 | `api/v1/` | Protobuf 定义、gRPC 服务接口 |
| 传输层 | `internal/httpserver/`, `internal/grpcserver/`, `internal/fdserver/` | HTTP/gRPC/FD 服务器实现 |
| 服务层 | `internal/service/` | 核心业务逻辑 |
| 入口点 | `cmd/network-agent/` | 程序入口、配置初始化 |

---

## 核心模块设计

### 1. Service 模块 (`internal/service/`)

#### 接口定义 (`service.go`)

```go
type Service interface {
    EnsureNetwork(ctx context.Context, req *EnsureNetworkRequest) (*EnsureNetworkResponse, error)
    ReleaseNetwork(ctx context.Context, req *ReleaseNetworkRequest) (*ReleaseNetworkResponse, error)
    ReconcileNetwork(ctx context.Context, req *ReconcileNetworkRequest) (*ReconcileNetworkResponse, error)
    GetNetwork(ctx context.Context, req *GetNetworkRequest) (*GetNetworkResponse, error)
    Health(ctx context.Context) error
}
```

#### 核心实现 (`local_service.go`)

`LocalService` 是服务层的核心实现，负责:

- **状态管理**: 维护每个沙箱的网络状态
- **TAP 生命周期**: 创建、池化、回收 TAP 设备
- **CubeVS 集成**: 通过 eBPF 进行包处理和端口映射
- **持久化**: 状态写入磁盘，支持恢复

**关键数据结构:**

```go
type localService struct {
    cfg       Config              // 配置
    store     *stateStore         // 持久化存储
    allocator *ipAllocator        // IP 分配器
    ports     *portAllocator      // 端口分配器
    device    *machineDevice      // 节点上行接口
    cubeDev   *cubeDev            // 虚拟网关设备

    mu                sync.Mutex
    states            map[string]*managedState  // 活跃沙箱状态
    tapPool           []*tapDevice              // 空闲 TAP 池
    abnormalTapPool   []*tapDevice              // 待清理 TAP
    destroyFailedTaps map[string]*tapDevice     // 销毁失败的 TAP
}
```

### 2. IPAM 模块 (`ipam.go`)

基于位图的 IP 地址分配器:

```
CIDR: 192.168.0.0/18 (16382 可用地址)

位图索引:
┌─────┬─────┬─────┬─────┬─────────┬─────┐
│  0  │  1  │  2  │  3  │   ...   │  N  │
├─────┼─────┼─────┼─────┼─────────┼─────┤
│网络 │网关 │可用 │可用 │   ...   │广播 │
│保留 │保留 │     │     │         │保留 │
└─────┴─────┴─────┴─────┴─────────┴─────┘
```

**特性:**
- 线程安全 (mutex 保护)
- 索引 0 保留为网络地址
- 索引 1 保留为网关地址
- 最后一个索引保留为广播地址

### 3. TAP 设备池 (`tap_lifecycle.go`)

预分配的 TAP 设备池，减少网络创建延迟:

```
TAP 池状态流转:

创建 ─────────────────────────────────────────────────────┐
  │                                                       │
  ▼                                                       │
┌─────────────┐    dequeue    ┌─────────────┐            │
│  TAP Pool   │ ─────────────▶│   In Use    │            │
│  (空闲池)   │               │  (使用中)   │            │
└─────────────┘               └─────────────┘            │
  ▲                                 │                    │
  │           enqueue               │ release            │
  └─────────────────────────────────┘                    │
                                    │                    │
                                    │ 异常               │
                                    ▼                    │
                            ┌─────────────┐             │
                            │  Abnormal   │──清理──────▶│
                            │  (异常池)   │             销毁
                            └─────────────┘
```

**配置参数:**
- `TapInitNum`: 预分配的 TAP 数量 (默认: 0)

### 4. 端口分配器 (`port_allocator.go`)

主机端口分配，用于端口映射:

- 分配范围: `ip_local_port_range` 上限到 65535
- 尊重 `/proc/sys/net/ipv4/ip_local_reserved_ports`
- 线程安全

### 5. 状态存储 (`state_store.go`)

JSON 文件存储，每个沙箱一个文件:

```
{StateDir}/
├── sandbox-001.json
├── sandbox-002.json
└── sandbox-003.json
```

**存储内容:**
```json
{
    "sandboxID": "sandbox-001",
    "networkHandle": "handle-001",
    "tapName": "z192.168.0.10",
    "tapIfIndex": 42,
    "sandboxIP": "192.168.0.10",
    "interfaces": [...],
    "routes": [...],
    "arpNeighbors": [...],
    "portMappings": [...],
    "cubeNetworkConfig": {...}
}
```

### 6. Host Proxy (`hostproxy.go`)

用户空间 TCP 代理，实现主机端口到 Guest 端口的转发:

```
Host:8080 ◀────────▶ TAP (192.168.0.10) ◀────────▶ Guest:80
           Host Proxy                    eBPF 处理
```

**实现细节:**
- 使用 `SO_BINDTODEVICE` 绑定到特定 TAP
- 双向流量转发
- 连接超时控制

---

## 服务器层设计

### 1. HTTP Server (`internal/httpserver/`)

REST API 服务器:

| 端点 | 方法 | 描述 |
|------|------|------|
| `/healthz` | GET | 存活探针 |
| `/readyz` | GET | 就绪探针 |
| `/v1/network/ensure` | POST | 创建/确保网络 |
| `/v1/network/release` | POST | 释放网络 |
| `/v1/network/reconcile` | POST | 协调网络状态 |
| `/v1/network/get` | POST | 获取网络状态 |

**监听方式:**
- Unix Socket: `unix:///tmp/cube/network-agent.sock`
- TCP: `tcp://127.0.0.1:8080`

### 2. gRPC Server (`internal/grpcserver/`)

Protobuf 定义的 gRPC 服务:

```protobuf
service NetworkAgent {
    rpc EnsureNetwork(EnsureNetworkRequest) returns (EnsureNetworkResponse);
    rpc ReleaseNetwork(ReleaseNetworkRequest) returns (ReleaseNetworkResponse);
    rpc ReconcileNetwork(ReconcileNetworkRequest) returns (ReconcileNetworkResponse);
    rpc GetNetwork(GetNetworkRequest) returns (GetNetworkResponse);
    rpc Health(HealthRequest) returns (HealthResponse);
}
```

### 3. FD Server (`internal/fdserver/`)

Unix Socket TAP 文件描述符服务器:

- 使用 `SCM_RIGHTS` 传递 FD
- JSON 请求协议
- 用于 Cubelet 获取 TAP FD

---

## 核心流程

### EnsureNetwork 流程

```
┌────────────────┐
│ 收到请求        │
└───────┬────────┘
        │
        ▼
┌────────────────┐     是     ┌────────────────┐
│ 沙箱已存在?     │────────────▶│ 返回现有状态    │
└───────┬────────┘             └────────────────┘
        │ 否
        ▼
┌────────────────┐     有     ┌────────────────┐
│ TAP 池有可用?   │────────────▶│ 从池中获取 TAP  │
└───────┬────────┘             └───────┬────────┘
        │ 无                           │
        ▼                              │
┌────────────────┐                     │
│ 分配 IP         │                     │
│ 创建新 TAP      │                     │
└───────┬────────┘                     │
        │                              │
        ▼◀─────────────────────────────┘
┌────────────────┐
│ 配置端口映射     │
│ (cubevs eBPF)  │
└───────┬────────┘
        │
        ▼
┌────────────────┐
│ 注册到 cubevs   │
│ eBPF maps      │
└───────┬────────┘
        │
        ▼
┌────────────────┐
│ 持久化状态      │
└───────┬────────┘
        │
        ▼
┌────────────────┐
│ 返回网络配置    │
└────────────────┘
```

### ReleaseNetwork 流程

```
┌────────────────┐
│ 收到请求        │
└───────┬────────┘
        │
        ▼
┌────────────────┐     否     ┌────────────────┐
│ 沙箱存在?       │────────────▶│ 返回成功        │
└───────┬────────┘             └────────────────┘
        │ 是
        ▼
┌────────────────┐
│ 关闭 Host Proxy │
└───────┬────────┘
        │
        ▼
┌────────────────┐
│ 清除端口映射     │
└───────┬────────┘
        │
        ▼
┌────────────────┐
│ 从 cubevs 移除  │
└───────┬────────┘
        │
        ▼
┌────────────────┐
│ TAP 回收到池    │
└───────┬────────┘
        │
        ▼
┌────────────────┐
│ 删除持久化状态  │
└───────┬────────┘
        │
        ▼
┌────────────────┐
│ 返回成功        │
└────────────────┘
```

### 启动恢复流程

```
┌────────────────────────┐
│     服务启动            │
└───────────┬────────────┘
            │
            ▼
┌────────────────────────┐
│ 加载磁盘持久化状态       │
└───────────┬────────────┘
            │
            ▼
┌────────────────────────┐
│ 列出系统现有 TAP 设备    │
└───────────┬────────────┘
            │
            ▼
┌────────────────────────┐
│ 列出 cubevs eBPF 条目   │
└───────────┬────────────┘
            │
            ▼
┌────────────────────────────────────────────────────┐
│              遍历每个 TAP 设备                       │
│  ┌─────────────────────────────────────────────┐  │
│  │ 有匹配的持久化状态? ──是──▶ 恢复 managedState  │  │
│  │         │                                   │  │
│  │         │ 否                                │  │
│  │         ▼                                   │  │
│  │ cubevs 中标记 InUse? ──是──▶ 从 TAP 重建状态  │  │
│  │         │                                   │  │
│  │         │ 否                                │  │
│  │         ▼                                   │  │
│  │  清理 cubevs 条目, 加入空闲池                  │  │
│  └─────────────────────────────────────────────┘  │
└────────────────────────┬───────────────────────────┘
                         │
                         ▼
┌────────────────────────────────────────────────────┐
│ 清理过期持久化状态 (TAP 已不存在)                     │
└───────────┬────────────────────────────────────────┘
            │
            ▼
┌────────────────────────┐
│ 预分配 TAP 到池         │
│ (补齐到 TapInitNum)    │
└───────────┬────────────┘
            │
            ▼
┌────────────────────────┐
│ 启动维护循环            │
└────────────────────────┘
```

---

## 依赖关系

### 内部依赖

```
network-agent
    │
    └──▶ cubevs (current private module path)
            │
            ├── eBPF TAP 设备管理
            ├── eBPF 端口映射
            └── eBPF 包处理过滤器
```

### 外部依赖

| 依赖 | 版本 | 用途 |
|------|------|------|
| `github.com/cilium/ebpf` | v0.17.3 | eBPF map 操作 |
| `github.com/vishvananda/netlink` | v1.1.0 | Linux netlink 接口 |
| `golang.org/x/sys` | latest | 底层系统调用 |
| `google.golang.org/grpc` | v1.79.2 | gRPC 框架 |
| `google.golang.org/protobuf` | v1.36.10 | Protocol Buffers |
| `github.com/pelletier/go-toml/v2` | v2.2.4 | TOML 配置解析 |

---

## 网络模型

### TAP 设备命名

TAP 设备命名格式: `z{IP地址}` (例: `z192.168.0.10`)

### CubeDev (网关设备)

名为 `cube-dev` 的 Linux dummy 接口，作为所有沙箱流量的虚拟网关。

### eBPF 集成

`cubevs` 库管理:
- TAP 设备注册到 eBPF maps
- 端口映射到 eBPF maps
- 附加到 TAP ingress 的包处理过滤器

### 网络拓扑

```
                                    ┌─────────────────┐
                                    │   外部网络       │
                                    └────────┬────────┘
                                             │
                                    ┌────────┴────────┐
                                    │    eth0         │
                                    │  (上行接口)      │
                                    └────────┬────────┘
                                             │
                              ┌──────────────┼──────────────┐
                              │              │              │
                     ┌────────┴────────┐     │     ┌────────┴────────┐
                     │   cube-dev      │     │     │   cubevs eBPF   │
                     │  (虚拟网关)      │     │     │   (包处理)       │
                     │ 169.254.68.5    │     │     └────────┬────────┘
                     └────────┬────────┘     │              │
                              │              │              │
         ┌────────────────────┼──────────────┼──────────────┤
         │                    │              │              │
┌────────┴────────┐  ┌────────┴────────┐     │     ┌────────┴────────┐
│  z192.168.0.10  │  │  z192.168.0.11  │    ...    │  z192.168.0.N   │
│     (TAP)       │  │     (TAP)       │           │     (TAP)       │
└────────┬────────┘  └────────┬────────┘           └────────┬────────┘
         │                    │                             │
┌────────┴────────┐  ┌────────┴────────┐           ┌────────┴────────┐
│   Sandbox 1     │  │   Sandbox 2     │    ...    │   Sandbox N     │
│ (micro-VM)      │  │ (micro-VM)      │           │ (micro-VM)      │
│ 169.254.68.6    │  │ 169.254.68.6    │           │ 169.254.68.6    │
└─────────────────┘  └─────────────────┘           └─────────────────┘
```

---

## 设计决策

### 1. 为什么使用 TAP 设备池?

**问题**: TAP 设备创建涉及多个系统调用，延迟较高。

**解决方案**: 预分配 TAP 设备池，按需分配，用完回收。

**权衡**: 占用额外内存和文件描述符，但显著减少网络创建延迟。

### 2. 为什么需要 FD Server?

**问题**: Cubelet 需要 TAP 文件描述符来配置 micro-VM 网络。

**解决方案**: 通过 Unix Socket 使用 `SCM_RIGHTS` 传递 FD。

**原因**: 文件描述符不能通过普通 RPC 传递，必须使用内核的 FD 传递机制。

### 3. 为什么同时支持 HTTP 和 gRPC?

**HTTP**: 简单调试、与不支持 gRPC 的组件集成。

**gRPC**: 高性能、类型安全、与 Cubelet 的首选通信方式。

### 4. 为什么状态需要持久化?

**问题**: 服务重启后需要恢复网络状态。

**解决方案**: 每个沙箱状态以 JSON 文件存储。

**恢复策略**: 启动时扫描磁盘状态、系统 TAP 设备、cubevs eBPF maps，三者对比恢复。

---

## 未实现功能 (MVP 阶段)

根据 README 说明，以下功能在 MVP 阶段暂不实现:

1. **VPC/ENI 分配**: 不涉及外部 IP 分配
2. **networkd 守护进程**: 不运行独立的网络守护进程
3. **CubeGW**: 不使用 CubeGW 网关
4. **隧道组**: 不支持隧道组功能

---

## 相关文档

- [API 参考](./API.md) - 详细的 API 接口文档
- [配置指南](./CONFIGURATION.md) - 配置选项说明
- [开发指南](./DEVELOPMENT.md) - 构建、测试、开发流程
