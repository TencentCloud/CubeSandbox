# Webhook 事件通知

CubeAPI 可以把沙箱生命周期事件投递到一个或多个 HTTP 端点。投递采用异步
方式：沙箱 API 成功响应不会等待 Webhook 网络请求、重试或接收端恢复。

可运行接收端位于
[`examples/webhook-receiver`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/webhook-receiver)。

## 支持的事件

| 事件 | 触发条件 | 附加上下文 |
| --- | --- | --- |
| `sandbox.created` | 沙箱创建成功 | `template_id` |
| `sandbox.deleted` | 沙箱删除成功 | - |
| `sandbox.paused` | 沙箱暂停成功 | - |
| `sandbox.resumed` | 沙箱恢复成功，包括 `connect` 触发的自动恢复 | `template_id` |

生命周期 API 调用失败时不会产生事件。

## 配置端点

在启动 CubeAPI 前，将 `CUBE_API_WEBHOOKS` 设置为 JSON 数组：

```bash
export CUBE_API_WEBHOOKS='[
  {
    "name": "automation",
    "url": "https://automation.example.com/cubesandbox/events",
    "events": ["sandbox.created", "sandbox.deleted"],
    "secret": "replace-with-a-high-entropy-secret"
  },
  {
    "name": "operations",
    "url": "https://alerts.example.com/cubesandbox/events",
    "events": ["sandbox.paused", "sandbox.resumed"]
  }
]'
```

每个端点支持以下字段：

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| `name` | 否 | CubeAPI 日志使用的安全标签，默认值为 `webhook-N` |
| `url` | 是 | 完整的 `http` 或 `https` URL；禁止 URL userinfo，不跟随重定向 |
| `events` | 否 | 订阅列表；省略或传空数组表示订阅全部支持事件 |
| `secret` | 否 | HMAC-SHA256 签名密钥；省略或传空字符串表示不签名 |

CubeAPI 会在启动时校验全部 URL 和订阅。错误配置会导致启动失败，而不是静默
关闭投递。配置调试输出和一键部署升级报告都会脱敏 URL 与 secret。

### 一键部署

把单行 JSON 写入安装器使用的 `.env`，然后重启 CubeAPI：

```bash
CUBE_API_WEBHOOKS='[{"name":"local","url":"http://127.0.0.1:8088/webhook","events":["sandbox.created","sandbox.deleted","sandbox.paused","sandbox.resumed"],"secret":"replace-me"}]'

sudo systemctl restart cube-sandbox-cube-api.service
sudo systemctl status cube-sandbox-cube-api.service
```

安装后的运行时环境文件仅允许 root 读取。不要把真实密钥提交到版本库。

### 直接运行二进制或容器

源码运行时，在 `cargo run` 前导出同一变量；容器部署时，通过编排系统传入该
变量。端点必须能从 CubeAPI 控制面进程访问，沙箱 CubeEgress 策略不控制这部分
流量。

## 投递参数

以下可选环境变量对全部端点生效：

| 变量 | 默认值 | 说明 |
| --- | ---: | --- |
| `CUBE_API_WEBHOOK_QUEUE_CAPACITY` | `1024` | 每个 CubeAPI 进程最多排队的生命周期事件数 |
| `CUBE_API_WEBHOOK_MAX_IN_FLIGHT` | `16` | 端点投递最大并发数 |
| `CUBE_API_WEBHOOK_TIMEOUT_MS` | `5000` | 每次 HTTP 尝试的超时时间 |
| `CUBE_API_WEBHOOK_MAX_ATTEMPTS` | `3` | 总尝试次数，包含首次请求 |
| `CUBE_API_WEBHOOK_RETRY_BASE_MS` | `500` | 首次重试延迟 |
| `CUBE_API_WEBHOOK_RETRY_MAX_MS` | `30000` | 最大重试延迟 |

重试延迟采用指数退避并设置上限：

```text
min(CUBE_API_WEBHOOK_RETRY_BASE_MS * 2^(failed_attempt - 1),
    CUBE_API_WEBHOOK_RETRY_MAX_MS)
```

连接失败、超时、HTTP `408`、HTTP `429` 和 `5xx` 会重试；其他 `3xx` 和
`4xx` 视为永久失败；任意 `2xx` 都表示成功。

## 请求格式

CubeAPI 每个 HTTP POST 投递一个事件，并设置 `Content-Type: application/json`。

```json
{
  "timestamp": "2026-07-10T08:15:30.123Z",
  "level": "info",
  "event": "sandbox.created",
  "sandbox_id": "sandbox-abc123",
  "template_id": "template-python"
}
```

字段说明：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `event` | string | 生命周期事件名 |
| `timestamp` | string | UTC RFC 3339 事件产生时间 |
| `sandbox_id` | string | 沙箱 ID |
| `template_id` | string | 无需额外控制面请求即可取得时携带 |
| `level` | string | 结构化事件级别；生命周期事件当前为 `info` |

请求头：

| 请求头 | 说明 |
| --- | --- |
| `X-CubeSandbox-Event` | 事件名 |
| `X-CubeSandbox-Delivery` | 一次端点投递的 UUID，重试时保持不变 |
| `X-CubeSandbox-Signature` | 配置 secret 时为 `sha256=<十六进制摘要>` |

## 验证签名

签名是对原始请求体字节计算的 HMAC-SHA256。必须先校验签名，再解析或重新
序列化 JSON。

```python
import hashlib
import hmac


def verify(body: bytes, header: str, secret: str) -> bool:
    expected = "sha256=" + hmac.new(
        secret.encode("utf-8"), body, hashlib.sha256
    ).hexdigest()
    return hmac.compare_digest(expected, header)
```

HMAC 可以证明请求来源，但不能单独防止重放。生产接收端应拒绝时间戳过旧的
事件，并在适当的保留周期内对 `X-CubeSandbox-Delivery` 去重。

## 运行接收端示例

```bash
cd examples/webhook-receiver
export WEBHOOK_SECRET=local-development-secret
python3 receiver.py
```

重启 CubeAPI 前，可在另一个终端验证接收端：

```bash
cd examples/webhook-receiver
export WEBHOOK_SECRET=local-development-secret
python3 send_test_event.py
```

把 CubeAPI 端点配置为 `http://127.0.0.1:8088/webhook`，然后依次创建、暂停、
恢复和删除沙箱。接收端会为每次回调输出一行 JSON。

## 企业微信与通用告警

CubeSandbox Payload 与企业微信群机器人 API 格式不同，因此不要把机器人 URL
直接配置为 CubeAPI 端点。可使用示例接收端做适配：

```bash
export WEBHOOK_SECRET=local-development-secret
export WECOM_BOT_URL='https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=replace-me'
python3 receiver.py
```

通用 HTTP 告警系统可以直接接收文档规定的 Payload，也可以用相同方式转换。
第三方平台凭据应只保存在接收端，使 CubeAPI 仅持有接收端 URL 和签名密钥。

## 投递保证与限制

- 队列有界且仅存在于内存中。队列满时会丢弃新事件并记录错误，但沙箱 API
  仍正常成功。
- 优雅关闭会排空 flush 屏障之前进入队列的事件；进程或宿主机崩溃可能丢失
  未投递事件。
- 运行中重试提供至少一次语义，因此接收端必须容忍重复事件。
- 当前不持久化投递历史，也没有死信存储。
- CubeAPI 多副本部署中，由处理生命周期 API 请求的副本产生事件；所有副本的
  端点配置应保持一致。
- 为避免把签名转发给非预期主机，投递不会跟随重定向。

需要更强投递保证时，应在接收端持久化回调，或接入持久化事件总线。

## 排障

| 现象 | 检查项 |
| --- | --- |
| CubeAPI 启动失败 | 确认 `CUBE_API_WEBHOOKS` 是 JSON 数组，且全部事件受支持 |
| 收不到回调 | 从 CubeAPI 宿主机检查端点连通性，并按端点 `name` 查看 CubeAPI 日志 |
| 接收端返回 HTTP 401 | 确认两端 secret 相同，并对原始请求体字节验签 |
| 收到重复回调 | 使用 `X-CubeSandbox-Delivery` 去重；重试会故意复用该值 |
| TLS 失败 | 将接收端 CA 安装到 CubeAPI 宿主机或容器信任库，不要关闭证书校验 |
