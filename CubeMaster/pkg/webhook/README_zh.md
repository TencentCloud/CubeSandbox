# 沙箱生命周期 Webhook

[English](README.md)

CubeMaster 将生命周期通知发送到静态配置的 HTTP 接收端。订阅由运维配置，
作用于整个集群，默认关闭；目前不是通过 SDK 管理的用户级订阅。

## 启用方法

1. 准备接收 JSON POST 的服务，处理重复通知，并在可靠接收后尽快返回 2xx。
2. 在每个 CubeMaster 配置中设置 `webhooks`，将下面的接收地址和 Redis
   配置替换为实际服务。
3. 使用相同配置重启**所有 Master**。不支持热更新。通过现有 API/SDK
   执行真实沙箱操作，检查接收端记录和 Master 的 webhook 指标。

```yaml
webhooks:
  enabled: true
  workers: 4
  request_timeout: 5s
  max_attempts: 3
  store:
    backend: redis
    redis:
      nodes: "redis.internal:6379"
      password: "REPLACE_WITH_REDIS_PASSWORD"
      db_no: 7
  subscriptions:
    - id: lifecycle-receiver
      enabled: true
      url: https://receiver.example.com/cubesandbox/events
      events:
        - sandbox.lifecycle.created
        - sandbox.lifecycle.resumed
        - sandbox.lifecycle.paused
        - sandbox.lifecycle.checkpointed
        - sandbox.lifecycle.updated
        - sandbox.lifecycle.killed
```

全局和订阅开关都必须开启。事件名称精确匹配，不支持通配符。示例 URL 是
占位地址，不是实际接收服务。

`store.redis` 是完整连接配置覆盖；省略时复制主 Redis 配置。如果需要隔离
通知系统故障和 OOM，应使用独立 Redis 服务。独立逻辑 DB 不能隔离服务故障
或资源争用。Redis 持久化、存储容量、访问控制及 `noeviction` 需要自行配置，
webhook 不会修改 Redis 服务端设置。

公网 HTTPS 使用正常证书校验。私网或回环目标需要配置 `allowed_cidrs`；
本地 HTTP 测试还需开启 `allow_http`：

```yaml
# 合并到 webhooks 下，同时将订阅 URL 改为本地接收端。
allow_http: true
allowed_cidrs: ["127.0.0.0/8"]
```

CIDR 白名单不会关闭 TLS 校验，也不会放行被禁止的特殊地址。
不跟随重定向，不使用环境变量中的 HTTP 代理。
完整限制见[配置说明](DELIVERY.md#configuration)，本地接收端见
[测试运行说明](../../../tests/e2e/webhook/README.md)。

## 事件语义

| 事件后缀（前缀为 `sandbox.lifecycle.`） | 发送时机 |
| --- | --- |
| `created` | 创建成功并完成最终后处理 |
| `resumed` | 暂停恢复或可信用户快照恢复成功；普通模板快速启动仍为 `created` |
| `paused` | 本地暂停及 Master 暂停完成 |
| `checkpointed` | 本地快照完成，最终任务 READY 状态持久化成功 |
| `updated` | 有限超时写入确认成功，或支持范围内的网络更新应用成功 |
| `killed` | 销毁和必要清理完成，覆盖支持的同步、异步路径 |

暂停和快照通知表示**本地成功**，不表示 S3 上传完成或远端耐久性成立。
之后上传失败不会撤回事件。内部刷新、回滚、暂停拆除不应产生用户级更新或
销毁通知。仅设置超时 `-1` 不触发 `updated`；纯 L7 网络变更未被完整表达。
精确完成边界和字段映射见 [DELIVERY.md](DELIVERY.md)。

## 接收端约定

请求包含 `Content-Type: application/json`、`e2b-webhook-id`（订阅 ID）和
`e2b-delivery-id`（每次实际投递生成的新 UUID）。JSON 包含 `id`、`version`、
`type`、`timestamp`、沙箱身份字段和事件专属 `event_data`；结构见
[encode.go](encode.go) 和 [protocol.go](protocol.go)。无法取得的身份字段
可能为空，因此不能保证兼容要求完整 E2B execution/team/build 身份的接收端。
不发送签名或版本 HTTP 头。

- 使用正文 `id` 去重，必要时加入订阅作用域。同一通知重试保持事件 ID 和
  正文不变，delivery ID 用于区分投递尝试。
- 不依赖顺序、恰好一次或零丢失。持久化前的内存事件可能丢失，已保存任务
  也可能重复投递。
- 2xx 表示成功；其他状态码、网络错误和超时受有限重试预算约束。
  Claim 即消耗尝试次数，即使尚未发送 HTTP。
- 如需可靠下游处理，应先持久保存工作再响应。预算耗尽进入终态后，接收端
  恢复不会自动重放该通知。

业务请求路径只捕获有界事件数据并执行非阻塞入队。Redis 持久化、编码和
HTTP 投递在后台进行。队列满、非法或超限数据、存储故障可能导致通知丢失，
而不是让业务请求等待投递。这不代表零 CPU/内存开销，也不保证任何负载下
性能完全不受影响。

## 已实现与未支持范围

| 能力 | 状态 |
| --- | --- |
| 六类生命周期事件 | 已实现，遵循上述完成边界 |
| 静态多订阅和事件过滤 | 已实现，修改后需重启全部 Master |
| Redis 持久化、有限重试、租约和竞争领取 | 已实现，支持单机 Redis 和 Sentinel |
| TLS/地址校验、有界队列和退出、监控指标 | 已实现 |
| 动态订阅增删改查、用户级订阅 API | 未实现 |
| 签名、自定义认证头、公开测试事件 API | 未实现 |
| 投递历史、查询、重放 API | 未实现 |
| Redis Cluster 路由 | 不支持 |
| 全序、恰好一次、零丢失、物理断电保证 | 不提供 |
| E2B 全部字段和操作的完整兼容 | 不提供，详见兼容性说明 |

## 运维与验收

关注 `cubemaster_webhook_operations_total` 和队列、inflight 等指标。计数器
用于诊断，不是持久投递历史。没有通知时，先检查开关、事件过滤和最终业务
结果，再排查捕获、持久化、领取及 HTTP 结果。非法 webhook 配置会禁用该
模块，应检查启动日志，不能仅凭 API 健康就认为 webhook 已启用。

敏感订阅撤销应先停止所有旧投递实例，再应用修改。全局关闭会保留已保存的
积压任务；使用新启动配置的已启用实例会取消已修改或删除的目标。已开始的
HTTP 请求无法撤回。OOM 或损坏排查应保留队列数据，仅恢复 Redis 容量未必
恢复调度；处理方法见 [DELIVERY.md](DELIVERY.md) 和 [STORE.md](STORE.md)。

[验收摘要](END-TO-END-VALIDATION.md) 区分组件、真实 XFS/S3、恢复及延迟证据
与未验证保证。单并发 API 实验验证的是指定投递故障下的业务隔离，不是通用
高并发性能 SLO。复现方法及破坏性测试的显式启用条件见
[测试运行说明](../../../tests/e2e/webhook/README.md)。

可移植的测试配置见[环境变量示例](../../../tests/e2e/webhook/env.example)。
复制后填写自己的专用运行目录和资源清单；故障测试不要求沿用原开发者的安装路径。
