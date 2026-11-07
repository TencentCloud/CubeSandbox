# 沙箱生命周期 Webhook

[English guide](../../guide/webhooks.md)

CubeMaster 向运维配置的接收端 POST 生命周期通知。静态订阅作用于整个集群，默认关闭，
不是 SDK 管理的用户级订阅。Webhook 只观察既有业务结果，投递失败不改变这些结果。

## 开启通知

1. 准备接收 JSON POST 的服务，处理重复请求，接纳下游工作后及时返回 2xx。
2. 将此块合并到各 Master 配置，替换占位 URL。保留其它平台设置，包括业务 Redis。
3. 使用一致配置重启**所有 Master**。运行中的订阅不热更新。执行真实沙箱操作，
   检查接收端记录和 Master 指标。

```yaml
webhooks:
  enabled: true
  ingress_capacity: 1024
  workers: 4
  request_timeout: 5s
  max_attempts: 3
  store:
    max_records: 10000
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

全局和订阅 `enabled` 必须都为 true。过滤精确匹配完整事件名，不接受通配符和重复事件。
订阅 ID 必须非空且唯一，用于标识订阅，不是沙箱 ID。

| `webhooks` 下的设置 | 默认值 | 接受范围 / 含义 |
| --- | --- | --- |
| `enabled` | `false` | 全局开关 |
| `ingress_capacity` | `1024` | 每 Master 1–4096 个捕获事件 |
| `workers` | `4` | 每 Master 1–32 个投递 worker |
| `request_timeout` | `5s` | 整次 HTTP 操作 1ms–1m |
| `max_attempts` | `3` | 1–1000 次尝试，包含首次 |
| `store.max_records` | `10000` | 每 Master 1–1000000 个 pending + inflight 目标任务 |
| `allow_http` | `false` | 显式允许私网/本地 HTTP |
| `allowed_cidrs` | 空 | 最多 64 个有效 CIDR，允许私网/回环目标 |
| `subscriptions` | 空 | 最多 64 个订阅 |
| `subscriptions[].enabled` | `false` | 订阅开关 |
| `subscriptions[].id`, `.url`, `.events` | 未设置 | 必填；ID ≤1024 字节，URL ≤4096 字节，1–6 个不同的支持事件 |

这些数量限制不是进程 RSS 上限。全局开启但没有活动订阅时，不执行捕获或投递。
关闭时跳过通知准备；开启后仍消耗 CPU 和内存。

## 事件与成功边界

| 完整事件名 | CubeMaster 观察到成功的时点 |
| --- | --- |
| `sandbox.lifecycle.created` | 公开创建获得既有成功结果并完成成功分支后处理。普通模板快速启动仍为 `created`。 |
| `sandbox.lifecycle.resumed` | 公开创建从可信用户快照模板启动成功，或暂停恢复的最终代理 purge 成功。不能仅凭调用方 annotation/ID 前缀判为快照。 |
| `sandbox.lifecycle.paused` | Cubelet 本地暂停和 Master `pausesnap.Complete` 成功。 |
| `sandbox.lifecycle.checkpointed` | 实际执行的快照任务完成本地工作并持久化 READY，位于最终查询前。后续查询失败不撤回通知；只查询 READY 不重复发布。 |
| `sandbox.lifecycle.updated` | 公开 SetTimeout/Refresh 返回可用有限截止时间，或既有节点网络更新成功。best-effort spec 写失败不撤销节点网络成功。 |
| `sandbox.lifecycle.killed` | 既有同步销毁调用或异步 destroy handler 成功，包括原有容忍/warn-only 情况。任务入队成功不发布。 |

XFS 与 S3 的 pause/checkpoint 都表示**本地成功**，不表示 S3 上传完成、远端耐久性或
跨节点恢复能力。后续上传状态不撤回或重复事件。创建、暂停、READY 写入或最终 resume
purge 失败，不发布其对应成功事件。内部创建回滚、暂停拆除和 resume timeout 刷新不
额外产生用户级 killed/updated。Mock 旁路不是真实生命周期通知入口。

创建取消遵循既有业务结果：取消先赢、操作返回失败时，后台随后成功和内部回收均不发布
created/resumed/killed。若业务成功先返回，之后请求在通知 hook 前取消，不抑制 created
或用户快照 resumed。投递仍为 best effort。

SetTimeout 接受正数秒数、`0`（立即截止）和 `-1`（永不超时），`-1` 不发 timeout-only
事件。Refresh 只接受正数秒数，拒绝 `0` 和负数。有限成功响应的 `EndAt > 0` 即可发布，
provider 缺失或失败时也可能如此，因为截止时间可由既有 fallback 提供。
**这不确认 Redis HSET 或 XADD 成功。** Resume 内部刷新不发布 updated。
重复成功操作可各自产生新事件。

Killed 遵循原业务对缺节点、nil/not-found 响应和 warn-only 清理的成功规则，不增加
物理销毁或 binding 删除完成的独立证明。业务重试仍执行原节点调用，通知不引入完成门控
或生产端去重。

## 接收与去重

| Header | 值 |
| --- | --- |
| `Content-Type` | `application/json` |
| `e2b-webhook-id` | 配置的订阅 ID |
| `e2b-delivery-id` | 每次投递尝试的新 UUID |

Payload `id` 和原始正文在重试及多订阅 fanout 间不变。按 payload `id` 去重，必要时
加入订阅作用域。Delivery ID 标识一次尝试，不是幂等键。先保存必要下游工作再确认，
容忍并发、乱序和重复。接收成功但响应丢失也可能导致重试。

任意 2xx 表示成功。非 2xx（含 3xx/4xx/5xx）、网络错误和超时受有限预算约束。Claim
即消耗一次，因此取消、身份生成或地址校验可能在 HTTP Header 写出前消耗预算。
指数退避带抖动，最高 30s。预算耗尽后，接收端恢复也不会自动重放。

## Payload 兼容性

本功能首版沿用现有 **`v2` payload**，不宣称完整兼容 E2B 字段/操作。路径相对 JSON 根。

| 字段 | 来源及空值/省略规则 | 接收端影响 |
| --- | --- | --- |
| `id` | 成功时捕获的 UUID | 稳定的事件去重键 |
| `version` | 固定 `"v2"` | 不是功能/签名版本 |
| `type` | 六个完整名称之一 | 精确匹配 |
| `timestamp` | 捕获时的 UTC RFC3339Nano 时间 | 不是 HTTP 发送时间或执行起点 |
| `sandbox_id` | 真实响应/请求沙箱 ID | 不是 E2B execution ID |
| `sandbox_template_id` | 可信创建解析结果或本来已读的 spec，否则 `""` | Timeout/network/killed 不为补字段查询 |
| `sandbox_execution_id` | `""`；无等价执行身份 | 严格 UUID/必填值解码器可能拒绝 |
| `sandbox_build_id` | `""`；无可信构建身份 | 不用 job/snapshot ID 替代 |
| `sandbox_team_id` | `""`；无等价 team | 严格 UUID/team 校验可能拒绝 |
| `event_category` | `""`，保留键 | 不伪造旧分类 |
| `event_label` | `""`，保留键 | 不伪造旧标签 |
| `events_ttl_days` | 省略 | 无事件历史留存服务 |
| `event_data` | 空时省略 | 不假定总是存在 |
| `event_data.sandbox_metadata` | 可信当前公开创建输入，模板合并前取得；nil/未知省略，已知空 `{}` | 用户快照创建可携带当前输入；后续操作不继承 |
| `event_data.execution` | 仅 paused/killed；至少 `{"started_at":""}` | 需容忍部分数据 |
| `event_data.execution.started_at` | `""`；无可靠执行起点 | 空字符串不是有效 RFC3339 |
| `event_data.execution.execution_time` | 省略；无可靠时长 | 不伪造零，不用于计费 |
| `event_data.execution.vcpu_count` | 已有单容器 `cubebox` CPU 为精确正整数时填写，否则省略 | `1000m` → 1；`1500m` 省略 |
| `event_data.execution.memory_mb` | 已有单容器 `cubebox` 内存，先向上取整字节再取整数 MiB，否则省略 | `1Gi` → 1024，`512M` → 488，`1.5Mi` → 1 |
| `event_data.set_timeout` | 捕获的有限响应 EndAt，UTC RFC3339 秒精度 | 可来自 fallback；不确认持久化，不编码 `-1` |
| `event_data.network_egress.allowed_cidrs` | 请求 IPv4 IP/CIDR；IP → `/32`，CIDR 规范化；空 `null` | 不是完整策略 diff；保留显式 DNS IP |
| `event_data.network_egress.denied_cidrs` | 请求 IPv4 deny；internet false 覆盖为 `["0.0.0.0/0"]`；空 `null` | Internet true/nil 使用本次请求 DenyOut |
| `event_data.network_egress.allowed_domains` | AllowOut 域名/通配符，转小写去尾点；空 `null` | 无 L7 Rules/Inject/凭据或节点注入 DNS |
| `event_data.pause_mode` | 当前已确认暂停路径为 `"full"` | 不是内部 snapshot_type |
| `event_data.resume_mode` | 当前暂停/用户快照恢复路径为 `"restore"` | 不承诺未来所有路径 |
| `event_data.kill_reason` | 保留显式原因；已知用户请求 `"request"`，超时 `"timeout"`，无来源 `"unknown"` | 不含内部错误文本，不额外通知清理 |

Metadata 排除精确的 `host-mount`、`X-Caller` 和 `cube.master.` 命名空间；普通用户键
含 token/secret/password 仍是用户数据，接收端日志应按此处理。创建捕获允许 64 个用户项、
key/value 共 16 KiB、key ≤256 字节、value ≤4096 字节。未知 metadata 不从合并标签、
缓存或私有存储字段补回。超限丢弃整条通知，不截断，不使业务失败。

资源字段与缺失时间独立。其它实例类型、多容器、非法/分数 CPU 和未知数量不猜测；
当前 killed 生产者无资源来源。网络投影不表达 IPv6 策略（当前节点拒绝）、全部 L7-only
变化或内部 DNS/私网规则，不补 E2B 默认 DNS。空分类是 `null`，包括空列表经 protobuf
往返后；要求 `[]` 的接收端需适配。

无 secret、自定义认证 Header、签名、`e2b-signature`、`e2b-signature-version`
或其它版本 Header。Header 和 ID 不能认证发送者。强制验签接收端不兼容，选择集成方式时
保留其安全要求。无动态订阅 CRUD、用户级订阅和公开测试事件 API。

### 编码示例

此例为既有 `TestEncodeSixEvents` 的 created fixture，输入来自 `capture_test.go`
的 `captureInput`，已对照当前编码器核对。ID/时间是测试数据，不是生产观测。Metadata
未知时省略 `event_data`；公开创建中已知空 metadata 则包含 `sandbox_metadata: {}`。

```json
{
  "id": "d12727fc-ab97-43d8-80cf-556cc1215017",
  "version": "v2",
  "type": "sandbox.lifecycle.created",
  "timestamp": "2026-10-14T12:00:00Z",
  "event_category": "",
  "event_label": "",
  "sandbox_id": "sb-1",
  "sandbox_execution_id": "",
  "sandbox_template_id": "tpl-1",
  "sandbox_build_id": "",
  "sandbox_team_id": ""
}
```

## 容量、丢失与退出

每 Master 有两级上限，由该 Master 全部订阅共享：

- Ingress 按等待 writer 的捕获事件计数，满时丢弃事件。
- `store.max_records` 按目标任务计数，含 pending 重试和 inflight HTTP。一个事件匹配
  三个订阅需三条记录。Fanout 可部分成功，任务满时丢该目标，不等空位。Retry 复用原记录，
  成功/耗尽/取消释放容量。

每个目标任务的编码正文最多 256 KiB。目标副本和 worker/transport 也占用内存，
因此这些上限不限制进程 RSS。

通知状态由 `DeliveryStore` 后的进程内存保存。重启、崩溃或退出可丢失剩余工作。
各 Master 不共享通知积压、不互相接管。无磁盘/Redis 持久化、严格顺序、恰好一次、
零丢失、历史/查询或 replay API。Redis 持久化/HA 留待后续，业务 Redis 仍是平台依赖。
部分 webhook 联合集成测试有意经过业务 Redis→内存 Runtime→HTTP，
这不构成通知 Redis backend。

Webhook 先停止接收，在**单一两秒总预算**或 caller 更早截止时间内尽力 drain，然后
取消通知 HTTP/等待并丢弃剩余工作。不保证全部投完，也不限制整个 App/systemd 两秒退出。
SIGKILL 跳过 drain。

## HTTP 与配置安全

公网目标要求 HTTPS，正常验证证书/主机名。私网/回环 HTTPS 还要求显式匹配的
`allowed_cidrs`。本地 HTTP 测试时将此例外合并到 `webhooks`，并将订阅 URL 改为
`http://127.0.0.1:18096/events`：

```yaml
allow_http: true
allowed_cidrs: ["127.0.0.1/32"]
```

HTTP 必须同时显式开启并匹配私网/回环 CIDR，公网 HTTP 仍被阻止。远端 Master 使用
接收端实际可达的精确网段。CIDR 不关闭 TLS，不放行 metadata、link-local、未指定、
组播或文档地址等被禁特殊地址。全部 DNS 结果均须符合策略，混合允许/禁止结果被拒绝。
新连接直接拨号到已检查 IP，已检查连接可复用。禁环境代理、重定向和自动 POST 重放。
拒绝 URL 用户凭据/fragment、未知键和错误类型。响应 body 读取 8 KiB、Header 16 KiB。

非法 webhook 配置只禁用通知，诊断脱敏 webhook 值。主 YAML 非法或其它模块仍沿用
原错误策略，不承诺独立修复通用配置日志。API 健康不代表 webhook 已开启。

敏感撤销前先停止全部旧投递实例，再应用新启动配置；混合配置期间仍可发给旧目标。
已开始请求无法撤回。重启丢弃旧内存积压，新订阅不接收历史事件。

## 排查与验证

| 指标 | 含义 |
| --- | --- |
| `cubemaster_webhook_operations_total{operation,result}` | 本地 capture/ingress、映射/编码、put/claim/complete/retry、send/HTTP/status 和终结结果 |
| `cubemaster_webhook_claim_age_seconds` | 领取时距目标任务创建的年龄直方图，含重试；不是执行时长 |
| `cubemaster_webhook_ingress_depth` | Writer 观察的本地缓冲深度，可能落后于接收；不是目标任务积压 |
| `cubemaster_webhook_inflight` | 本地活动 worker 任务，含发送前检查 |

常用计数对：`capture/oversized`、`ingress/full`、`event/unmatched`、`put/capacity`、
`http/attempt`、`http/success`、`terminal/failed`、`terminal/cancelled`、
`task/shutdown_discarded`、`ingress/unprocessed_shutdown`。HTTP attempt 计 Header
写出，不是 Claim 或接收端确认处理次数。有限诊断标签不是持久历史，无共享 Redis backlog 指标。

漏通知先查两级启动开关、精确过滤、真实业务成功点、capture/ingress 丢弃、mapping/put
结果，再查接收端 HTTP 状态、超时与地址/TLS 策略。确认是否重启或预算耗尽。
不要放宽 TLS 或改业务来获得通知，真实操作见[沙箱生命周期](./lifecycle.md)。

仓库[webhook 测试运行说明](https://github.com/TencentCloud/CubeSandbox/blob/master/tests/e2e/webhook/README.md)
提供有界本地 receiver、环境示例、组件检查和真实 SDK 六事件命令。
[SDK 测试 README](https://github.com/TencentCloud/CubeSandbox/blob/master/tests/e2e/sdk_compat/README.md)
说明现有 requirements 和平台 preflight。使用已有匹配工具链。Receiver/framework 自测和
collection 不证明部署中的 Master/Cubelet、XFS/S3、TLS 或故障场景通过。只运行已准备的
适用场景，保留初次失败，区分跳过、仅收集和未运行。文档检查不是生命周期验收。

## 迁移早期 PR 试用配置

删除 `store.backend`、`store.redis`、`lease`、旧 store 连接池键、`operation_timeout`
和 `terminal_retention`，通知 store 块只保留 `store.max_records`。废弃键通过严格校验
禁用通知，不静默选择内存或保留耐久性。无自动 Redis→memory backlog 迁移或重放。
核对两级开关、订阅和容量后重启所有 Master，平台业务 Redis 配置原样保留。
