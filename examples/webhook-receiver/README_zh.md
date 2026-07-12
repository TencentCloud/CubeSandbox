# CubeAPI Webhook 接收端

[English](README.md)

这个无第三方依赖的开发接收端会校验 CubeAPI HMAC 签名、事件请求头和 payload
必填字段，打印通过校验的事件，并可选择转发文本消息到企业微信群机器人。

```bash
export CUBE_WEBHOOK_SECRET=created-endpoint-secret
python receiver.py
```

按照[集成指南](../../docs/zh/guide/integrations/webhooks.md)为 CubeAPI 配置相同的
URL 和密钥。通过 `WEBHOOK_RECEIVER_HOST`、`WEBHOOK_RECEIVER_PORT` 修改监听地址；
设置 `WECOM_BOT_URL` 可把事件转发到企业微信群机器人。

运行 `python -m unittest -v test_receiver.py` 验证签名逻辑。此程序用于本地集成
演示，不应直接作为公网生产入口。
