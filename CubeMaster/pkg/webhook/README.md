# Webhook 系统文档

## 概述

CubeSandbox 的 Webhook 系统是一个异步事件通知管道，允许外部服务订阅并接收沙箱和模板的生命周期事件通知。当沙箱被创建、暂停、恢复、销毁，或模板被构建、删除时，系统会通过 HTTP POST 将事件投递到预先配置的接收端点。

## 架构

```
业务代码 → Capture(捕获) → BufferedPublisher(缓冲通道) 
→ Writer(匹配订阅者 + 映射协议 + 编码 JSON) 
→ RedisDeliveryStore(持久化到 Redis) 
→ Worker goroutines(竞争领取任务) 
→ Sender(HTTP POST 投递)
```

## 事件类型

### 沙箱生命周期事件

| 事件类型 | 描述 | 触发时机 |
|---|---|---|
| `sandbox.lifecycle.created` | 沙箱创建完成 | 沙箱成功创建并从模板启动后 |
| `sandbox.lifecycle.killed` | 沙箱被销毁 | 沙箱被主动销毁或超时回收 |
| `sandbox.lifecycle.paused` | 沙箱暂停 | 沙箱从运行状态进入暂停状态 |
| `sandbox.lifecycle.resumed` | 沙箱恢复 | 沙箱从暂停状态恢复到运行状态 |
| `sandbox.lifecycle.updated` | 沙箱配置更新 | 沙箱的超时时间或网络规则被更新 |
| `sandbox.lifecycle.checkpointed` | 沙箱检查点(快照)创建 | 从运行中的沙箱创建快照成功 |

### 模板生命周期事件

| 事件类型 | 描述 | 触发时机 |
|---|---|---|
| `template.lifecycle.build.started` | 模板构建开始 | 构建任务被提交到模板构建中心(TemplateCenter)后 |
| `template.lifecycle.build.completed` | 模板构建完成 | 模板构建成功，制品已分发到节点 |
| `template.lifecycle.build.failed` | 模板构建失败 | 模板构建过程中发生不可恢复的错误 |
| `template.lifecycle.deleted` | 模板被删除 | 模板及其关联资源被成功清理 |
| `template.lifecycle.migrated` | 模板迁移完成 | 模板通过 redo/migrate 流程重新分发到新节点 |

### 快照生命周期事件

| 事件类型 | 描述 | 触发时机 |
|---|---|---|
| `template.lifecycle.snapshot.created` | 快照创建完成 | 从沙箱创建快照成功（即 `checkpointed`） |
| `template.lifecycle.snapshot.deleted` | 快照被删除 | 快照及其物理文件被成功清理 |
| `template.lifecycle.snapshot.rollback.completed` | 快照回滚完成 | 沙箱成功回滚到指定快照版本 |

## HTTP 投递格式

每个事件通过 HTTP POST 发送，请求体为 JSON 格式：

```json
{
    "id": "550e8400-e29b-41d4-a716-446655440000",
    "version": "v2",
    "type": "sandbox.lifecycle.created",
    "timestamp": "2026-11-03T11:06:02.123456789Z",
    "event_category": "sandbox",
    "event_label": "created",
    "sandbox_id": "sb-abc123",
    "sandbox_execution_id": "",
    "sandbox_template_id": "tmpl-xyz789",
    "snapshot_id": "",
    "sandbox_build_id": "",
    "sandbox_team_id": "",
    "event_data": {
        "sandbox_metadata": {
            "user_token": "kept"
        }
    }
}
```

### 不同事件类型的 event_data

**sandbox.lifecycle.created / resumed**：无额外数据，或包含 `sandbox_metadata`。

**sandbox.lifecycle.killed**：
```json
{
    "event_data": {
        "kill_reason": "request",
        "execution": {
            "started_at": "2026-11-03T10:00:00Z",
            "vcpu_count": 2,
            "memory_mb": 2048,
            "execution_time": 3600000
        }
    }
}
```

**sandbox.lifecycle.paused**：
```json
{
    "event_data": {
        "pause_mode": "full",
        "execution": { "...": "..." }
    }
}
```

**sandbox.lifecycle.updated**：
```json
{
    "event_data": {
        "set_timeout": "2026-11-03T12:00:00Z",
        "network_egress": {
            "allowed_cidrs": ["10.0.0.0/8"],
            "denied_cidrs": [],
            "allowed_domains": ["example.com"]
        }
    }
}
```

**template.lifecycle.build.started**：
```json
{
    "type": "template.lifecycle.build.started",
    "event_category": "template",
    "event_label": "build.started",
    "sandbox_template_id": "tmpl-xyz789",
    "sandbox_build_id": "job-456",
    "event_data": {
        "source_image_ref": "ubuntu:22.04",
        "instance_type": "cubebox",
        "build_mode": "remote"
    }
}
```

**template.lifecycle.build.completed**：
```json
{
    "type": "template.lifecycle.build.completed",
    "event_category": "template",
    "event_label": "build.completed",
    "sandbox_template_id": "tmpl-xyz789",
    "event_data": {
        "source_image_ref": "ubuntu:22.04",
        "artifact_id": "art-xxx",
        "instance_type": "cubebox",
        "build_mode": "remote",
        "ready_node_count": 3
    }
}
```

**template.lifecycle.build.failed**：
```json
{
    "type": "template.lifecycle.build.failed",
    "event_category": "template",
    "event_label": "build.failed",
    "sandbox_template_id": "tmpl-xyz789",
    "event_data": {
        "source_image_ref": "ubuntu:22.04",
        "error_message": "artifact distribution did not reach any node",
        "build_mode": "remote"
    }
}
```

### 快照事件的标识字段

快照事件以 `snapshot_id` 作为自己的标识，而不是把快照 id 塞进
`sandbox_template_id`——否则消费端无法区分一个模板和一个快照。

**template.lifecycle.snapshot.created**（与 `sandbox.lifecycle.checkpointed`
同一时刻发出，两者描述的是同一件事的两个资源）：
```json
{
    "type": "template.lifecycle.snapshot.created",
    "event_category": "snapshot",
    "event_label": "created",
    "sandbox_id": "sb-abc123",
    "sandbox_template_id": "tmpl-xyz789",
    "snapshot_id": "snap-new456",
    "event_data": {
        "backend": "s3"
    }
}
```

**template.lifecycle.snapshot.deleted** / **snapshot.rollback.completed**：
只带 `snapshot_id`（回滚事件另外带上发起回滚的 `sandbox_id`），
`sandbox_template_id` 为空。

```json
{
    "type": "template.lifecycle.snapshot.deleted",
    "event_category": "snapshot",
    "event_label": "deleted",
    "sandbox_id": "",
    "sandbox_template_id": "",
    "snapshot_id": "snap-new456",
    "event_data": {
        "error_message": ""
    }
}
```

## 投递保证

- **At-least-once 投递**：每个事件至少尝试投递一次
- **最多重试 3 次**（可配置），使用指数退避策略
- **Redis 持久化**：事件在投递前持久化到 Redis，Master 重启不会丢失
- **去重**：基于事件 `id` (UUID) 和 `delivery_id` 区分重试
- **不保证 exactly-once**：接收方应具备幂等处理能力

## 配置

在 CubeMaster 的配置文件中配置 Webhook：

```yaml
webhooks:
  enabled: true
  ingress_capacity: 1024      # 缓冲队列大小 (1-4096)
  workers: 4                   # 投递 Worker 数 (1-32)
  request_timeout: 5s          # HTTP 请求超时 (1ms-1min)
  max_attempts: 3              # 最大重试次数 (1-1000)
  lease: 15s                   # 任务租约时间
  allow_http: false            # 是否允许 HTTP (默认仅 HTTPS)
  allowed_cidrs: []            # HTTP 白名单 CIDR
  store:
    backend: redis
    redis:
      nodes: "redis.internal:6379"
      db_no: 7
    operation_timeout: 2s
    max_records: 10000
    max_active: 8
    max_idle: 4
    terminal_retention: 24h
  subscriptions:
    - id: my-webhook
      enabled: true
      url: https://my-server.example.com/events
      events:
        - sandbox.lifecycle.created
        - sandbox.lifecycle.killed
        - sandbox.lifecycle.paused
        - sandbox.lifecycle.resumed
        - sandbox.lifecycle.updated
        - sandbox.lifecycle.checkpointed
        - template.lifecycle.build.started
        - template.lifecycle.build.completed
        - template.lifecycle.build.failed
        - template.lifecycle.deleted
        - template.lifecycle.migrated
        - template.lifecycle.snapshot.created
        - template.lifecycle.snapshot.deleted
        - template.lifecycle.snapshot.rollback.completed
```

## 接收端点要求

接收端点应：
1. 接受 `POST` 请求，`Content-Type: application/json`
2. 在 5 秒内（由 `request_timeout` 配置）返回 HTTP 200/201/202 表示成功
3. 返回其他状态码触发重试（直到 `max_attempts` 上限）
4. 请求头中包含：
   - `e2b-webhook-id`: 事件唯一标识
   - `e2b-delivery-id`: 本次投递唯一标识
5. 如果使用 HTTP（非 HTTPS），需要设置 `allow_http: true` 并配置 `allowed_cidrs` 白名单

## 扩展 Webhook

### 添加新的事件类型

1. 在 `CubeMaster/pkg/webhook/event.go` 中定义新的事件常量
2. 更新 `validType()`、`IsSandboxEvent()`/`IsTemplateEvent()`/`IsSnapshotEvent()` 函数
3. 在 `CubeMaster/pkg/webhook/encode.go` 中更新 `eventCategoryAndLabel()` 和 `protocolData()`
4. 在 `CubeMaster/pkg/base/config/webhook.go` 中添加新事件到配置验证
5. 在业务代码中通过对应的 `Publish*` 辅助函数发布事件；新增事件类型时要同时加上
   辅助函数和发出它的生产者，只在前四步登记的类型等于一个永远收不到事件的订阅

## 设计原理

- **缓冲通道**：接收事件后先入内存缓冲队列，不阻塞业务路径
- **Redis 持久化**：Writer goroutine 将事件从缓冲写入 Redis，保证 Master 重启不丢事件
- **Worker 竞争**：多个 Worker 从 Redis 竞争领取投递任务，支持水平扩展
- **租约机制**：领取的任务有租约时间，Worker 崩溃后任务被其他 Worker 重新领取
- **指数退避**：失败重试使用指数退避 + 随机抖动，避免 thundering herd
