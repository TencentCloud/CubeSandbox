# CubeSandbox Webhook 接收端

这是一个零第三方依赖的 Python 接收端示例，可接收 CubeSandbox 沙箱生命周期
Webhook，并可选校验 `X-Cube-Signature-256` HMAC-SHA256 签名。

## 启动

在仓库根目录执行：

```bash
python3 examples/webhook-receiver/receiver.py \
  --port 8099 \
  --secret local-development-secret
```

CubeAPI 使用逗号分隔的一个或多个地址：

```bash
export WEBHOOK_URLS=http://127.0.0.1:8099/webhook
export WEBHOOK_SECRET=local-development-secret
export WEBHOOK_EVENTS=sandbox.created,sandbox.deleted,sandbox.paused,sandbox.resumed
export WEBHOOK_MAX_RETRIES=3
export WEBHOOK_RETRY_BASE_MS=250
```

重启 `cube-api` 后，通过正常的 CubeAPI 沙箱 API 操作即可。接收端会打印每个
通过校验的 JSON，并返回 `204 No Content`。

如果接收端在另一台机器上，请使用 HTTPS，并将 secret 放在部署系统的密钥管理器
中，不要写入 shell 历史或源码。

## Payload

Payload 就是结构化 `LogEvent` 对象。生命周期事件至少包含 `event`、`timestamp`、
`sandbox_id`；创建事件还会携带 `template_id`。

```json
{
  "event": "sandbox.created",
  "timestamp": "2026-07-23T12:00:00.000Z",
  "level": "info",
  "sandbox_id": "sbx-example",
  "template_id": "tpl-example"
}
```

## 手动校验签名

签名针对 HTTP 请求的原始字节计算，而不是重新编码后的 JSON 对象：

```bash
secret='local-development-secret'
body='{"event":"sandbox.created","timestamp":"2026-07-23T12:00:00Z","sandbox_id":"sbx-demo"}'
signature="sha256=$(printf '%s' "$body" | openssl dgst -sha256 -hmac "$secret" | awk '{print $2}')"

curl -i http://127.0.0.1:8099/webhook \
  -H 'Content-Type: application/json' \
  -H "X-Cube-Signature-256: $signature" \
  --data-binary "$body"
```

预期响应为 `204`。如果接收端以 `--secret` 启动，签名缺失或错误会返回 `401`。

## 对接企业微信或通用告警

接收端完成签名校验后，可以在同一个处理函数中把事件转换后转发给企业微信机器人：

```python
import json
import os
import urllib.request

message = {
    "msgtype": "markdown",
    "markdown": {"content": f"CubeSandbox 事件：{payload['event']}\n"
                              f"sandbox：{payload['sandbox_id']}"},
}
request = urllib.request.Request(
    os.environ["WECOM_BOT_URL"],
    data=json.dumps(message).encode(),
    headers={"Content-Type": "application/json"},
)
urllib.request.urlopen(request, timeout=5).read()
```

对接通用 HTTP 告警时，可保留原始 Payload，并在转发时增加下游服务自己的
Authorization 或消息包装。不要把 CubeSandbox 的 HMAC header 当作下游服务的认证。
