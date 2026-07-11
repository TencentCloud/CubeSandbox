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

三个容量参数分别限制 channel 中等待的事件、尚未完成的 endpoint batch 投递，
以及正在执行网络 I/O 的 HTTP attempt：

```text
event_queue_capacity -> max_outstanding_deliveries -> max_concurrent_requests
```

它们统计的单位不同，因此没有强制的数值大小关系。通常让
`max_outstanding_deliveries` 大于 `max_concurrent_requests`，使处于重试退避的
投递保留 task 名额时，不会占用全部 HTTP 请求并发。

同一个 `url` 可以在另一条 endpoint 配置中复用，用于不重叠的高频事件集合，
例如 `api.request` 和 `api.response`，并给那条配置设置更大的 `batch_size`。
如果相同 `url` 和相同事件被重复订阅，CubeAPI 会拒绝启动。

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

创建、暂停、恢复、销毁一个沙箱后，在 `batch_size = 1` 下，接收端会为每个
投递成功的事件打印一段 JSON batch。

## 故障模拟

把 `url` 指向一个没有监听的端口，重启 CubeAPI 后触发事件。沙箱 API 调用应仍然成功，
CubeAPI 日志中会出现重试和最终投递失败记录。
