# CubeSandbox Webhook 接收端示例

这个示例会启动一个小型 HTTP 服务，接收 CubeOps 投递的 webhook batch，打印
JSON payload，并可选校验 `X-Cube-Signature-256`。

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

服务默认监听 `http://0.0.0.0:8088/webhook`。可以使用
`WEBHOOK_RECEIVER_HOST` 和 `WEBHOOK_RECEIVER_PORT` 覆盖监听地址。

## CubeOps 配置

创建 `/usr/local/services/cubetoolbox/CubeOps/webhooks.toml`：

```toml
[delivery]
event_queue_capacity = 10000
max_outstanding_deliveries = 1000
max_concurrent_requests = 100
default_batch_size = 1
flush_interval_secs = 5
request_timeout_secs = 5
max_attempts = 3
initial_backoff_ms = 500
max_backoff_secs = 10

[[endpoints]]
name = "local-dev-lifecycle"
url = "http://127.0.0.1:8088/webhook"
events = [
  "sandbox.created",
  "sandbox.deleted",
  "sandbox.paused",
  "sandbox.resumed",
  "api.error",
]
batch_size = 1
secret_env = "CUBE_WEBHOOK_SECRET_0"
```

`url` 必须能从 CubeOps 进程访问。在 `dev-env` 中，如果接收端运行在宿主机、
CubeOps 运行在 VM 内，请使用 `http://10.0.2.2:8088/webhook`。

把下面的值加入 `/usr/local/services/cubetoolbox/.one-click.env`，然后重启
CubeOps：

```bash
CUBE_OPS_WEBHOOK_CONFIG=/usr/local/services/cubetoolbox/CubeOps/webhooks.toml
CUBE_WEBHOOK_SECRET_0=change-me
sudo systemctl restart cube-sandbox-cubeops.service
```

创建、暂停、恢复并删除 sandbox。当 `batch_size = 1` 时，接收端应为每个成功
投递的事件打印一个 JSON batch。

投递是 best-effort。接收端应按 `batch_id` 对已经完整处理的外部 batch 去重；它在
同一个 batch 的重试之间保持不变。应在产生外部副作用前原子记录该 batch 已完成；
batch 可能到达零次、一次或多次，不同 batch 也可能乱序到达。

## 模拟失败

把 `url` 指向未监听的端口，重启 CubeOps 后触发事件。沙箱 API 调用仍会成功，
CubeOps 日志会记录重试和最终投递失败。
