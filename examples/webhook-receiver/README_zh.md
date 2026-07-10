# CubeAPI Webhook 接收端

这个仅使用 Python 标准库的接收端会打印 CubeAPI 生命周期事件，并可选校验
HMAC-SHA256 签名。

```bash
export CUBE_WEBHOOK_SECRET=change-me
python3 receiver.py
```

在另一个终端配置 CubeAPI：

```bash
export WEBHOOK_URLS=http://127.0.0.1:8088/webhook
export WEBHOOK_SECRET=change-me
export WEBHOOK_EVENTS=sandbox.created,sandbox.deleted,sandbox.paused,sandbox.resumed
```

接收端会打印每个 JSON payload。payload 始终包含 `event`、`timestamp` 和
`level`；生命周期事件包含 `sandbox_id`，创建事件在 CubeAPI 获取到时还会
包含 `template_id`。

可通过 `WEBHOOK_RECEIVER_HOST` 和 `WEBHOOK_RECEIVER_PORT` 修改监听地址。
请勿将该示例接收端暴露到不受信任的网络。
