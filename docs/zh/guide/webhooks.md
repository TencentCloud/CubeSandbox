---
title: Webhook 事件通知
lang: zh-CN
---

# Webhook 事件通知

CubeSandbox 可以在**沙箱生命周期事件**发生时，实时将其推送到你自己的 HTTP
端点，这样上层的 Agent 编排系统、运维平台或 IM 工具就无需再轮询 API 获取状态
变更。

可运行的接收端和企业微信转发桥见
[`examples/webhook-receiver/`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/webhook-receiver)。

## 支持的事件

| 事件 | 触发时机 | Payload 字段 |
|---|---|---|
| `sandbox.created` | 创建沙箱 | `sandbox_id`、`template_id` |
| `sandbox.paused` | 暂停沙箱 | `sandbox_id` |
| `sandbox.resumed` | 恢复沙箱 | `sandbox_id` |
| `sandbox.deleted` | 销毁沙箱 | `sandbox_id` |

## 配置

Webhook 通过一个配置文件（YAML / JSON / TOML）声明，用 `--webhook-config`
参数或 `CUBE_API_WEBHOOK_CONFIG` 环境变量指定其路径。两者都不设置时，Webhook
功能关闭，且无任何额外开销。

```yaml
# webhooks.yaml
webhooks:
  - url: "http://127.0.0.1:9100/webhook"
    events:
      - sandbox.created
      - sandbox.paused
      - sandbox.resumed
      - sandbox.deleted
    secret: "my-shared-secret"   # 可选
    timeout_ms: 5000             # 可选（默认 5000）
    max_retries: 3               # 可选（默认 3）
```

```bash
cube-api --webhook-config /path/to/webhooks.yaml
# 或
CUBE_API_WEBHOOK_CONFIG=/path/to/webhooks.yaml cube-api
```

可以注册**多个端点**，每个端点订阅不同的事件子集。启动时 CubeAPI 会打印
`webhook event notifications enabled` 及端点数量。

| 字段 | 必填 | 说明 |
|---|---|---|
| `url` | 是 | 接收 `POST` 的 HTTP 端点。 |
| `events` | 是 | 该端点订阅的事件类型。 |
| `secret` | 否 | 用于 HMAC-SHA256 签名的共享密钥（见下文）。 |
| `timeout_ms` | 否 | 单次请求超时（毫秒）。默认 `5000`。 |
| `max_retries` | 否 | 首次投递之后的重试次数。默认 `3`。 |

## 管理 API

端点也可以在运行时通过 HTTP 管理——无需重启。WebUI 就是用它让用户在控制台里
增删 webhook 的。

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/webhooks` | 列出已注册端点（secret 打码）。 |
| `POST` | `/webhooks` | 注册一个端点；`id` 由服务端分配。 |
| `DELETE` | `/webhooks/{id}` | 按 id 删除一个端点。 |

```bash
# 注册一个 webhook
curl -X POST http://localhost:3000/webhooks -H 'Content-Type: application/json' -d '{
  "url": "http://127.0.0.1:9100/webhook",
  "events": ["sandbox.created", "sandbox.deleted"],
  "secret": "my-shared-secret"
}'
# → 201 { "id": "8f3a…", "url": "…", "has_secret": true, ... }

# 列出，再按 id 删除
curl http://localhost:3000/webhooks
curl -X DELETE http://localhost:3000/webhooks/8f3a…   # → 204
```

响应中永远不含 `secret` 原文——只有 `has_secret: true|false`。

::: warning 运行时改动是内存态、且按实例隔离
通过该 API 增删的端点**不持久化**：重启即丢失。配置文件负责"开机时的持久初始
值"。（要持久化运行时改动需要引入存储，超出本功能范围。）

在**高可用部署（负载均衡后有多个 CubeAPI 副本）**下，该 API 只会修改处理本次请求的
那个副本，因此副本之间会发散——在某个副本上注册的端点，收不到由其他副本处理的
事件。高可用场景应把**配置文件作为声明式的唯一事实源**（每个副本一致），并把运行时
CRUD 视为「单实例、非权威」。全集群一致的运行时管理属于持久化控制面消费者的后续项。
:::

## Payload

每个事件以 HTTP `POST` 投递，`Content-Type: application/json`，并带有以下头：

| 头 | 含义 |
|---|---|
| `X-Cube-Event` | 事件类型，例如 `sandbox.created`。 |
| `X-Cube-Delivery` | 本次投递的唯一 id。**重试时保持不变**，因此接收端可在"投递已处理但被重试"时据此去重。 |
| `X-Cube-Signature` | HMAC-SHA256 签名，仅在配置了 `secret` 时出现（见下文）。 |

请求体始终包含 `event`、`timestamp`（RFC 3339，UTC）与 `sandbox_id`，并尽量
携带 `template_id` 等上下文字段。

```json
{
  "event": "sandbox.created",
  "timestamp": "2026-07-21T08:30:12.481Z",
  "sandbox_id": "sbx-abc123",
  "template_id": "base"
}
```

## 投递语义

- **非阻塞**：事件入队后由后台任务投递。沙箱 API 请求从不等待 Webhook 投递，
  因此接收端慢或不可达都不会拖慢或影响创建/暂停/恢复/销毁调用。
- **有界队列**：内存队列有容量上限。持续过载（接收端跟不上）时会丢弃最新事件，
  而不是无限增长内存——被丢弃的事件会打一条（限流的）`warn` 日志,
  而非以 OOM 的形式一次性丢光。
- **相互隔离**：每个（事件，端点）的投递独立进行——某个慢端点不会拖累其他端点。
  并发在飞投递数有上限,避免突发一次性打开过多连接。
- **自动重试**：投递失败（连接错误、超时或非 2xx 响应）会按指数退避
  （1s、2s、4s……）重试，最多 `max_retries` 次。最终放弃会以 `error` 级别记录日志。
- **关机时排空**：优雅停止（SIGTERM，例如滚动重启）时，队列中和在飞的投递会被
  排空——重试退避被截断，投递在有界的宽限期内尽量完成——因此常规发布不会丢失
  在飞的 webhook。
- **硬崩溃下至多一次**：投递态保存在内存中，因此硬杀（SIGKILL / OOM / 节点故障）
  仍可能丢失队列中和在飞的事件。持久记录仍是
  [结构化事件日志](./sandbox-logs.md)；跨崩溃的持久「至少一次」投递作为后续项规划
  （由控制面事件流消费者实现，与路线图上的控制面/数据面分离方向一致）。

接收端应尽快返回 `2xx`，把重活放到异步处理，并**基于 `X-Cube-Delivery`（每个事件
稳定不变）去重**——「至少一次」的重试意味着同一事件可能被投递多次。

### 顺序

投递**不保证全局有序**：每个（事件，端点）由独立任务投递，因此重试的
`sandbox.created` 可能晚于同一沙箱的 `sandbox.deleted` 到达。每个 payload 都带有单调
的 `timestamp`，接收端应**按 `timestamp` 消解顺序**——为每个 `sandbox_id` 记录最新状态，
并忽略 `timestamp` 早于已应用状态的事件（例如丢弃晚于更新的 `deleted` 才到达的
`created`）。严格有序投递将作为持久化控制面消费者的一部分（见下文）。

## 调优

投递子系统的默认值对生产是安全的；如需按部署环境调整，用环境变量覆盖（无需重新
编译）：

| 环境变量 | 默认 | 含义 |
|---|---|---|
| `CUBE_API_WEBHOOK_QUEUE_CAPACITY` | `10000` | 内存事件队列容量,超出后过载丢弃最新事件。 |
| `CUBE_API_WEBHOOK_DRAIN_GRACE_SECS` | `25` | 关机时等待在飞投递排空的最长时间,应小于编排器的终止宽限期。 |
| `CUBE_API_WEBHOOK_MAX_CONCURRENCY` | `256` | 并发在飞投递请求的上限。 |

事件丢失通过日志暴露:队列满会打一条限流的 `warn`（`webhook: queue full, dropping
event`）,重试耗尽会打一条 `error`（`webhook delivery giving up after exhausting
retries`）。

::: warning 日志级别会同时门控 webhook 投递
生命周期事件以 `info` 级别发出,和文件日志走同一个级别过滤器。如果把 `log_level`
调到 `warn` 或 `error`（比如想让日志更安静),webhook 投递会**随 info 级文件日志
一起停掉**。使用 webhook 时请把 `log_level` 保持在 `info`(或更低)。
:::

## 签名校验

设置 `secret` 后，CubeAPI 会用 HMAC-SHA256 对**原始请求体**签名，并放入
`X-Cube-Signature` 头：

```
X-Cube-Signature: sha256=<hex-digest>
```

请对**收到的原始字节**进行校验（不要先把 JSON 重新序列化——键顺序可能不同）：

```python
import hashlib, hmac

def verify(secret: str, body: bytes, header: str) -> bool:
    expected = "sha256=" + hmac.new(secret.encode(), body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(expected, header)
```

请使用常量时间比较（`hmac.compare_digest`）以防时序攻击。

## 对接企业微信 / 通用告警

用一个小桥接程序即可把事件转成企业微信群机器人消息。在群里添加群机器人、复制其
Webhook 地址，然后运行示例桥接：

```bash
WECOM_BOT_URL="https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=xxxxxxxx" \
CUBE_WEBHOOK_SECRET=my-shared-secret \
python3 examples/webhook-receiver/wecom_bridge.py
```

同样的模式适用于 Slack、飞书、PagerDuty 或任意 HTTP 告警系统：接收事件、校验
签名、再把格式化后的消息转发到目标平台 API。
