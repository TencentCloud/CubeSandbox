# CubeSandbox Webhook 接收端

[English](README.md)

一个可直接运行、零依赖的示例，用于接收 CubeSandbox 的 **Webhook 事件通知**
（沙箱生命周期事件 `sandbox.created`、`sandbox.paused`、`sandbox.resumed`、
`sandbox.deleted`），校验可选的 HMAC-SHA256 签名，并打印每一条事件。

完整功能说明（配置格式、Payload 规范、签名机制）见
[Webhook 集成文档](../../docs/zh/guide/webhooks.md)。

## 目录内容

| 文件 | 用途 |
|---|---|
| `receiver.py` | 将收到的事件打印到标准输出；校验签名。 |
| `wecom_bridge.py` | 将事件转发到企业微信群机器人。 |
| `webhooks.example.yaml` | 可复制的 CubeAPI Webhook 配置示例。 |

两个脚本都只用 Python 3 标准库，无需 `pip install`。

## 5 分钟跑通全流程

### 1. 启动接收端

```bash
# 普通模式（不校验签名）
python3 receiver.py

# 或使用共享密钥校验签名
CUBE_WEBHOOK_SECRET=my-shared-secret python3 receiver.py
```

默认监听 `http://0.0.0.0:9100/webhook`（可用 `HOST` / `PORT` 覆盖）。

### 2. 配置 CubeAPI

复制 `webhooks.example.yaml`，把地址指向你的接收端：

```yaml
webhooks:
  - url: "http://127.0.0.1:9100/webhook"
    events: [sandbox.created, sandbox.paused, sandbox.resumed, sandbox.deleted]
    secret: "my-shared-secret"   # 必须与上面的 CUBE_WEBHOOK_SECRET 一致
```

用该配置启动（或重启）CubeAPI：

```bash
cube-api --webhook-config /path/to/webhooks.yaml
# 或
CUBE_API_WEBHOOK_CONFIG=/path/to/webhooks.yaml cube-api
```

启动时应能看到类似日志：`webhook event notifications enabled endpoints=1`。

### 3. 触发事件

通过 API 或 SDK 创建、暂停、恢复、删除一个沙箱。每个操作都会让接收端打印一条
事件，例如：

```
[2026-07-21 08:30:12 UTC] ✓ sandbox.created  (signature verified ✓)
{
  "event": "sandbox.created",
  "timestamp": "2026-07-21T08:30:12.481Z",
  "sandbox_id": "sbx-abc123",
  "template_id": "base"
}
```

> `template_id` 仅在 `sandbox.created` 上携带；其余三个事件包含 `sandbox_id`
> 和 `timestamp`。

## 自行校验签名

当设置了 `secret` 时，CubeAPI 会对**原始请求体**签名，并发送
`X-Cube-Signature: sha256=<hex>`。校验方式：

```python
import hashlib, hmac

def verify(secret: str, body: bytes, header: str) -> bool:
    expected = hmac.new(secret.encode(), body, hashlib.sha256).hexdigest()
    return hmac.compare_digest("sha256=" + expected, header)
```

务必对**收到的原始字节**计算 HMAC（不要先把 JSON 重新序列化——键顺序可能不同）。

## 转发到企业微信

在企业微信群里添加一个群机器人，复制其 Webhook 地址，然后：

```bash
WECOM_BOT_URL="https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=xxxxxxxx" \
CUBE_WEBHOOK_SECRET=my-shared-secret \
python3 wecom_bridge.py
```

每条沙箱事件都会以 markdown 消息转发到群里。同样的模式也适用于 Slack、飞书或
任意通用 HTTP 告警端点——把 `send_to_wecom` 换成目标平台的 API 即可。
