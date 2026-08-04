[English](./README.md) | [中文](./README_zh.md)

# CubeSandbox Webhook 接收端

这是一个仅使用 Python 标准库的 Webhook 接收端示例。它可以接收
CubeSandbox 生命周期事件、校验可选的 HMAC-SHA256 签名、将事件按 JSONL
输出，并可选择把事件转换为企业微信群机器人文本消息。

## 文件说明

- `receiver.py`：多线程 HTTP 接收端，包含 Payload 与签名校验
- `send_test_event.py`：无需启动 CubeAPI 即可发送一个签名测试事件
- `test_receiver.py`：签名和 Payload 校验单元测试
- `.env.example`：环境变量参考

## 快速开始

只需 Python 3.9 或更高版本，无需安装第三方包。

终端 1：

```bash
cd examples/webhook-receiver
export WEBHOOK_SECRET=local-development-secret
python3 receiver.py
```

终端 2：

```bash
cd examples/webhook-receiver
export WEBHOOK_SECRET=local-development-secret
python3 send_test_event.py
```

预期输出：

```text
receiver returned HTTP 204
```

接收端会输出一行 JSON，其中包含 `delivery_id`、`event`、`timestamp` 和
`sandbox_id`。运行示例单元测试：

```bash
python3 -m unittest -v test_receiver.py
```

## 接入 CubeAPI

在启动 CubeAPI 前设置 `CUBE_API_WEBHOOKS`。该变量是 JSON 数组，因此一个
CubeAPI 进程可以向多个端点投递，并为每个端点配置独立订阅。

```bash
export CUBE_API_WEBHOOKS='[
  {
    "name": "local-receiver",
    "url": "http://127.0.0.1:8088/webhook",
    "events": [
      "sandbox.created",
      "sandbox.deleted",
      "sandbox.paused",
      "sandbox.resumed"
    ],
    "secret": "local-development-secret"
  }
]'
```

重启 CubeAPI，设置一个处于 Ready 状态的模板 ID，然后执行完整生命周期：

```bash
export CUBE_API_URL="${CUBE_API_URL:-http://127.0.0.1:3000}"
export CUBE_TEMPLATE_ID="替换为可用的模板ID"

SANDBOX_ID="$(
  curl -fsS \
    -H 'Content-Type: application/json' \
    -d "{\"templateID\":\"${CUBE_TEMPLATE_ID}\",\"timeout\":300}" \
    "${CUBE_API_URL}/sandboxes" |
    python3 -c 'import json, sys; print(json.load(sys.stdin)["sandboxID"])'
)"
printf 'sandbox: %s\n' "${SANDBOX_ID}"

curl -fsS -o /dev/null -X POST \
  "${CUBE_API_URL}/sandboxes/${SANDBOX_ID}/pause"
curl -fsS -o /dev/null -X POST \
  -H 'Content-Type: application/json' \
  -d '{"timeout":300}' \
  "${CUBE_API_URL}/sandboxes/${SANDBOX_ID}/resume"
curl -fsS -o /dev/null -X DELETE \
  "${CUBE_API_URL}/sandboxes/${SANDBOX_ID}"
```

接收端应收到 `sandbox.created`、`sandbox.paused`、`sandbox.resumed` 和
`sandbox.deleted` 四类回调。投递会并发执行，因此不保证到达顺序。创建和恢复
事件还会携带 `template_id`。如果 CubeAPI 启用了鉴权，请在每条 `curl` 命令中
添加部署所需的鉴权请求头。

一键部署环境可将同一变量写入安装使用的 `.env` 文件，然后重启 CubeAPI：

```bash
sudo systemctl restart cube-sandbox-cube-api.service
sudo journalctl -u cube-sandbox-cube-api.service -f
```

完整配置、Payload、重试语义、部署方式和排障方法请参阅
[Webhook 指南](../../docs/zh/guide/webhooks.md)。

## 转发到企业微信

在接收端设置 `WECOM_BOT_URL`，不要直接把企业微信机器人 URL 配置给
CubeAPI。示例接收端会把 CubeSandbox Payload 转换成群机器人要求的消息格式。

```bash
export WECOM_BOT_URL='https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=replace-me'
python3 receiver.py
```

企业微信转发失败时，接收端返回 HTTP 502，CubeAPI 会重试原始投递。因此生产
消费者应使用 `X-CubeSandbox-Delivery` 做幂等去重。

## 安全与限制

- 接收端默认只监听 `127.0.0.1`。对外提供服务前，应部署在带鉴权的 TLS
  反向代理之后。
- 不要把 `WEBHOOK_SECRET` 提交到版本库；生产环境应使用高熵随机值。
- 签名覆盖原始请求体字节，必须先验签再解析 JSON。
- 示例只打印事件，不保存持久化投递历史。
- 使用 `Ctrl-C` 停止接收端；它不会创建容器或持久文件。
