# Webhook 事件通知

CubeAPI 支持将沙箱生命周期事件推送到外部 HTTP 端点。当前覆盖沙箱创建、超时更新、续期、暂停、恢复和删除六类事件，可用于实时监控、告警平台对接、沙箱使用统计等场景。

如果需要对接企业微信等只接受固定消息格式的工具，需要在 CubeAPI 与第三方 webhook 之间增加适配服务，见下方[企业微信对接](#企业微信对接)。

## 快速跑通

> 本节假设你已经通过[快速开始](./quickstart.md)或[PVM 部署](./pvm-deploy.md)完成了 CubeSandbox 部署。不熟悉 systemd 服务管理的话，可以先看[服务管理与日志](./service-management.md)。

### 启动 example receiver

`CubeSandbox` 仓库的 `examples/` 中提供了一个基于 Python 标准库的 webhook 接收端，无需额外安装依赖：

```bash
python3 examples/webhook-receiver/receiver.py
```

receiver 默认监听 `:18080`。如需调整端口或启用 HMAC 验签，可以通过环境变量配置：

| 环境变量         | 说明                            |
| ---------------- | ------------------------------- |
| `WEBHOOK_PORT`   | receiver 监听端口，默认 `18080` |
| `WEBHOOK_SECRET` | HMAC 验签密钥；为空时不校验签名 |

先用 curl 发一条测试事件，确认 receiver 正常工作：

```bash
curl -X POST http://localhost:18080/webhook \
  -H "Content-Type: application/json" \
  -d '{"events":[{"timestamp":"2026-01-01T12:00:00Z","level":"info","event":"sandbox.created","sandbox_id":"test-1","template_id":"tmpl-1"}]}'
```

receiver 会输出类似内容：

```text
[<receive-time>] received 1 event(s)
  [2026-01-01T12:00:00Z] sandbox.created
    sandbox_id : test-1
    template_id: tmpl-1
```

第一行是 receiver 实际收到请求的时间，事件行中的 timestamp 来自 webhook payload。

### 配置 CubeAPI

::: tip `.one-click.env` 的位置
one-click 安装后 `.one-click.env` 位于：

```text
/usr/local/services/cubetoolbox/.one-click.env
```

:::

编辑 `.one-click.env`，在末尾追加 webhook 配置：

```ini
CUBE_API_WEBHOOK_ENABLED=true
CUBE_API_WEBHOOK_URL=http://127.0.0.1:18080/webhook
CUBE_API_WEBHOOK_EVENTS=sandbox.*
```

如果 receiver 与 CubeAPI 不在同一台机器上，需要将 `127.0.0.1` 替换为 CubeAPI 能访问到的 receiver 地址。

重启 CubeAPI：

```bash
systemctl restart cube-sandbox-cube-api.service
```

查看启动日志，确认 webhook 已加载：

```bash
journalctl -u cube-sandbox-cube-api.service -n 20 --no-pager
```

日志中应包含类似内容：

```text
INFO cube_api: cube-api starting ... webhook_enabled=true webhook_endpoint_count=1
INFO cube_api: webhook endpoint configured url=http://127.0.0.1:18080/webhook
```

以上是 systemd + 环境变量的最简方式，一次只能注册一个 endpoint。如需多 endpoint、配置文件、HMAC 签名或启动参数等完整选项，见下方[配置方式](#配置方式)。

### 触发事件

依次创建、暂停、恢复、删除一个沙箱：

```bash
# 创建沙箱
curl -X POST http://localhost:3000/sandboxes \
  -H "Content-Type: application/json" \
  -d '{"templateID": "<模板 ID>"}'
# 记录返回的 sandbox_id

# 暂停沙箱
curl -X POST http://localhost:3000/sandboxes/<sandbox_id>/pause

# 恢复沙箱
curl -X POST http://localhost:3000/sandboxes/<sandbox_id>/resume \
  -H "Content-Type: application/json" \
  -d '{}'

# 删除沙箱
curl -X DELETE http://localhost:3000/sandboxes/<sandbox_id>
```

每一步操作成功后，receiver 终端应打印对应事件。例如：

```text
[<receive-time>] received 1 event(s)
  [2026-01-01T12:00:00Z] sandbox.created
    sandbox_id : your-sandbox-id
    template_id: your-template-id

[<receive-time>] received 1 event(s)
  [2026-01-01T12:00:05Z] sandbox.timeout.updated
    sandbox_id : your-sandbox-id
    timeout: 300

[<receive-time>] received 1 event(s)
  [2026-01-01T12:00:10Z] sandbox.refreshed
    sandbox_id : your-sandbox-id
    duration: 600

[<receive-time>] received 1 event(s)
  [2026-01-01T12:00:15Z] sandbox.paused
    sandbox_id : your-sandbox-id

[<receive-time>] received 1 event(s)
  [2026-01-01T12:00:20Z] sandbox.resumed
    sandbox_id : your-sandbox-id

[<receive-time>] received 1 event(s)
  [2026-01-01T12:00:25Z] sandbox.deleted
    sandbox_id : your-sandbox-id
```

至此，CubeAPI 到外部 HTTP receiver 的 webhook 投递链路已经跑通。更多配置方式见下方[配置方式](#配置方式)。

## 支持的事件

| 事件                       | 触发操作                                 | 携带字段                    |
| -------------------------- | ---------------------------------------- | --------------------------- |
| `sandbox.created`          | `POST /sandboxes` 成功                   | `sandbox_id`、`template_id` |
| `sandbox.timeout.updated`  | `POST /sandboxes/:id/timeout` 成功       | `sandbox_id`、`timeout`     |
| `sandbox.refreshed`        | `POST /sandboxes/:id/refreshes` 成功     | `sandbox_id`、`duration`    |
| `sandbox.paused`           | `POST /sandboxes/:id/pause` 成功         | `sandbox_id`                |
| `sandbox.resumed`          | `POST /sandboxes/:id/resume` 成功        | `sandbox_id`                |
| `sandbox.deleted`          | `DELETE /sandboxes/:id` 成功             | `sandbox_id`                |

### 事件订阅

每个 endpoint 可以配置自己关心的事件类型：

| 模式              | 匹配范围                          |
| ----------------- | --------------------------------- |
| `sandbox.created` | 精确匹配单个事件                  |
| `sandbox.*`       | 匹配所有沙箱生命周期事件，默认值  |
| `*`               | 匹配所有事件，包括 debug 级别事件 |

endpoint 未指定 `events` 时，默认匹配 `sandbox.*`。

`api.request` 是 CubeAPI 内部用于记录 HTTP handler 调用的 debug 事件，不属于沙箱生命周期事件。除非显式订阅 `*`，否则不会投递到 webhook endpoint。

## 配置方式

多种配置共存时按以下优先级合并：

```text
启动参数 > 环境变量 > 配置文件 > 默认值
```

其中，`CUBE_API_WEBHOOK_URL` 和 `--webhook-url` 表示单 endpoint 配置；一旦设置，会替换配置文件中的 `endpoints` 列表，而不是追加到列表中。

### systemd / one-click（推荐）

CubeAPI 以 systemd 服务运行时，推荐通过 `.one-click.env` 注入 webhook 配置。

::: tip `.one-click.env` 的位置
one-click 安装后 `.one-click.env` 位于：

```text
/usr/local/services/cubetoolbox/.one-click.env
```

:::

在文件末尾追加：

```ini
CUBE_API_WEBHOOK_ENABLED=true
CUBE_API_WEBHOOK_URL=http://receiver-host:18080/webhook
CUBE_API_WEBHOOK_EVENTS=sandbox.*
CUBE_API_WEBHOOK_SECRET=<your-hmac-secret>     # 可选，不设置则不签名
```

重启 CubeAPI 后生效：

```bash
systemctl restart cube-sandbox-cube-api.service
```

这种方式无需改动 systemd unit 文件，但一次只能注册一个 endpoint。如需同时向多个接收端推送事件，见下方[配置文件（多 endpoint）](#配置文件-多-endpoint)。

### 配置文件（多 endpoint）

需要向多个接收端推送事件，或为不同 endpoint 设置不同的 `hmac_secret` 和事件订阅时，可以使用配置文件：

```toml
enabled = true

# 投递参数（以下均为默认值，可按需调整）
batch_size = 100              # 每批最大事件数
flush_interval_secs = 5       # 最长刷新间隔，单位秒
max_retries = 3               # 首次失败后最多重试次数
retry_backoff_millis = 200    # 基础退避间隔，单位毫秒
request_timeout_secs = 5      # 单次请求超时，单位秒

# endpoint A：只接收 created / deleted
[[endpoints]]
url = "http://receiver-a:18080/webhook"
events = ["sandbox.created", "sandbox.deleted"]
hmac_secret = "secret-for-a"

# endpoint B：接收全部沙箱生命周期事件
[[endpoints]]
url = "http://receiver-b:18081/webhook"
events = ["sandbox.*"]
hmac_secret = "secret-for-b"
```

建议将配置文件放在 CubeAPI 安装目录下：

```text
/usr/local/services/cubetoolbox/CubeAPI/webhook.toml
```

然后在 `.one-click.env` 中指定配置文件路径：

```ini
CUBE_API_WEBHOOK_CONFIG=/usr/local/services/cubetoolbox/CubeAPI/webhook.toml
```

重启 CubeAPI 后生效：

```bash
systemctl restart cube-sandbox-cube-api.service
```

### 环境变量

如果直接通过 `./cube-api` 二进制启动，而不是通过 systemd 启动，可以在启动前 export 环境变量：

```bash
export CUBE_API_WEBHOOK_ENABLED=true
export CUBE_API_WEBHOOK_URL=http://receiver:18080/webhook
export CUBE_API_WEBHOOK_EVENTS=sandbox.*
export CUBE_API_WEBHOOK_SECRET=your-secret
./cube-api
```

可选的调参变量如下，不设置时使用默认值：

| 变量                                    | 默认值 | 说明                   |
| --------------------------------------- | ------ | ---------------------- |
| `CUBE_API_WEBHOOK_BATCH_SIZE`           | 100    | 每批最大事件数         |
| `CUBE_API_WEBHOOK_FLUSH_INTERVAL_SECS`  | 5      | 最长刷新间隔，单位秒   |
| `CUBE_API_WEBHOOK_MAX_RETRIES`          | 3      | 最大重试次数           |
| `CUBE_API_WEBHOOK_RETRY_BACKOFF_MILLIS` | 200    | 基础退避间隔，单位毫秒 |
| `CUBE_API_WEBHOOK_REQUEST_TIMEOUT_SECS` | 5      | 单次请求超时，单位秒   |

::: warning 注意
直接运行 `./cube-api` 只会启动 CubeAPI 进程本身。生产环境中，CubeAPI 依赖 cubemaster、cubelet 等后端服务才能完成完整的沙箱生命周期。日常部署建议优先使用 systemd 方式。
:::

### 启动参数

开发调试时，也可以通过启动参数快速验证 endpoint 连通性。启动参数优先级最高：

```bash
./cube-api \
  --webhook-url http://127.0.0.1:18080/webhook \
  --webhook-events sandbox.* \
  --webhook-secret your-secret
```

| 启动参数           | 对应环境变量              | 说明               |
| ------------------ | ------------------------- | ------------------ |
| `--webhook-config` | `CUBE_API_WEBHOOK_CONFIG` | 配置文件路径       |
| `--webhook-url`    | `CUBE_API_WEBHOOK_URL`    | 单 endpoint URL    |
| `--webhook-events` | `CUBE_API_WEBHOOK_EVENTS` | 逗号分隔的事件列表 |
| `--webhook-secret` | `CUBE_API_WEBHOOK_SECRET` | HMAC 密钥          |

## Payload

CubeAPI 向每个 endpoint 发送 `POST` 请求，`Content-Type` 为 `application/json`。

payload 以 batch 形式发送，即使当前批次中只有一个事件，也会放在 `events` 数组中：

```json
{
  "events": [
    {
      "timestamp": "2026-01-01T12:00:00Z",
      "level": "info",
      "event": "sandbox.created",
      "sandbox_id": "sbx-abc123",
      "template_id": "tmpl-xyz789"
    }
  ]
}
```

事件对象中的常见字段如下：

| 字段          | 说明                               |
| ------------- | ---------------------------------- |
| `timestamp`   | 事件产生时间                       |
| `level`       | 事件级别                           |
| `event`       | 事件名称                           |
| `sandbox_id`  | 沙箱 ID                            |
| `template_id` | 模板 ID，仅 `sandbox.created` 携带                |
| `timeout`     | 新的超时值（秒），仅 `sandbox.timeout.updated` 携带 |
| `duration`    | 续期时长（秒），仅 `sandbox.refreshed` 携带         |

请求头如下：

| Header                     | 值                                            |
| -------------------------- | --------------------------------------------- |
| `Content-Type`             | `application/json`                            |
| `X-Cube-Webhook-Event`     | `batch`                                       |
| `X-Cube-Webhook-Signature` | `sha256=<hex>`，仅配置了 `hmac_secret` 时携带 |

## HMAC-SHA256 签名验证

为 endpoint 配置 `hmac_secret` 后，CubeAPI 会对原始 POST body 计算 HMAC-SHA256 签名，并附加到请求头中：

```text
X-Cube-Webhook-Signature: sha256=<hex>
```

接收端验签示例：

```python
import hmac, hashlib

def verify(raw_body: bytes, header_value: str, secret: str) -> bool:
    if not header_value or not header_value.startswith("sha256="):
        return False
    expected = hmac.new(
        secret.encode(), raw_body, hashlib.sha256
    ).hexdigest()
    return hmac.compare_digest(f"sha256={expected}", header_value)
```

::: tip 验签的是原始字节
签名是对 **HTTP 请求的原始 body bytes** 计算的。如果接收端先 `json.loads()` 再 `json.dumps()`，字段顺序或空格差异会导致验签失败。直接对原始字节计算即可。
:::

完整 receiver 实现参见 [examples/webhook-receiver/receiver.py](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/webhook-receiver/receiver.py)。

## 重试策略

webhook 投递在独立后台 task 中运行，不会阻塞沙箱创建、暂停、恢复、删除等 API handler。

投递失败时，CubeAPI 会按指数退避进行有限次数重试：

```text
第 1 次失败 → 等待 base
第 2 次失败 → 等待 base × 2
第 3 次失败 → 等待 base × 4
...
```

其中 `base` 为 `retry_backoff_millis`，单位毫秒。

| 参数                   | 默认值 | 说明                     |
| ---------------------- | ------ | ------------------------ |
| `max_retries`          | 3      | 首次投递失败后的重试次数 |
| `retry_backoff_millis` | 200    | 基础退避间隔，单位毫秒   |
| `request_timeout_secs` | 5      | 单次请求超时，单位秒     |
| `batch_size`           | 100    | 每批最大事件数           |
| `flush_interval_secs`  | 5      | 最长刷新间隔，单位秒     |

全部重试耗尽后，CubeAPI 会记录 error 日志并丢弃该批次。某个 endpoint 投递失败不会影响其他 endpoint，也不会阻塞 CubeAPI API 调用。

## 企业微信对接

企业微信群机器人只接受自己的消息格式，例如 `msgtype` + `text` / `markdown`，不支持直接接收任意 JSON payload。因此，不能把企业微信群机器人的 webhook URL 直接配置为 CubeAPI 的 webhook endpoint。

推荐做法是在中间增加一个适配服务：

```text
CubeAPI -- JSON --> 适配服务 -- 企业微信格式 --> 群机器人
```

实现步骤：

1. 在企业微信群中添加群机器人，获取机器人 webhook 地址：

```text
https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=YOUR_KEY
```

2. 部署一个适配服务，接收 CubeAPI webhook 回调，将事件转换成企业微信 markdown 或 text 消息后转发到机器人地址。

3. `examples/webhook-receiver/` 可以作为适配服务的起点，在其基础上添加企业微信消息格式化逻辑和 HTTP 转发逻辑。

4. 配置 CubeAPI 向适配服务发送事件：

```ini
CUBE_API_WEBHOOK_ENABLED=true
CUBE_API_WEBHOOK_URL=http://<your-adapter>:<your-port>/webhook
CUBE_API_WEBHOOK_EVENTS=sandbox.*
```

## 常见问题

**重启后日志里没有 `webhook_enabled=true`**

检查 `.one-click.env` 中变量名是否正确，应为 `CUBE_API_WEBHOOK_ENABLED`，不是 `ENABLE`。确认后重启 CubeAPI 再查看日志。

**Receiver 收不到事件**

先确认 CubeAPI 启动日志中 `webhook_enabled=true`，且 endpoint URL 正确。

然后在 CubeAPI 所在机器上确认 receiver 可达：

```bash
curl http://<receiver>:18080/health
```

正常情况下应返回：

```json
{"status":"ok"}
```

如果仍然收不到事件，可以查看投递错误日志：

```bash
journalctl -u cube-sandbox-cube-api.service --no-pager | grep HttpLogger
```

**HMAC 签名不匹配**

确认 CubeAPI 侧的 `hmac_secret` 和 receiver 侧的 `WEBHOOK_SECRET` 完全相同。

签名是对原始 POST body bytes 计算的，不要在接收端重新序列化 JSON 后再验签。需要手动验证时，可以将 body 保存为文件后使用：

```bash
openssl dgst -sha256 -hmac "your-secret" < body.bin
```

**日志中出现持续投递失败**

可以临时调大 `retry_backoff_millis` 或减小 `max_retries`，降低无效重试频率。

如果某个 endpoint 长期不可达，建议先注释掉对应配置并重启 CubeAPI，等 receiver 恢复后再加回来。