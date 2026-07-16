# CubeAPI Webhook receiver

[中文文档](README_zh.md)

This dependency-free development receiver validates CubeAPI HMAC signatures,
checks the event header and required payload fields, prints accepted events,
and can optionally forward a text notification to a WeCom bot.
Signed requests older than five minutes and repeated nonces are rejected to
limit replay attacks.

```bash
export CUBE_WEBHOOK_SECRET=created-endpoint-secret
python receiver.py
```

Configure CubeAPI with the matching URL and secret as described in the
[integration guide](https://github.com/TencentCloud/CubeSandbox/blob/master/docs/guide/integrations/webhooks.md). Set
`WEBHOOK_RECEIVER_HOST` or `WEBHOOK_RECEIVER_PORT` to change the listener.
Set `WECOM_BOT_URL` to forward accepted events to a WeCom group robot.

Run the signature tests with `python -m unittest -v test_receiver.py`. The
receiver is a local integration example, not a hardened public ingress service.
