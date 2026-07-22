# CubeSandbox Webhook 接收端示例

这个示例启动一个本地 HTTP 接收端，用来接收 CubeAPI Webhook 事件。示例只使用
Python 标准库。

## 运行

```bash
cd examples/webhook-receiver
WEBHOOK_SECRET=dev-secret python3 receiver.py
```

默认监听地址是 `http://127.0.0.1:9000/webhook`。

为 CubeAPI 配置 Webhook endpoint：

```bash
export CUBE_API_WEBHOOK_ENDPOINTS='[
  {
    "url": "http://127.0.0.1:9000/webhook",
    "events": [
      "sandbox.created",
      "sandbox.deleted",
      "sandbox.paused",
      "sandbox.resumed"
    ],
    "secret": "dev-secret"
  }
]'
```

带着上面的环境变量启动 CubeAPI。创建、暂停、恢复或删除沙箱时，接收端会打印事件
payload。

## 签名

配置 `secret` 后，CubeAPI 会发送这些请求头：

- `X-Cube-Timestamp`
- `X-Cube-Nonce`
- `X-Cube-Signature-256`

签名计算方式：

```text
sha256=HMAC_SHA256(secret, timestamp + "." + nonce + "." + raw_body)
```

接收端设置相同的 `WEBHOOK_SECRET` 后即可完成验签。

## 测试

```bash
python3 -m unittest discover -s examples/webhook-receiver
```
