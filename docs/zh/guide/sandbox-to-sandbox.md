# 沙箱间互访

一个沙箱访问另一个沙箱中的服务有两条数据面路径：**CubeProxy Host 路由**
（与公网 URL 相同的入口）和**母机端口映射**（CubeVS eBPF DNAT）。
方式一通过 HTTP Host 头部指定访问的沙箱 id 和 port
（`<port>-<sandboxID>.cube.app`），由 CubeProxy 做路由转发代理到目标沙箱；
方式二则是直接访问目标沙箱的 nodeIP 和映射的 port，即 `节点IP:hostPort`。

方式一数据流程（CubeProxy 与沙箱不同机）：

```text
  源沙箱 A                CubeProxy 节点               目的节点
 （源节点）               CubeProxy :80                主网卡 ingress
     │                        │                           │
     │  ① Host: 8080-B.cube.app                           │
     │    （--resolve 钉到 CubeProxy 节点 IP）             │
     │───────────────────────►│                           │
     │                        │  ② 访问 hostIP:hostPort    │
     │                        │──────────────────────────►│ from_world DNAT
     │                        │                           │ 至 sandboxIP:8080
     │                        │                           │──► 目的沙箱 B :8080
```

方式二数据流程 · 同节点（源节点 = 目的节点）：

```text
  源沙箱 A            from_cube                  主网卡 ingress
     │                   │                           │
     │ ① nodeIP:hostPort │                           │
     │──────────────────►│ ② SNAT 后软件环回          │
     │                   │    bpf_redirect           │
     │                   │    (BPF_F_INGRESS)        │
     │                   │──────────────────────────►│ from_world DNAT
     │                   │                           │ 至 sandboxIP:8080
     │                   │                           │──► 目的沙箱 B :8080

  全程在本节点内完成，不依赖网卡/交换机 hairpin。
```

方式二数据流程 · 跨节点：

```text
  源沙箱 A            from_cube                    目的节点
 （源节点）           （源节点）                   主网卡 ingress
     │                   │                           │
     │ ① nodeIP:hostPort │                           │
     │──────────────────►│ ② SNAT，出物理网卡         │
     │                   │═══════════════════════════►│ from_world DNAT
     │                   │      （物理链路）          │ 至 sandboxIP:8080
     │                   │                           │──► 目的沙箱 B :8080
```

完整可运行的示例脚本见
[examples/code-sandbox-quickstart](https://github.com/TencentCloud/CubeSandbox/tree/main/examples/code-sandbox-quickstart)
下的 `sandbox-curl-sandbox.py`（方式一）与 `sandbox-port-mapping.py`（方式二）。

## 如何选择

| 维度 | 方式一：CubeProxy Host 路由 | 方式二：母机端口映射 |
|------|---------------------------|---------------------|
| 协议 | 仅 HTTP / HTTPS / gRPC（L7，按 Host 头路由） | 任意 TCP 协议（L4 DNAT） |
| 数据面路径 | 经 CubeProxy 转发，多一跳代理 | eBPF DNAT 直连沙箱，无代理开销 |
| 入口鉴权 | 支持 `allow_public_traffic=False` 的 per-sandbox token | 不支持 token，由网络策略与 expose-port 控制暴露面 |
| 服务端口的来源 | URL 的 `<port>` 前缀指定 B 的容器端口 | info API 查询 B 的容器端口对应的 `hostIP:hostPort` |
| 端点稳定性 | 域名格式固定，暂停/恢复后不变 | hostIP:hostPort 可能在暂停/恢复后改变（见下文） |
| 沙箱内 DNS | 不需要（`--resolve` 钉到 proxy IP） | 不需要（直接用节点 IP） |

典型场景：

- **方式一**：沙箱内 Agent 调用另一个沙箱托管的 HTTP API / gRPC 服务；
  需要复用 CubeProxy 的 token 鉴权；希望端点 URL 稳定、免去查询 hostPort。
- **方式二**：沙箱间跑任意 TCP 协议（数据库、消息队列、自定义二进制
  协议）；对时延敏感、想绕过代理；端口可在模板中预先声明。

## 模板构建：声明服务端口

两种方式都以模板 `--expose-port` 声明的端口为服务端口契约 —— 调用方
只需知道 B 的容器端口，**无需感知是否与 B 同节点**。在标准模板基础上
追加服务端口即可（以 8080 为例；镜像仓库地址见
[快速入门](./quickstart.md)，中国大陆环境用 `cube-sandbox-cn` 仓库）：

```bash
cubemastercli tpl create-from-image \
  --image cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  --writable-layer-size 1G \
  --expose-port 49999 \   # code interpreter（sandbox-code 默认）
  --expose-port 49983 \   # envd（sandbox-code 默认）
  --expose-port 8080  \   # 你的服务端口
  --probe 49999
```

说明：

- `--expose-port` 可重复多次；只有声明过的端口才会分配 hostPort 映射
  （方式二与方式一的跨节点路径都依赖它）。
- `--probe` 是模板就绪探针端口，`sandbox-code` 用 49999。
- 命令会打印 `template_id`，并等待构建完成（`READY`）；也可用
  `cubemastercli tpl list` 查看状态。
- 已有模板追加端口只能重新构建 —— expose-port 是模板属性，运行时不可改。

创建沙箱时使用新模板 ID 即可，端口映射由 Cubelet 在沙箱启动时自动完成，
无需在创建请求里额外传参。

## 方式一：CubeProxy Host 路由

CubeProxy 按 `<port>-<sandboxID>.<domain>` 形式的 Host 头路由到对应沙箱。
沙箱 A 内用 curl `--resolve` 把域名钉到 CubeProxy 节点 IP，无需 DNS：

```bash
curl -sS --resolve 8080-<sandboxID-B>.cube.app:80:<proxy节点IP> \
     http://8080-<sandboxID-B>.cube.app/
```

B 用按上文声明了服务端口的模板创建。调用方在 Host 头里指定 B 的容器
端口：同节点时 CubeProxy 直接拨 `sandboxIP:容器端口`；跨节点时经该端口
对应的 `hostIP:hostPort` 映射进入 B（未声明的端口跨节点返回 404）。

> 实现细节：同节点时 CubeProxy 的同机直连不检查端口映射，未声明的端口
> 也能访问——但这是宽松行为，A、B 一旦被调度到不同节点就会 404，
> 不应依赖。

特点：

- 与公网 URL 走同一入口，**鉴权语义一致**：B 以
  `network={"allow_public_traffic": False}` 创建时，请求必须携带
  `e2b-traffic-access-token` 头（见[限制公开访问](./restrict-public-access.md)）。
- 依赖 CubeProxy 可用。

限制：

- **仅支持 L7 协议**：HTTP / HTTPS / gRPC（gRPC 基于 HTTP/2，可经 Host
  路由）。任意 TCP/UDP 服务（如数据库、Redis、自定义协议）无法用此方式，
  请改用方式二。
- 服务端口需经模板 `--expose-port` 声明（见上文"模板构建：声明服务端口"）。

## 方式二：母机端口映射（hostIP:hostPort）

模板以 `--expose-port` 声明端口后，Cubelet 在创建沙箱时向 CubeVS 下发
端口映射：发往 `<节点IP>:<hostPort>` 的 TCP 流量由主网卡 ingress 上的
`from_world` eBPF 程序直接 DNAT 进沙箱，**不经过 CubeProxy**。

### 前提

- cubemaster 配置 `cubelet_conf.enable_exposed_port: true`（默认开启）；
- 模板已按上文"模板构建：声明服务端口"声明了服务端口。

### 查询分配的 hostPort

hostPort 按沙箱动态分配，通过 CubeMaster info 接口查询：

```bash
curl -s -H 'X-Cube-Caller-Host-IP: 198.51.100.254' \
  'http://<cubemaster>:8089/cube/sandbox/info?sandbox_id=<B>&container_port=8080'
# → data[0].exposed_port_endpoint = "9.135.152.214:20006"
#   data[0].exposed_port_mode     = "host_port"
```

与沙箱同节点的调用方默认拿到的是 `tap_ip` 端点（`sandboxIP:containerPort`）；
`X-Cube-Caller-Host-IP` 头用于强制返回 `host_port` 端点。

### 沙箱 A 访问

```bash
curl -sS http://<节点IP>:<hostPort>/
```

A、B 同节点时，沙箱 A 的报文在 `from_cube` 中先做 SNAT，再由软件环回进
节点网卡 ingress 触发 DNAT —— 不依赖网卡/交换机 hairpin；跨节点（A、B 在
不同节点）流量经物理链路到达同一 ingress 路径。

特点：

- 数据面直连，无代理开销；**支持任意 TCP 协议**（HTTP、数据库、消息
  队列、自定义二进制协议均可）。
- 不经过 CubeProxy，因此**不适用** `allow_public_traffic` token 鉴权，
  暴露面由[网络策略](./network-policy.md)与模板 expose-port 共同控制。

限制：

- **仅 TCP**，不支持 UDP。
- 服务端口必须是模板 `--expose-port` 声明过的端口。
- **暂停/恢复后端点可能变化**：hostPort 在沙箱创建（含 resume 时的内部
  重建）时动态分配，沙箱 pause → resume 后 `hostIP:hostPort` 可能不同
  （跨节点调度时 hostIP 也会变）。调用方必须在 B 恢复后重新查询 info
  接口获取新端点，不要缓存长期使用。

## 补充：控制面方式（SDK connect）

如果需求是"在 A 里操作 B"（执行命令、读写文件）而非访问 B 的服务端口，
可以在 A 内安装 `cubesandbox` SDK 并 `Sandbox.connect(B的ID)`：

```python
from cubesandbox import Sandbox
sandbox_b = Sandbox.connect("<sandboxID-B>")
sandbox_b.commands.run("echo hello")
```

A 内需设置 `CUBE_API_URL`（指向 CubeAPI 节点可达地址，注意 `.env` 里的
`127.0.0.1` 在沙箱内指沙箱自身）与 `CUBE_PROXY_NODE_IP`。
完整示例见 `sandbox-in-sandbox.py`。

## 测试验证

两种方式都有可运行的端到端示例脚本（位于
[examples/code-sandbox-quickstart](https://github.com/TencentCloud/CubeSandbox/tree/main/examples/code-sandbox-quickstart)），
脚本自动完成：创建沙箱 A/B → B 内起 HTTP 服务 → A 内 curl 访问 → 校验响应：

- `sandbox-curl-sandbox.py` —— 方式一（服务端口固定 8080）；
- `sandbox-port-mapping.py` —— 方式二（端口可用 `CUBE_SERVICE_PORT`
  覆盖，默认 8080）。

两者都应使用按上文"模板构建"声明了对应端口的模板（`CUBE_TEMPLATE_ID`）。

环境准备、运行命令与预期输出见该目录的 README。若 A 内 curl 返回
ECONNREFUSED（退出码 2），`sandbox-port-mapping.py` 会打印排查清单
（端口未 expose、`enable_exposed_port` 未开、出口策略拦截）。

## 通用注意点

- **出口策略**：A 若配置了 `allow_out` / `deny_out`（见
  [网络策略](./network-policy.md)），白名单需覆盖 CubeAPI / CubeProxy /
  宿主节点 IP。
- **API 地址**：沙箱内访问控制面时，loopback 地址（`127.0.0.1`）指向
  沙箱自身，必须替换为节点可达地址。

## 延伸阅读

- [网络策略](./network-policy.md) —— 沙箱出站白/黑名单与 L7 规则。
- [限制公开访问](./restrict-public-access.md) —— CubeProxy 入口的 per-sandbox token。
- [安全代理](./security-proxy.md) —— 出站 HTTP/HTTPS 的域名过滤与凭证注入。
