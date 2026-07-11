# CubeAPI Webhook 接收端示例

[English](./README.md)

本示例使用 Python 标准库实现了一个本地 CubeAPI Webhook 接收端。接收端监听 `127.0.0.1:18080`，并通过 `POST /webhook` 接收 CubeAPI 推送的生命周期事件。

## 前置条件

开始验证前，请确认以下条件已经满足：

- 本地 CubeSandbox 环境已正常部署并运行。
- CubeAPI 可通过 `http://127.0.0.1:3000` 访问。
- 已准备一个可用于创建沙箱的有效模板 ID。
- 已安装 Python 3 和 Rust 工具链。
- 本机端口 `18080` 未被其他程序占用。
- 具备临时停止和恢复现有 CubeAPI 服务的权限。

本示例用于验证 Webhook 功能，不包含 CubeSandbox 的完整安装流程。首次部署 CubeSandbox 时，请先按照项目主文档完成环境安装。

## 快速开始

下面的验证过程需要同时使用三个终端，以便将接收端输出、CubeAPI 运行日志和生命周期接口命令分别展示，避免验证信息相互混杂：

- 终端一：运行 Webhook 接收端。
- 终端二：运行当前分支构建的 CubeAPI。
- 终端三：调用沙箱生命周期接口。

### 1. 启动接收端

在第一个终端中进入示例目录，并启用 HMAC-SHA256 签名校验：

```bash
cd CubeAPI/examples/webhook-receiver
WEBHOOK_SECRET=test-secret python3 receiver.py
```

如果需要验证不带签名的 Webhook，可以不设置 `WEBHOOK_SECRET`：

```bash
python3 receiver.py
```

此时还需要从 `CUBE_API_WEBHOOK_ENDPOINTS` 配置中删除 `secret` 字段，否则 CubeAPI 仍会发送签名请求。

启用签名后，接收端会读取以下请求头：

```text
X-Cube-Webhook-Signature
```

签名内容由以下字段拼接而成：

```text
timestamp + "." + delivery_id + "." + raw_request_body
```

签名值的格式为：

```text
v1=<lowercase-hex>
```

其中 `<lowercase-hex>` 表示 HMAC-SHA256 结果对应的小写十六进制字符串。

### 2. 从当前分支启动 CubeAPI

在第二个终端中进入 `CubeAPI` 目录，并构建当前分支：

```bash
cd CubeAPI
cargo build
```

如果系统中已有 CubeAPI 服务正在运行，需要先临时停止该服务，避免端口 `3000` 被占用：

```bash
sudo systemctl stop cube-sandbox-cube-api.service
```

然后使用当前分支构建出的二进制启动 CubeAPI：

```bash
CUBE_API_WEBHOOK_ENDPOINTS='[{"url":"http://127.0.0.1:18080/webhook","events":["sandbox.created","sandbox.deleted","sandbox.paused","sandbox.resumed"],"secret":"test-secret","enabled":true,"allow_private_urls":true}]' \
  ./target/debug/cube-api
```

在整个验证过程中，请保持该 CubeAPI 进程持续运行。

> 已部署的 CubeAPI 可能依赖额外的环境变量、配置文件或特定工作目录。手动运行当前分支二进制时，需要保留原 CubeAPI 服务使用的必要运行环境。

示例中的 Webhook 地址使用了 `127.0.0.1`，因此配置中显式设置了：

```json
"allow_private_urls": true
```

CubeAPI 默认会拒绝私有地址、回环地址和链路本地地址，以降低 SSRF 风险。该选项只应在本地开发或可信内部网络中使用，不要为不可信的 Webhook 地址启用。

### 3. 执行生命周期验证

在第三个终端中，先确认 CubeAPI 已经正常启动：

```bash
curl -fsS http://127.0.0.1:3000/health
```

然后设置一个有效的模板 ID：

```bash
export CUBE_TEMPLATE_ID=<your-template-id>
```

创建沙箱，并从响应中提取沙箱 ID：

```bash
CREATE_RESPONSE=$(curl -fsS -X POST http://127.0.0.1:3000/sandboxes \
  -H 'Content-Type: application/json' \
  -d "{\"templateID\":\"${CUBE_TEMPLATE_ID}\",\"timeout\":300}")

SANDBOX_ID=$(printf '%s' "$CREATE_RESPONSE" | python3 -c \
  'import json, sys; print(json.load(sys.stdin)["sandboxID"])')
```

随后依次暂停、恢复并删除该沙箱：

```bash
curl -fsS -X POST \
  "http://127.0.0.1:3000/sandboxes/${SANDBOX_ID}/pause"

curl -fsS -X POST \
  "http://127.0.0.1:3000/sandboxes/${SANDBOX_ID}/resume" \
  -H 'Content-Type: application/json' \
  -d '{"timeout":300}'

curl -fsS -X DELETE \
  "http://127.0.0.1:3000/sandboxes/${SANDBOX_ID}"
```

如果当前 CubeAPI 部署启用了身份认证，请根据部署配置为每个请求添加相应的认证请求头。

### 4. 恢复原 CubeAPI 服务

验证完成后，在第二个终端中按 `Ctrl+C` 停止当前分支的前台 CubeAPI 进程。

随后恢复原来的 CubeAPI 服务：

```bash
sudo systemctl start cube-sandbox-cube-api.service
sudo systemctl status cube-sandbox-cube-api.service --no-pager -l
```

在自定义部署或非 systemd 环境中，服务名称和恢复方式可能不同。验证完成后，应确保原 CubeAPI 服务重新启动并通过健康检查。

## 预期输出

生命周期接口调用成功后，接收端终端中应看到以下事件：

```text
sandbox.created
sandbox.paused
sandbox.resumed
sandbox.deleted
```

每个通知中都会包含投递元数据和与当前事件相关的字段，但通知内容并不是完整的 `Sandbox` 对象。

设置 `WEBHOOK_SECRET` 后，接收端只有在 HMAC 签名校验通过时才会输出事件。

如果签名无效，接收端将返回：

```text
401 invalid signature
```

## 故障排查

| 问题 | 可能原因 |
|---|---|
| `401 invalid signature` | 接收端的 `WEBHOOK_SECRET` 与端点配置中的 `secret` 不一致，或者代理修改了参与签名的请求体或请求头 |
| `400 invalid JSON` | 请求体不是合法的 UTF-8 JSON |
| 连接被拒绝或持续重试 | 接收端未启动、端口被占用，或者配置的 Webhook URL 不正确 |
| 未收到回调 | 端点未启用、没有订阅对应事件，或者 CubeAPI 启动时未加载 `CUBE_API_WEBHOOK_ENDPOINTS` |
| 启动时报私有地址错误 | 本地接收端使用了回环地址，需要在可信环境中设置 `"allow_private_urls": true` |
| 容器中的 CubeAPI 无法访问接收端 | 容器内的 `127.0.0.1` 指向容器自身，应改用宿主机可达地址或容器服务名称 |

## 告警适配器模式

CubeAPI Webhook 使用通用 HTTP JSON 格式，不直接依赖企业微信、飞书、Slack 等平台的机器人协议。

需要接入第三方告警平台时，建议部署一个独立的适配器，由适配器完成以下工作：

1. 接收 CubeAPI Webhook 请求；
2. 使用原始请求体验证 HMAC 签名；
3. 将沙箱生命周期事件转换为目标平台要求的消息格式；
4. 使用由适配器安全保存的凭据，将消息转发到目标服务。

不要在 CubeAPI 中硬编码特定厂商的机器人协议，也不要将密钥或其他凭据写入 Webhook Payload、应用日志或 Pull Request 描述。