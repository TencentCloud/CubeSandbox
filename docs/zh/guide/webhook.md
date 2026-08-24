# Webhook 事件通知

CubeSandbox 可以把沙箱的生命周期事件（创建、删除、暂停、恢复）以 HTTP POST 推送到你指定的回调地址。典型用途：业务系统在沙箱创建后执行初始化、在沙箱删除后回收外部资源，或者以事件流的方式对沙箱生命周期做审计和统计。

推送由 CubeOps 内置的投递组件完成：它消费平台内部的生命周期事件流，把每个事件按订阅写入投递账本，再逐条推送到各订阅端点，失败自动重试。该组件默认关闭，开启方式见[开启与配置](#开启与配置)。

本文涉及的职责分两层，实际使用中常由同一个人或同一团队承担：

- **事件消费方（业务系统侧）**：订阅沙箱事件，在自己现有的业务系统里处理回调，完成 CubeSandbox 与业务的集成。从[快速开始](#快速开始)入手，然后阅读[事件类型与 Payload](#事件类型与-payload)和[请求头与验签](#请求头与验签)，即可完成对接。
- **平台运维**：负责开启、配置和监控投递组件。重点阅读[开启与配置](#开启与配置)、[投递语义](#投递语义)、[监控与告警](#监控与告警)和[手动重放](#手动重放)。

## 快速开始

目标：30 分钟内完成"开启投递 → 注册订阅 → 收到第一条事件"的端到端验证。以下步骤均在一套已部署的 CubeSandbox 环境（如一键安装）上执行，只需额外保证两点：

- Redis 版本 ≥ 7，且 CubeOps 可以访问（复用平台 Redis 即可）；
- 拥有 CubeOps 的管理员账号（默认 `admin` / `admin`，正式环境请先修改密码）。

### 第 1 步：开启投递组件

投递组件由 CubeOps 进程**启动时**的环境变量控制，默认关闭；订阅本身在第 3 步通过 REST API 注册（`POST /api/v1/webhooks`），两者是不同层面的配置：环境变量管「组件是否开启」，REST 管「往哪个端点推、推哪些事件」。

以 one-click / systemd 部署为例，CubeOps 服务通过 `EnvironmentFile` 读取 `/usr/local/services/cubetoolbox/.one-click.env`，追加以下变量并重启服务：

```bash
cat >> /usr/local/services/cubetoolbox/.one-click.env <<'EOF'
# 平台共用的 Redis（一键安装默认密码 ceuhvu123，见下方 tip）
REDIS_URL=redis://:ceuhvu123@127.0.0.1:6379/0
# 开启投递组件
CUBE_OPS_WEBHOOK_ENABLED=true
# 仅本地联调，生产保持 false（见下方 warning）
CUBE_OPS_WEBHOOK_ALLOW_PRIVATE_NETWORKS=true
EOF

# 重启服务使配置生效
systemctl restart cube-sandbox-cubeops
```

> 非 systemd 部署（直接运行 CubeOps 二进制）：在启动进程前 `export` 同样的三个变量再启动即可，无需改环境文件。

验证：

```bash
curl -s http://127.0.0.1:3010/webhook/healthz
# 期望 {"webhook":"ready"}；未开启时为 {"webhook":"disabled"}
```

::: tip 关于 REDIS_URL 的密码
一键安装部署的 Redis 默认开启 requirepass（默认密码 `ceuhvu123`，安装时可用 `CUBE_SANDBOX_REDIS_PASSWORD` / `CUBE_EXTERNAL_REDIS_PASSWORD` 覆盖）。若不确定实际密码，用 `docker inspect cube-sandbox-redis` 查看容器启动命令（`Cmd` 字段）即可看到 `--requirepass` 后的值；连接串格式为 `redis://[:密码]@主机:端口/库`，只有自建且未开启 requirepass 的 Redis 才能省略密码。
:::

::: warning 关于 allow_private_networks
出于 SSRF 防护，投递组件默认拒绝向 loopback 和 RFC1918 私网地址推送。本例的接收端运行在本机 `127.0.0.1`，因此临时打开了该开关。**生产环境必须保持 `false`**，端点应为公网或集群内可路由地址。
:::

### 第 2 步：启动示例接收端

本步是给「接收方开发者」的参考实现：用示例接收端代替你将要接入的业务服务，先验证事件能带正确签名送达。如果你已有自己的接收端点，可直接跳到第 3 步，订阅时填你的 URL。

仓库自带一个最小接收端 `examples/webhook-receiver`（Rust），能够验签、校验时间戳、按投递 ID 去重，并把事件打印到终端。它只需能被 CubeOps 网络访问即可，不必与 CubeOps 同机。

在开发机上执行（未克隆请先克隆仓库）：

```bash
git clone https://github.com/tencentcloud/CubeSandbox.git
cd CubeSandbox/examples/webhook-receiver
WEBHOOK_SECRET=my-test-secret PORT=9095 cargo run
# webhook-receiver listening on http://127.0.0.1:9095
```

> 接收端默认监听 `127.0.0.1:9090`，但 one-click 部署的 cube-egress（沙箱出口代理）已占用 9090（cube-proxy 的 gRPC 也默认 9090），因此文档统一用 `PORT=9095`，并把第 3 步订阅 URL 的端口保持一致。若改用其他端口，两处需同步修改。

没有源码也没有 Rust 环境时（例如直接在部署服务器上验证，需已安装 python3），用 python3 起一个最小接收端即可：

```bash
cat > /tmp/webhook-recv.py <<'PY'
import hmac, hashlib
from http.server import BaseHTTPRequestHandler, HTTPServer
class H(BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get('Content-Length', 0)))
        sig = (self.headers.get('X-Cube-Signature-256') or '').strip()
        if sig.startswith('sha256='):
            sig = sig[len('sha256='):]
        ok = hmac.compare_digest(sig, hmac.new(b'my-test-secret', body, hashlib.sha256).hexdigest())
        print('sig_ok=%s event_id=%s body=%s' % (ok, self.headers.get('X-Cube-Event-ID'), body.decode('utf-8', 'replace')), flush=True)
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b'ok')
    def log_message(self, *a):
        pass
HTTPServer(('127.0.0.1', 9095), H).serve_forever()
PY
python3 /tmp/webhook-recv.py
# webhook-receiver listening on http://127.0.0.1:9095
```

> 无论用哪种方式，接收端的 SECRET 必须与第 3 步订阅里的 `secret` 一致（本示例统一为 `my-test-secret`）。

### 第 3 步：登录 CubeOps 并创建订阅

```bash
# 登录取 JWT（默认账号 admin/admin；返回字段为驼峰的 accessToken）
TOKEN=$(curl -s -X POST http://127.0.0.1:3010/api/v1/auth/login \
  -H "Content-Type: application/json" \
  -d '{"username":"admin","password":"admin"}' \
  | python3 -c "import sys,json;print(json.load(sys.stdin)['accessToken'])")

# 创建订阅：接收 sandbox.created 事件，推送到本机接收端
curl -s -X POST http://127.0.0.1:3010/api/v1/webhooks \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "local-test",
    "url": "http://127.0.0.1:9095/webhook",
    "events": ["sandbox.created"],
    "secret": "my-test-secret"
  }'
```

创建成功返回 201，响应中的 `id` 即订阅 ID（下文假设为 `1`）。`secret` 必须与接收端启动时的 `WEBHOOK_SECRET` 一致，否则验签会失败。

### 第 4 步：发一条测试投递

```bash
curl -s -X POST http://127.0.0.1:3010/api/v1/webhooks/1/test \
  -H "Authorization: Bearer $TOKEN"
# {"delivery_id":1}
```

接收端终端应立即打印一条 `sandbox.created` 事件（`sandbox_id` 为 `test-sandbox`）。如果打印 `REJECTED (signature mismatch)`，说明订阅的 `secret` 与接收端不一致。

### 第 5 步：触发真实事件

创建一个沙箱，接收端会收到一条真实的 `sandbox.created`，payload 中带有该沙箱的 `sandbox_id` 和模板信息。下面给出最直接的 API 方式（E2B 兼容接口，端口 `3000`）；也可按[快速开始](./quickstart.md)用 Python SDK 创建。

先确认有一个 `READY` 的模板（快速开始第 3 步会创建，模板 ID 形如 `tpl-xxx`；也可用 `cubemastercli tpl list` 查看），然后：

```bash
curl -s -X POST http://127.0.0.1:3000/sandboxes \
  -H "Content-Type: application/json" -H "X-API-Key: e2b_000000" \
  -d '{"templateID":"tpl-<你的模板ID>"}'
# 返回示例：
# {"templateID":"tpl-xxx","sandboxID":"...","clientID":"...","envdVersion":"...","domain":"cube.app"}
```

> 注意请求体字段是驼峰 `templateID`（不是 `template`）。创建成功后，接收端 log 会多出一条 `event_id` 为纯数字（Redis Stream ID）的真实 `sandbox.created`。

### 第 6 步：核对投递记录

```bash
curl -s "http://127.0.0.1:3010/api/v1/webhooks/1/deliveries?limit=20" \
  -H "Authorization: Bearer $TOKEN" | python3 -m json.tool
```

每条投递记录包含 `status`（`succeeded` / `failed` / …）、`attempts`、`http_status`、`last_error` 等字段，是排查"接收端没收到"问题的第一入口。

::: tip 一键冒烟脚本
`scripts/webhook-local-smoke.sh` 把上述流程自动化（自动检查依赖、注册订阅、发送测试投递并输出 PASS/FAIL）。脚本会显式设置 `allow_private_networks=true` 并打印警告，仅限本地使用。
:::

## 事件类型与 Payload

订阅时必须显式声明接收哪些事件，可选值为：

| 事件 | 触发时机 |
| --- | --- |
| `sandbox.created` | 沙箱创建成功 |
| `sandbox.deleted` | 沙箱删除完成 |
| `sandbox.paused` | 沙箱暂停完成 |
| `sandbox.resumed` | 沙箱恢复运行 |

Payload 为 UTF-8 JSON，公共字段固定，可选字段按事件类型出现（字段缺失时应容忍未知字段，便于未来扩展）：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `schema_version` | string | payload 结构版本，当前为 `1` |
| `event_id` | string | 事件唯一 ID（Redis 流 ID 或 `test:<uuid>`），同一事件的所有投递共享 |
| `event` | string | 事件类型，见上表 |
| `timestamp` | number | 事件发生时间，Unix 毫秒 |
| `occurred_at` | string | 事件发生时间，RFC3339 格式，与 `timestamp` 同源 |
| `sandbox_id` | string | 沙箱 ID |
| `template_id` | string | 可选；创建沙箱所用模板，元数据缺失时省略 |
| `source` | string | 可选；仅暂停/恢复事件，取值 `api` / `auto_pause` / `auto_resume`，区分用户操作与平台自动暂停恢复 |
| `reason` | string | 可选；仅删除事件，取值 `request` / `timeout` / `orphaned` 等，原样透传删除原因 |

`sandbox.created` 的完整示例：

```json
{
  "schema_version": "1",
  "event_id": "1789312345678-0",
  "event": "sandbox.created",
  "timestamp": 1789312345678,
  "occurred_at": "2026-08-18T09:25:45Z",
  "sandbox_id": "sbx-abc123",
  "template_id": "tpl-xyz"
}
```

## 请求头与验签

每次推送都是一个 HTTP POST，请求头如下：

```http
POST {url}
Content-Type: application/json
X-Cube-Event-ID: {event_id}
X-Cube-Delivery: {event_id}:{subscription_id}
X-Cube-Timestamp: {unix_ms}
X-Cube-Signature-256: {hex}   ← 仅当订阅配置了 secret
```

接收方建议按以下顺序校验：

1. 用订阅 secret 计算 `hex(HMAC-SHA256(secret, 原始请求体字节))`，与 `X-Cube-Signature-256` 恒定时间比较；
2. 校验 `X-Cube-Timestamp` 与当前时间的偏差（建议 ±5 分钟，可自行放宽或收紧）；
3. 按 `X-Cube-Delivery` 做幂等去重——同一投递的所有重试复用同一个 delivery ID，不生成新 ID。

`examples/webhook-receiver` 是以上三步的参考实现，可直接复用其校验代码。

::: warning 时间戳检查不能防重放
签名只覆盖请求体，`X-Cube-Timestamp` 等请求头不参与签名。截获过请求的攻击者可以随意改写这些未签名的头，因此时间戳偏差检查只能过滤过期请求，不能防止重放。真正的防重放依赖第 3 步的幂等去重（`X-Cube-Delivery` 或请求体内已签名的 `event_id`），接收方必须实现去重，不要只依赖时间戳窗口。生产端点建议在入口再叠加 IP 白名单或网关鉴权。
:::

## 订阅管理 API

所有接口挂在 JWT 鉴权之下，前缀 `/api/v1/webhooks`。当前版本任意已认证用户可管理全部订阅（暂无按所有者的权限隔离）。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | `/api/v1/webhooks` | 创建订阅，返回 201 |
| GET | `/api/v1/webhooks` | 订阅列表（不含已删除），分页 `limit` / `offset`，默认 50、上限 200 |
| GET | `/api/v1/webhooks/:id` | 订阅详情；已删除订阅返回 200 且带 `deleted_at` 字段，表示只读可查，不可再操作 |
| PUT | `/api/v1/webhooks/:id` | 部分更新；对已删除订阅返回 404 |
| DELETE | `/api/v1/webhooks/:id` | 软删除，返回 204；重复 DELETE 返回 404 |
| POST | `/api/v1/webhooks/:id/test` | 落一条测试投递并返回 `{"delivery_id":...}`；全局关闭时 503、订阅停用时 409、已删除时 404；每订阅每分钟限 5 次（按进程计，多副本为 5 × 副本数） |
| GET | `/api/v1/webhooks/:id/deliveries` | 投递记录查询，支持 `status`、`event_id_prefix`、`limit` / `offset` |

创建请求体示例：

```json
{
  "name": "business-system",
  "url": "https://example.com/cubesandbox/events",
  "events": ["sandbox.created", "sandbox.deleted"],
  "secret": "optional-hmac-secret",
  "enabled": true
}
```

字段规则：

- `name` 全局唯一，最长 128 字符；软删除会释放该名字，同名重建会得到新的订阅 ID；
- `events` 必须非空且全部属于上表的白名单；
- `url` 仅允许 `http` / `https`，禁止携带 userinfo；
- `secret` 可选。密文存储，任何查询接口都不会回显明文。PUT 时缺省表示保留旧值，显式传空字符串表示清除签名（之后推送不再带签名头）；
- 删除订阅不会删除历史投递记录，仍可通过旧订阅 ID 的 `GET /:id/deliveries` 查询。

## 投递语义

理解以下语义有助于正确实现接收端：

- **至少一次（at-least-once）**：成功写入平台事件流的事件保证至少投递一次；写入失败或事件流被 Redis 按长度裁剪属于已声明的丢失窗口（通过 XADD 失败计数与消费积压告警暴露，见[监控与告警](#监控与告警)）。因此接收端可能收到重复推送，必须按 `X-Cube-Delivery` 幂等去重。
- **不保证顺序**：同一沙箱的多个事件不保证按发生顺序送达。需要强顺序的业务应按 payload 中的 `timestamp` / `occurred_at` 自行判断与丢弃过期事件。
- **2xx 即成功**：接收端返回任何 2xx 状态码即视为投递成功，平台不做端到端业务确认。业务侧的处理结果应通过自身的机制保证，不要依赖推送重试。

### 重试与死信

- 可重试的失败：HTTP 408 / 429 / 5xx、网络错误、超时、重定向。每次重试 `attempts` 加一，指数退避（基准 1 秒、封顶 10 分钟、带随机抖动）。
- 永久失败：其他 4xx（接收端明确拒绝）、secret 解密失败、SSRF 地址策略拒绝。直接置为 `permanent_failed`，保留 `last_error` / `http_status` 供排查，不再重试。
- 两种兜底模式（`dead_letter_mode` 配置）：
  - `keep-pending`（默认）：持续重试，但受 `keep_pending_max_retry_window`（默认 7 天）限制，超期转 `dead`。设为 `0` 表示无限重试（只能通过环境变量 `CUBE_OPS_WEBHOOK_KEEP_PENDING_MAX_RETRY_WINDOW=0` 设置，YAML 里无法表达裸 `0`），此时必须配置积压告警与人工处置流程。
  - `dead-letter`：`max_attempts`（默认 5）耗尽后转 `dead`。

## 开启与配置

除本节开头的两个环境变量外，完整配置见 `CubeOps/config.example.yaml` 的 `webhook:` 段，每个字段都有对应的 `CUBE_OPS_WEBHOOK_*` 环境变量覆盖。常用项：

| 配置 | 默认 | 说明 |
| --- | --- | --- |
| `enabled` | `false` | 总开关；开启需要同时配置 `redis_url` |
| `consumer_group` | `cube-webhook` | Redis 消费组名，不能与平台内组件（`cube-proxy-sidecar`）冲突 |
| `worker_concurrency` / `per_subscription_concurrency` | 8 / 2 | 每副本的发送并发：总量与单订阅上限。多副本时总并发为副本数乘以该值 |
| `http_timeout` | `10s` | 单次推送的超时时间 |
| `max_attempts` | `5` | `dead-letter` 模式下的最大尝试次数 |
| `keep_pending_max_retry_window` | `168h` | `keep-pending` 模式的重试总窗口；`0` = 无限重试（须配告警），**仅能通过环境变量 `CUBE_OPS_WEBHOOK_KEEP_PENDING_MAX_RETRY_WINDOW=0` 设置**，YAML 写裸 `0` 会解析失败、写 `"0s"` 会被默认值覆盖 |
| `dead_letter_mode` | `keep-pending` | 兜底模式，见上文 |
| `allow_private_networks` | `false` | 放行 loopback / RFC1918 地址，仅本地联调用，生产必须关闭 |
| `cleanup.*` | 30 天 / 90 天 / 24h | 终态投递记录的保留时长与清理周期 |

前置条件：Redis ≥ 7（消费组 lag 告警依赖 `XINFO GROUPS` 的 lag 字段；低版本时该告警退化为近似值）。

## 监控与告警

CubeOps 的 `GET /metrics`（默认端口 3010）暴露投递组件的全部指标，关键项：

| 指标 | 说明 |
| --- | --- |
| `cubeops_webhook_delivery_result_total` | 投递结果计数（succeeded / retryable / permanent / shutdown） |
| `cubeops_webhook_delivery_duration_seconds` | 推送 HTTP 耗时分布 |
| `cubeops_webhook_backlog_rows` | 按状态统计的可行动积压（pending / retryable failed） |
| `cubeops_webhook_keep_pending_dead_total` | 因窗口耗尽转 dead 的行数 |
| `cubeops_webhook_lease_contention_total` | 多副本 lease 争抢次数 |

CubeMaster 侧另有 `cubemaster_lifecycle_xadd_failures_total`（事件写入事件流失败计数）。建议至少对以下情况配置告警：投递延迟 P95、`failed` / `dead` 积压增长、XADD 失败计数非零、消费组 lag 持续超过阈值、`keep_pending_max_retry_window=0` 时的积压水位。

## 手动重放

对 `permanent_failed` 或 `dead` 的投递行，修复接收端问题后可用 SQL 重放（重放会清空 `first_failed_at`，不会立即再转 dead）：

```sql
UPDATE t_webhook_delivery
SET status='pending', attempts=0, first_failed_at=NULL, next_retry_at=now(),
    lease_owner=NULL, lease_until=NULL, http_status=NULL, last_error=NULL
WHERE id=? AND status IN ('permanent_failed','dead');

-- 若该事件曾触发物化失败隔离，可选清理失败记录（90 天保留策略也会自动清理）
DELETE FROM t_webhook_materialization_failure WHERE event_id = ?;
```

## 安全限制

- SSRF 防护：推送前对回调域名做单次 DNS 解析，并对每个解析地址做 CIDR 校验（loopback、RFC1918、链路本地含云 metadata 地址、CGNAT、多播、保留段、IPv4-mapped IPv6 均覆盖），任一地址违规即整体拒绝（fail-closed）。
- 不跟随 HTTP 重定向（3xx 按可重试失败处理）；响应体读取上限 1 MiB。
- secret 加密存储、日志不输出 secret 与签名值。

## 故障排查

| 现象 | 排查方向 |
| --- | --- |
| 测试投递返回 503 | 投递组件未开启（`CUBE_OPS_WEBHOOK_ENABLED`），或 `redis_url` 未配置导致启动失败 |
| `/webhook/healthz` 返回 not_ready | 检查 Redis 连通性与版本（≥ 7），以及 CubeOps 日志中 webhook 相关报错 |
| 接收端完全收不到事件 | 依次核对：订阅 `enabled` 与 `events` 是否包含目标事件；`deliveries` 接口里是否生成了投递行——有行但 `failed` 说明推送环节问题（看 `last_error`），无行说明事件未匹配到订阅 |
| 一直重试不成功 | `last_error` / `http_status` 定位：连接类错误检查网络与 SSRF 策略（私网端点需走公网地址）；401/403 检查接收端验签实现 |
| 接收端报签名不匹配 | 订阅 `secret` 与接收端配置不一致；或接收端对 body 做了二次编码（必须用原始请求体字节计算 HMAC） |
| 事件延迟明显 | 查 `backlog_rows` 积压与消费组 lag；单订阅端点持续 5xx 会拖慢整体，可调低该订阅优先级或临时停用 |
| 想重放某条失败投递 | 见[手动重放](#手动重放) |

推送侧日志位于 CubeOps 日志目录（默认 `/data/log/CubeOps`），检索 `webhook` 关键字可看到每次推送的分类结果。
