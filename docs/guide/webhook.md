# Webhook 事件通知

## 语义声明(先读这段)

**成功写入事件流且未被裁剪的事件,保证至少投递一次(at-least-once);入流失败
或被 Redis 裁剪属于已声明的丢失窗口**,通过 XADD 失败计数与 consumer lag
告警暴露。同一投递行的所有重试使用同一个 `X-Cube-Delivery`,接收方应据此
幂等;平台不保证同一 sandbox 的事件顺序,需要强顺序的业务由接收方按
`timestamp`/`occurred_at` 自行处理。2xx 即视为投递成功,平台不做端到端业务
确认。

前置条件:启用 webhook 要求 **Redis ≥ 7**(lag 计算依赖 `XINFO GROUPS` 的
lag 字段;低于 7 时 lag 告警降级为 PEL 深度/XLEN 近似)。

## 开启与配置

webhook 默认关闭。开启方式(任一):

```bash
# 环境变量
export REDIS_URL="redis://:pass@127.0.0.1:6379/0"
export CUBE_OPS_WEBHOOK_ENABLED=true
```

或 `CubeOps/config.example.yaml` 的 `webhook:` 段(全部字段都有 env 覆盖,
见文件内注释)。关键配置:

| 配置 | 默认 | 说明 |
| --- | --- | --- |
| `consumer_group` | `cube-webhook` | 不能与 CLM 的 `cube-proxy-sidecar` 相同 |
| `keep_pending_max_retry_window` | `168h` | keep-pending 模式下失败重试总窗口,超时转 `dead`;`0` = 无限重试(必须配积压告警 + 人工处置 SOP) |
| `dead_letter_mode` | `keep-pending` | `dead-letter` 模式在 `max_attempts` 耗尽后转 `dead` |
| `worker_concurrency` / `per_subscription_concurrency` | 8 / 2 | 每副本并发;多副本总并发 = 副本数 × 值 |
| `allow_private_networks` | `false` | 仅本地联调用,放行 loopback/RFC1918,切勿在生产开启 |

## 订阅管理 API

所有接口挂在 JWT 鉴权组下,`/api/v1/webhooks`。任意已认证用户可管理全部订阅
(本期无所有权/RBAC,安全评审已确认)。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | `/api/v1/webhooks` | 创建订阅,201 |
| GET | `/api/v1/webhooks` | 列表(默认排除已删除),分页 `limit`/`offset`(默认 50,上限 200) |
| GET | `/api/v1/webhooks/:id` | 详情;**已删除订阅返回 200 且含 `deleted_at`——"已删除"= 只读可查(含历史投递归属),不可再 PUT/test/DELETE,请勿把 200 理解为订阅仍可用** |
| PUT | `/api/v1/webhooks/:id` | 部分更新;对已删除订阅返回 404 |
| DELETE | `/api/v1/webhooks/:id` | 软删除(置 `deleted_at` 并改名释放 name),204;重复 DELETE 返回 404(软删后该 id 即不存在于可操作集合) |
| POST | `/api/v1/webhooks/:id/test` | 落一条测试投递,返回 `{delivery_id}`;全局关闭 503、停用 409、已删除 404;每订阅 60s 限 5 次(进程内存,多副本 = 5×副本数) |
| GET | `/api/v1/webhooks/:id/deliveries` | 投递记录,支持 `status`、`event_id_prefix`、`limit`/`offset` |

创建/更新请求体:

```json
{
  "name": "business-system",
  "url": "https://example.com/cubesandbox/events",
  "events": ["sandbox.created", "sandbox.deleted", "sandbox.paused", "sandbox.resumed"],
  "secret": "optional-hmac-secret",
  "enabled": true
}
```

规则:`events` 必须非空且属于白名单;`url` 仅 `http`/`https` 且禁止 userinfo;
`secret` 可选、不设最低长度,密文落库、查询不回显(只返回 `secret_set`);
PUT 时 `secret` 缺省保留旧值、空字符串清除签名。**删除后再建同名订阅会得到新
id,旧 id 下的历史 delivery 仍可通过 `GET /:id/deliveries` 查询(需要知道旧
id);列表不显示已删除订阅。**

## 事件类型与 Payload

`events` 白名单:`sandbox.created` / `sandbox.deleted` / `sandbox.paused` /
`sandbox.resumed`。公共 payload 字段按事件类型区分(不是全字段示例):

| 事件 | 必含 | 可选 |
| --- | --- | --- |
| sandbox.created | `event`/`timestamp`/`sandbox_id` | `template_id`、`instance_type`、`host_id` |
| sandbox.deleted | 同上 | `template_id`、`reason` |
| sandbox.paused / resumed | 同上 | `source`、`template_id` |

`source` 仅来自 state 事件(`api`/`auto_pause`/`auto_resume`);`reason` 仅来自
delete 事件(`request`/`timeout`/`orphaned` 及扩展值,原样透传)。

## 请求头与验签

```http
POST {url}
Content-Type: application/json
X-Cube-Event-ID: {event_id}
X-Cube-Delivery: {event_id}:{subscription_id}
X-Cube-Timestamp: {unix_ms}
X-Cube-Signature-256: {hex}   ← 仅当订阅配置了 secret
```

验签:`hex(HMAC-SHA256(secret, 原始请求体字节))` 与 `X-Cube-Signature-256`
恒定时间比较;再校验 `X-Cube-Timestamp` 偏差(建议 ±5 分钟,可配);最后按
`X-Cube-Delivery` 幂等去重(重试复用同一 delivery id,不生成新 id)。完整示例
见 `examples/webhook-receiver`。

> **安全语义注意**:签名只覆盖请求体,`X-Cube-Timestamp` 等请求头**不参与签名**,
> 因此时间戳偏差检查只能过滤过期请求,本身**不能防重放**——截获过 (body,
> signature) 的攻击者可以随意改写未签名的头。真正的防重放依赖按
> `X-Cube-Delivery`(或 body 内已签名的 `event_id`)做幂等去重,接收方**必须**
> 实现去重,不要只依赖时间戳窗口。

## 重试与死信

- 可重试:408 / 429 / 5xx / 网络错误 / 超时 / 重定向,`attempts+1`,指数退避
  (base 1s、封顶 10 分钟、±500ms jitter 内)。
- 永久失败:其他 4xx、secret 解密失败、SSRF 拒绝 → `permanent_failed`,保留
  `last_error`/`http_status` 供排查。
- `dead-letter` 模式:`max_attempts`(默认 5)耗尽 → `dead`。
- `keep-pending` 模式(默认):持续重试,但受 `keep_pending_max_retry_window`
  兜底(默认 7 天),到期转 `dead`;**`0` = 无限重试,必须配积压告警与人工处置
  SOP**。
- 投递延迟、HTTP 状态分布、SQL 积压按 status、dead 堆积、lease 争抢率、
  worker 饱和、Redis lag/PEL、SSRF/重定向/解密拒绝计数等指标在
  `GET /metrics` 暴露(per-subscription 序列有基数上限)。

## 手动重放

对 `permanent_failed`/`dead` 行,可用 SQL 重放(重放后 `first_failed_at`
清空,不会立即再转 dead):

```sql
UPDATE t_webhook_delivery
SET status='pending', attempts=0, first_failed_at=NULL, next_retry_at=now(),
    lease_owner=NULL, lease_until=NULL, http_status=NULL, last_error=NULL
WHERE id=? AND status IN ('permanent_failed','dead');

-- 若该事件曾触发物化失败隔离,重放成功后清理失败记录(可选,90 天保留策略也会清理)
DELETE FROM t_webhook_materialization_failure WHERE event_id = ?;
```

## 安全限制

- SSRF:发送前单次解析 DNS 并对每个地址做 CIDR 校验(loopback/私网/metadata/
  多播/文档保留段/IPv4-mapped IPv6 全部覆盖),任一地址违规即拒绝
  (fail-closed);`allow_private_networks=true` 仅用于本地联调。
- 不跟随 HTTP 重定向;响应体读取上限 1 MiB。
- 日志不输出 secret、签名值、完整敏感响应体。
- 生产端点建议在公网入口再叠加 IP 白名单/网关鉴权(本期仅 HMAC + 时间戳)。

## 告警参考(企业微信 / 通用 HTTP)

把 `docs/guide` 提到的 Prometheus 指标接到告警规则,再经
`examples/webhook-receiver` 的 `WECOM_WEBHOOK_URL` 转发到企业微信群机器人,
或直接用任意 HTTP 告警通道订阅告警 webhook。推荐至少告警:投递延迟 P95、
`failed`/`dead` 堆积、XADD 失败计数、consumer lag 超过 `EventStreamMaxLen`
的 80%、worker 饱和率、`keep_pending_max_retry_window=0` 时的积压水位。

## 本地 30 分钟快速验证

```bash
# 1. 启动依赖(MySQL、Redis;已有 one-click 环境可复用)
# 2. 启动 CubeOps(webhook.enabled=true,allow_private_networks=true 仅本地)
# 3. 启动接收端
cd examples/webhook-receiver && WEBHOOK_SECRET=test-secret cargo run
# 4. 注册订阅 + 发一条测试投递
TOKEN=$(curl -s -X POST http://127.0.0.1:3010/api/v1/login -d '{"username":"admin","password":"..."}' | jq -r .access_token)
curl -s -X POST http://127.0.0.1:3010/api/v1/webhooks \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"local","url":"http://127.0.0.1:9090/webhook","events":["sandbox.created"],"secret":"test-secret"}'
curl -s -X POST http://127.0.0.1:3010/api/v1/webhooks/1/test \
  -H "Authorization: Bearer $TOKEN"
# 5. 接收端应打印一条 sandbox.created,并可通过
#    GET /api/v1/webhooks/1/deliveries?event_id_prefix=test: 查看投递状态
```

一键脚本(自动检查/启动依赖并输出 PASS/FAIL):`scripts/webhook-local-smoke.sh`
(脚本内显式设置 `allow_private_networks=true` 并输出 WARNING,勿在生产复用)。
