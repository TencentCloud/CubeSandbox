# Webhook 接收端示例

一个零依赖、可直接运行的沙箱生命周期 Webhook 接收器。它监听
**cube-lifecycle-manager（CLM）**（或 CubeAPI）推送的 `POST` 事件，以人类可读
的方式打印，并支持可选的 HMAC-SHA256 签名验证。

> 线协议与 CubeAPI 的 webhook 发射器完全一致，因此本接收器对两者通用。完整
> 集成说明见 [`docs/zh/guide/integrations/webhook.md`](../../docs/zh/guide/integrations/webhook.md)。

## 环境要求

- Python 3.10+（仅标准库，无需 `pip install`）

## 快速开始

**1. 启动接收器**

```sh
cd examples/webhook-receiver

# 不做签名验证（仅开发用）
python receiver.py

# 带签名验证（推荐）
WEBHOOK_SECRET=my-shared-secret python receiver.py

# 自定义 host/port/path
python receiver.py --host 0.0.0.0 --port 9999 --path /events
```

**2. 让 cube-lifecycle-manager 指向它**

在 CLM 进程环境变量中配置：

```bash
export CUBE_LCM_WEBHOOK_URLS="http://127.0.0.1:8081/webhook"
export CUBE_LCM_WEBHOOK_EVENTS="*"              # 或逗号分隔的事件过滤
export CUBE_LCM_WEBHOOK_SECRET="my-shared-secret"  # 必须与上面的 WEBHOOK_SECRET 一致
```

重启 CLM。它现在通过自己的消费组（`cube-webhook-delivery`）消费生命周期事件流，
并把每个映射后的事件投递给接收器。完整变量清单见 `.env.example`。

**3. 观察事件到达**

每个沙箱的 `create / pause / resume / delete / timeout-update` 都会打印一个
彩色块，例如：

```
sandbox.created  2026-07-24 12:00:00.000  sandbox=sb-abc123
  template_id: tpl-python-3.12
  instance_type: cubebox
  timeout_seconds: 300
  auto_pause: true
  auto_resume: true
  created_at: 1784966400000
  end_at: 1784966700000
```

## 签名验证

设置 `WEBHOOK_SECRET` 后，接收器会拒绝任何 `X-Cube-Signature-256` 头与
`sha256=<hex>` 不匹配的请求——hex 是**对原始请求体做 HMAC-SHA256**（以 secret
为密钥）得到的值。CLM 侧使用同一 secret（`CUBE_LCM_WEBHOOK_SECRET`）。签名错误
返回 `401`。

## 投递语义（你需要知道的行为）

- **异步且不阻塞**：投递在后台进行，绝不影响沙箱创建/销毁主路径。
- **重试**：CLM 对每次投递最多重试 `CUBE_LCM_WEBHOOK_MAX_RETRIES + 1` 次
  （默认共 3 次），指数退避；耗尽后 ack 并 drop。接收器应返回 `2xx` 以停止重试。
- **At-least-once**：事件在投递完成后才 ack，因此 CLM 崩溃可能重投。接收器
  请按 `event_id` 字段（稳定流 ID）去重。
- **Payload**：JSON，含 `event`、`timestamp`、`event_id`、`sandbox_id`，以及
  best-effort 的上下文（`template_id`、`host_id` 等）扁平化在根对象上。

## 转发到企业微信或通用告警端点

本接收器是一个"展示"型接收端。要把它变成告警：

- **通用 HTTP 告警**（PagerDuty、Slack、自建 API）：把 `CUBE_LCM_WEBHOOK_URLS`
  指向一个能验签并按下游 API 重排 payload 的小型 relay。
- **企业微信机器人**：机器人 webhook 要求的请求体格式不同
  （`{"msgtype":"markdown",...}`），因此链路是 CLM → 小型 relay → 企业微信。
  可直接使用的 relay 片段与两种对接方式见
  [集成文档](../../docs/zh/guide/integrations/webhook.md#alerting-wecom-bot-and-generic-http)。

## License

Apache-2.0
