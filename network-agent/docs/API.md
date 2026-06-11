# Network Agent API 参考文档

本文档详细描述 Network Agent 提供的所有 API 接口。

---

## 概述

Network Agent 提供三种接口:

| 接口类型 | 默认地址 | 用途 |
|----------|----------|------|
| HTTP REST | `unix:///tmp/cube/network-agent.sock` | REST API、健康检查 |
| gRPC | `unix:///tmp/cube/network-agent-grpc.sock` | 高性能 RPC |
| FD Server | `unix:///tmp/cube/network-agent-tap.sock` | TAP 文件描述符传递 |

---

## 数据类型定义

### Interface (网络接口)

```json
{
    "name": "eth0",
    "macAddress": "20:90:6f:fc:fc:fc",
    "ipCIDRs": ["169.254.68.6/30"],
    "gateway": "169.254.68.5",
    "mtu": 1500
}
```

| 字段 | 类型 | 描述 |
|------|------|------|
| `name` | string | 接口名称 |
| `macAddress` | string | MAC 地址 |
| `ipCIDRs` | []string | IP 地址列表 (CIDR 格式) |
| `gateway` | string | 网关地址 |
| `mtu` | int32 | MTU 大小 |

### Route (路由)

```json
{
    "destinationCIDR": "0.0.0.0/0",
    "gateway": "169.254.68.5",
    "device": "eth0"
}
```

| 字段 | 类型 | 描述 |
|------|------|------|
| `destinationCIDR` | string | 目标网段 (CIDR 格式) |
| `gateway` | string | 网关地址 |
| `device` | string | 出接口名称 |

### ARPNeighbor (ARP 邻居)

```json
{
    "ip": "169.254.68.5",
    "macAddress": "20:90:6f:cf:cf:cf",
    "device": "eth0"
}
```

| 字段 | 类型 | 描述 |
|------|------|------|
| `ip` | string | IP 地址 |
| `macAddress` | string | MAC 地址 |
| `device` | string | 接口名称 |

### PortMapping (端口映射)

```json
{
    "protocol": "tcp",
    "containerPort": 80,
    "hostPort": 30080,
    "hostIP": "127.0.0.1"
}
```

| 字段 | 类型 | 描述 |
|------|------|------|
| `protocol` | string | 协议 (tcp/udp) |
| `containerPort` | uint16 | 容器端口 |
| `hostPort` | uint16 | 主机端口 |
| `hostIP` | string | 主机绑定 IP |

### CubeNetworkConfig (网络配置)

```json
{
    "tunnelGroup": 0,
    "appID": 12345,
    "businessType": "normal",
    "networkPolicies": [...],
    "subENI": {...}
}
```

| 字段 | 类型 | 描述 |
|------|------|------|
| `tunnelGroup` | uint32 | 隧道组 ID |
| `appID` | uint32 | 应用 ID |
| `businessType` | string | 业务类型 |
| `networkPolicies` | []NetworkPolicy | 网络策略列表 |
| `subENI` | SubENIInfo | 子 ENI 信息 |

### SubENIInfo (子 ENI 信息)

```json
{
    "ifName": "eth1",
    "ipCIDR": "10.0.0.5/24",
    "gateway": "10.0.0.1",
    "macAddress": "fa:16:3e:xx:xx:xx"
}
```

---

## HTTP REST API

### 健康检查

#### GET /healthz

存活探针，用于 Kubernetes liveness probe。

**响应:**
- `200 OK`: `ok`

#### GET /readyz

就绪探针，用于 Kubernetes readiness probe。

**响应:**
- `200 OK`: `ready`

---

### 网络管理

#### POST /v1/network/ensure

创建或确保网络配置存在。操作是幂等的。

**请求体:**

```json
{
    "sandboxID": "sandbox-001",
    "networkHandle": "handle-001",
    "portMappings": [
        {
            "protocol": "tcp",
            "containerPort": 80
        }
    ],
    "cubeNetworkConfig": {
        "appID": 12345,
        "businessType": "normal"
    }
}
```

| 字段 | 类型 | 必填 | 描述 |
|------|------|------|------|
| `sandboxID` | string | 是 | 沙箱唯一标识 |
| `networkHandle` | string | 否 | 网络句柄 |
| `portMappings` | []PortMapping | 否 | 端口映射列表 |
| `cubeNetworkConfig` | CubeNetworkConfig | 否 | 网络配置 |

**响应:**

```json
{
    "tapName": "z192.168.0.10",
    "tapIfIndex": 42,
    "sandboxIP": "192.168.0.10",
    "interfaces": [...],
    "routes": [...],
    "arpNeighbors": [...],
    "portMappings": [...]
}
```

| 字段 | 类型 | 描述 |
|------|------|------|
| `tapName` | string | TAP 设备名称 |
| `tapIfIndex` | int | TAP 设备索引 |
| `sandboxIP` | string | 分配的沙箱 IP |
| `interfaces` | []Interface | 网络接口配置 |
| `routes` | []Route | 路由配置 |
| `arpNeighbors` | []ARPNeighbor | ARP 配置 |
| `portMappings` | []PortMapping | 完整的端口映射 (包含分配的 hostPort) |

**错误:**
- `400 Bad Request`: 请求体格式错误
- `500 Internal Server Error`: 服务内部错误

---

#### POST /v1/network/release

释放网络资源。操作是幂等的。

**请求体:**

```json
{
    "sandboxID": "sandbox-001"
}
```

| 字段 | 类型 | 必填 | 描述 |
|------|------|------|------|
| `sandboxID` | string | 是 | 沙箱唯一标识 |

**响应:**

```json
{
    "ok": true
}
```

**错误:**
- `400 Bad Request`: 请求体格式错误
- `500 Internal Server Error`: 服务内部错误

---

#### POST /v1/network/reconcile

协调网络状态，更新端口映射等配置。

**请求体:**

```json
{
    "sandboxID": "sandbox-001",
    "portMappings": [
        {
            "protocol": "tcp",
            "containerPort": 80
        },
        {
            "protocol": "tcp",
            "containerPort": 443
        }
    ]
}
```

| 字段 | 类型 | 必填 | 描述 |
|------|------|------|------|
| `sandboxID` | string | 是 | 沙箱唯一标识 |
| `portMappings` | []PortMapping | 否 | 新的端口映射列表 |

**响应:**

```json
{
    "portMappings": [
        {
            "protocol": "tcp",
            "containerPort": 80,
            "hostPort": 30080,
            "hostIP": "127.0.0.1"
        },
        {
            "protocol": "tcp",
            "containerPort": 443,
            "hostPort": 30443,
            "hostIP": "127.0.0.1"
        }
    ]
}
```

**错误:**
- `400 Bad Request`: 请求体格式错误
- `404 Not Found`: 沙箱不存在
- `500 Internal Server Error`: 服务内部错误

---

#### POST /v1/network/get

获取网络状态。

**请求体:**

```json
{
    "sandboxID": "sandbox-001"
}
```

| 字段 | 类型 | 必填 | 描述 |
|------|------|------|------|
| `sandboxID` | string | 是 | 沙箱唯一标识 |

**响应:**

```json
{
    "tapName": "z192.168.0.10",
    "tapIfIndex": 42,
    "sandboxIP": "192.168.0.10",
    "interfaces": [...],
    "routes": [...],
    "arpNeighbors": [...],
    "portMappings": [...]
}
```

**错误:**
- `400 Bad Request`: 请求体格式错误
- `404 Not Found`: 沙箱不存在
- `500 Internal Server Error`: 服务内部错误

---

## gRPC API

### 服务定义

```protobuf
syntax = "proto3";

package network_agent.api.v1;

service NetworkAgent {
    rpc EnsureNetwork(EnsureNetworkRequest) returns (EnsureNetworkResponse);
    rpc ReleaseNetwork(ReleaseNetworkRequest) returns (ReleaseNetworkResponse);
    rpc ReconcileNetwork(ReconcileNetworkRequest) returns (ReconcileNetworkResponse);
    rpc GetNetwork(GetNetworkRequest) returns (GetNetworkResponse);
    rpc Health(HealthRequest) returns (HealthResponse);
}
```

### EnsureNetwork

创建或确保网络配置。

**请求:**

```protobuf
message EnsureNetworkRequest {
    string sandbox_id = 1;
    string network_handle = 2;
    repeated PortMapping port_mappings = 3;
    CubeNetworkConfig cube_network_config = 4;
}
```

**响应:**

```protobuf
message EnsureNetworkResponse {
    string tap_name = 1;
    int32 tap_if_index = 2;
    string sandbox_ip = 3;
    repeated Interface interfaces = 4;
    repeated Route routes = 5;
    repeated ARPNeighbor arp_neighbors = 6;
    repeated PortMapping port_mappings = 7;
}
```

### ReleaseNetwork

释放网络资源。

**请求:**

```protobuf
message ReleaseNetworkRequest {
    string sandbox_id = 1;
}
```

**响应:**

```protobuf
message ReleaseNetworkResponse {
    bool ok = 1;
}
```

### ReconcileNetwork

协调网络状态。

**请求:**

```protobuf
message ReconcileNetworkRequest {
    string sandbox_id = 1;
    repeated PortMapping port_mappings = 2;
}
```

**响应:**

```protobuf
message ReconcileNetworkResponse {
    repeated PortMapping port_mappings = 1;
}
```

### GetNetwork

获取网络状态。

**请求:**

```protobuf
message GetNetworkRequest {
    string sandbox_id = 1;
}
```

**响应:**

```protobuf
message GetNetworkResponse {
    string tap_name = 1;
    int32 tap_if_index = 2;
    string sandbox_ip = 3;
    repeated Interface interfaces = 4;
    repeated Route routes = 5;
    repeated ARPNeighbor arp_neighbors = 6;
    repeated PortMapping port_mappings = 7;
}
```

### Health

健康检查。

**请求:**

```protobuf
message HealthRequest {}
```

**响应:**

```protobuf
message HealthResponse {
    bool ok = 1;
}
```

### gRPC 健康检查服务

除了自定义的 Health RPC，还实现了标准的 gRPC Health Check Protocol:

```protobuf
service Health {
    rpc Check(HealthCheckRequest) returns (HealthCheckResponse);
    rpc Watch(HealthCheckRequest) returns (stream HealthCheckResponse);
}
```

---

## FD Server API

### 概述

FD Server 通过 Unix Socket 使用 `SCM_RIGHTS` 传递 TAP 文件描述符。

### 请求格式

```json
{
    "name": "z192.168.0.10",
    "sandboxId": "sandbox-001"
}
```

| 字段 | 类型 | 必填 | 描述 |
|------|------|------|------|
| `name` | string | 是 | TAP 设备名称 |
| `sandboxId` | string | 是 | 沙箱 ID |

### 响应

成功时返回空 JSON `{}`，同时通过 `SCM_RIGHTS` 传递 TAP 文件描述符。

失败时返回错误信息:

```json
{
    "error": "tap not found"
}
```

### 使用示例 (Go)

```go
import (
    "encoding/json"
    "net"
    "golang.org/x/sys/unix"
)

func getTapFD(socketPath, tapName, sandboxID string) (int, error) {
    conn, err := net.Dial("unix", socketPath)
    if err != nil {
        return -1, err
    }
    defer conn.Close()

    // 发送请求
    req := map[string]string{
        "name":      tapName,
        "sandboxId": sandboxID,
    }
    if err := json.NewEncoder(conn).Encode(req); err != nil {
        return -1, err
    }

    // 接收 FD
    unixConn := conn.(*net.UnixConn)
    file, _ := unixConn.File()
    
    buf := make([]byte, 1024)
    oob := make([]byte, unix.CmsgsLen(4))
    
    n, oobn, _, _, err := unix.Recvmsg(int(file.Fd()), buf, oob, 0)
    if err != nil {
        return -1, err
    }

    // 解析响应
    var resp map[string]interface{}
    json.Unmarshal(buf[:n], &resp)
    if errMsg, ok := resp["error"]; ok {
        return -1, fmt.Errorf("%v", errMsg)
    }

    // 提取 FD
    cmsgs, _ := unix.ParseSocketControlMessage(oob[:oobn])
    fds, _ := unix.ParseUnixRights(&cmsgs[0])
    
    return fds[0], nil
}
```

---

## 错误处理

### HTTP 错误码

| 状态码 | 含义 |
|--------|------|
| 200 | 成功 |
| 400 | 请求格式错误 |
| 404 | 资源不存在 |
| 500 | 服务内部错误 |

### gRPC 错误码

| 错误码 | 含义 |
|--------|------|
| OK | 成功 |
| INVALID_ARGUMENT | 参数错误 |
| NOT_FOUND | 资源不存在 |
| INTERNAL | 内部错误 |

---

## 使用示例

### curl 示例

```bash
# 创建网络
curl -X POST --unix-socket /tmp/cube/network-agent.sock \
    http://localhost/v1/network/ensure \
    -H "Content-Type: application/json" \
    -d '{
        "sandboxID": "test-sandbox",
        "portMappings": [
            {"protocol": "tcp", "containerPort": 80}
        ]
    }'

# 获取网络
curl -X POST --unix-socket /tmp/cube/network-agent.sock \
    http://localhost/v1/network/get \
    -H "Content-Type: application/json" \
    -d '{"sandboxID": "test-sandbox"}'

# 释放网络
curl -X POST --unix-socket /tmp/cube/network-agent.sock \
    http://localhost/v1/network/release \
    -H "Content-Type: application/json" \
    -d '{"sandboxID": "test-sandbox"}'

# 健康检查
curl --unix-socket /tmp/cube/network-agent.sock http://localhost/healthz
```

### grpcurl 示例

```bash
# 健康检查
grpcurl -unix /tmp/cube/network-agent-grpc.sock \
    network_agent.api.v1.NetworkAgent/Health

# 创建网络
grpcurl -unix /tmp/cube/network-agent-grpc.sock \
    -d '{"sandbox_id": "test-sandbox"}' \
    network_agent.api.v1.NetworkAgent/EnsureNetwork
```
