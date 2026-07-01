# Webhook 接收端示例

[English](README.md)

这个示例启动一个轻量 HTTP receiver，用于接收 CubeAPI webhook 回调。它适合本地快速验证 webhook 投递链路，也可以作为自定义接收端的参考实现。

特性：

* 纯 Python 标准库，无需 `pip install`
* 支持 HMAC-SHA256 签名验证
* 提供 `/health` 健康检查端点
* 收到事件后在 stdout 打印事件名称、sandbox ID 和相关字段

完整配置说明，包括多 endpoint、TOML 配置、重试调参和企业微信对接等，见 [Webhook 事件通知](../../docs/zh/guide/webhook-events.md)。

## 快速开始

```bash
python3 receiver.py
```

启动后 receiver 默认监听 `:18080`。保持该终端运行，再用另一个终端发送测试请求。

需要调整运行方式时，可以通过环境变量配置：

| 环境变量             | 说明                       |
| ---------------- | ------------------------ |
| `WEBHOOK_PORT`   | receiver 监听端口，默认 `18080` |
| `WEBHOOK_SECRET` | HMAC 验签密钥；为空时不校验签名       |

例如：

```bash
WEBHOOK_PORT=19090 WEBHOOK_SECRET=secret python3 receiver.py
```

## 本地发送测试 payload

```bash
curl -s -X POST http://localhost:18080/webhook \
  -H "Content-Type: application/json" \
  -d '{"events":[{"timestamp":"2026-01-01T12:00:00Z","level":"info","event":"sandbox.created","sandbox_id":"sbx-1","template_id":"tmpl-1"}]}'
```

receiver 会输出类似内容：

```text
[<receive-time>] received 1 event(s)
  [2026-01-01T12:00:00Z] sandbox.created
    sandbox_id : sbx-1
    template_id: tmpl-1
```

其中第一行是 receiver 实际收到请求的时间，事件行中的 timestamp 来自 webhook payload。

## 启用 HMAC 并测试签名

先用密钥启动 receiver：

```bash
WEBHOOK_SECRET=secret python3 receiver.py
```

然后在另一个终端计算签名并发送请求：

```bash
SECRET="secret"
BODY='{"events":[{"timestamp":"2026-06-26T12:00:00Z","level":"info","event":"sandbox.created","sandbox_id":"sbx-1","template_id":"tmpl-1"}]}'
SIG="sha256=$(echo -n "$BODY" | openssl dgst -sha256 -hmac "$SECRET" | cut -d' ' -f2)"

curl -s -X POST http://localhost:18080/webhook \
  -H "Content-Type: application/json" \
  -H "X-Cube-Webhook-Signature: $SIG" \
  -d "$BODY"
```

签名基于 **HTTP 请求的原始 body bytes** 计算。接收端不要先 `json.loads()` 再 `json.dumps()` 后验签，否则字段顺序或空格差异会导致签名不匹配。

## 让 CubeAPI 投递到 receiver

在 `.one-click.env` 末尾追加 webhook 配置。one-click 安装后该文件通常位于：

```text
/usr/local/services/cubetoolbox/.one-click.env
```

配置内容：

```ini
CUBE_API_WEBHOOK_ENABLED=true
CUBE_API_WEBHOOK_URL=http://127.0.0.1:18080/webhook
CUBE_API_WEBHOOK_EVENTS=sandbox.*
```

重启 CubeAPI：

```bash
systemctl restart cube-sandbox-cube-api.service
```

查看启动日志，确认 webhook 已启用：

```bash
journalctl -u cube-sandbox-cube-api.service -n 20 --no-pager
```

日志中应包含类似内容：

```text
INFO cube_api: cube-api starting ... webhook_enabled=true webhook_endpoint_count=1
INFO cube_api: webhook endpoint configured url=http://127.0.0.1:18080/webhook
```

## 触发生命周期事件

当前 webhook receiver 可用于验证以下沙箱生命周期事件：

| 事件                       | 触发操作                                 |
| -------------------------- | ---------------------------------------- |
| `sandbox.created`          | `POST /sandboxes` 成功                   |
| `sandbox.timeout.updated`  | `POST /sandboxes/:id/timeout` 成功       |
| `sandbox.refreshed`        | `POST /sandboxes/:id/refreshes` 成功     |
| `sandbox.paused`           | `POST /sandboxes/:id/pause` 成功         |
| `sandbox.resumed`          | `POST /sandboxes/:id/resume` 成功        |
| `sandbox.deleted`          | `DELETE /sandboxes/:id` 成功             |

依次执行以下请求即可触发各类事件：

```bash
# 创建沙箱
curl -X POST http://localhost:3000/sandboxes \
  -H "Content-Type: application/json" \
  -d '{"templateID": "your-template-id"}'

# 更新沙箱超时
curl -X POST http://localhost:3000/sandboxes/<sandbox_id>/timeout \
  -H "Content-Type: application/json" \
  -d '{"timeout": 300}'

# 续期沙箱超时
curl -X POST http://localhost:3000/sandboxes/<sandbox_id>/refreshes \
  -H "Content-Type: application/json" \
  -d '{"duration": 600}'

# 暂停沙箱
curl -X POST http://localhost:3000/sandboxes/<sandbox_id>/pause

# 恢复沙箱
curl -X POST http://localhost:3000/sandboxes/<sandbox_id>/resume \
  -H "Content-Type: application/json" \
  -d '{}'

# 删除沙箱
curl -X DELETE http://localhost:3000/sandboxes/<sandbox_id>
```

每次操作成功后，receiver 终端应打印对应的 webhook 事件。

## 常见问题

**日志里没有 `webhook_enabled=true`**

检查 `.one-click.env` 中的变量名是否为 `CUBE_API_WEBHOOK_ENABLED`，不要写成 `ENABLE`。修改后重启 CubeAPI。

**Receiver 收不到事件**

先确认 CubeAPI 所在机器能访问 receiver：

```bash
curl http://<receiver>:18080/health
```

正常情况下应返回：

```json
{"status":"ok"}
```

同时检查 CubeAPI 启动日志中的 endpoint URL 是否正确。

**HMAC 签名不匹配**

确认 CubeAPI 侧配置的 `hmac_secret` 与 receiver 侧的 `WEBHOOK_SECRET` 完全一致。签名必须基于原始 body bytes 计算，不要重新序列化 JSON 后再验签。

## 备注

* receiver 不会在输出中打印密钥明文。
* receiver 只接受 `/webhook` 的 POST 请求，其他路径返回 404。
* receiver 默认监听 `0.0.0.0`，因此 CubeAPI 在另一个容器或虚拟机中也可以访问。
* 更多配置和排查方式见 [Webhook 事件通知](../../docs/zh/guide/webhook-events.md)。
