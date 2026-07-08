# CubeSandbox Webhook 接收端示例

这个示例会启动一个小型 HTTP 服务，用于接收 CubeAPI Webhook 事件、
打印 JSON Payload，并可选校验 `X-Cube-Signature-256`。

## 运行

```bash
cd examples/webhook-receiver
python3 receiver.py
```

启用签名校验：

```bash
export CUBE_WEBHOOK_SECRET_0='change-me'
python3 receiver.py
```

默认监听 `http://0.0.0.0:8088/webhook`。也可以覆盖监听地址：

```bash
WEBHOOK_RECEIVER_HOST=127.0.0.1 WEBHOOK_RECEIVER_PORT=8089 python3 receiver.py
```

## CubeAPI 配置

创建 `/usr/local/services/cubetoolbox/CubeAPI/webhooks.toml`：

```toml
[delivery]
queue_size = 10000
request_timeout_secs = 5
max_attempts = 3
initial_backoff_ms = 500
max_backoff_secs = 10
max_in_flight = 100

[[endpoints]]
name = "local-dev"
url = "http://127.0.0.1:8088/webhook"
events = [
  "sandbox.created",
  "sandbox.deleted",
  "sandbox.paused",
  "sandbox.resumed",
  "api.error",
]
secret_env = "CUBE_WEBHOOK_SECRET_0"
```

`url` 必须从 CubeAPI 进程所在机器可访问。只有接收端和 CubeAPI 在同一台机器上
时才使用 `127.0.0.1`。在 `dev-env` 中，如果接收端跑在宿主机、CubeAPI 跑在
VM 里，请使用 `http://10.0.2.2:8088/webhook`。

在 `/usr/local/services/cubetoolbox/.one-click.env` 中增加以下配置，然后重启 CubeAPI：

```bash
CUBE_API_WEBHOOK_CONFIG=/usr/local/services/cubetoolbox/CubeAPI/webhooks.toml
CUBE_WEBHOOK_SECRET_0=change-me
```

```bash
sudo systemctl restart cube-sandbox-cube-api.service
```

创建、暂停、恢复、销毁一个沙箱后，接收端会为每个投递成功的事件打印一段 JSON。

## 故障模拟

把 `url` 指向一个没有监听的端口，重启 CubeAPI 后触发事件。沙箱 API 调用应仍然成功，
CubeAPI 日志中会出现重试和最终投递失败记录。
