# Webhook 接收端 — Cube Sandbox 事件回调

[English](README.md)

实时接收并查看 [Cube Sandbox](https://github.com/tencent-cube/cube-sandbox) 的生命周期事件。CubeAPI 会在沙箱创建、删除、暂停、恢复等生命周期变化时，向配置的 Webhook URL 发送 HTTP POST 回调。

---

## 1. 架构

```
                          服务器
  ┌─────────────────────────────────────────────────────┐
  │                                                     │
  │  python create.py  ──▶  CubeAPI (:3000)             │
  │  (SDK 客户端)               │                       │
  │                             ▼                       │
  │                    python receiver.py (:8080)        │
  │                    (POST /webhook)                   │
  │                             │                       │
  │                        stdout: 格式化事件输出        │
  └─────────────────────────────────────────────────────┘
```

CubeAPI 异步投递事件（`tokio::spawn`，不阻塞），带指数退避重试（3 次：200ms → 500ms → 1s）。

---

## 2. 服务器操作

### 2.1 上传编译好的 CubeAPI 二进制

在本地构建 CubeAPI（项目根目录）：

```bash
cd CubeAPI/
cargo build --release
```

编译产物在 `CubeAPI/target/release/cube-api`。把这个二进制和接收端脚本一起上传到服务器：

```bash
scp target/release/cube-api user@<服务器IP>:/opt/cube/
scp examples/webhook-receiver/receiver.py user@<服务器IP>:/opt/cube/
```

### 2.2 启动接收端

```bash
cd /opt/cube/

# 默认监听 http://0.0.0.0:8080/webhook
python receiver.py

# 开启签名验证：
# WEBHOOK_SECRET=my-shared-secret python receiver.py
```

### 2.3 配置并启动 CubeAPI

**启动 CubeAPI 前设置**：

```bash
export CUBE_API_WEBHOOK_URLS=http://localhost:8080/webhook   # 接收端同机运行
export CUBE_API_WEBHOOK_EVENTS=*                              # 全部事件
export CUBE_API_WEBHOOK_SECRET=my-shared-secret               # 可选，和接收端保持一致

# 启动 CubeAPI
./cube-api
```

### 2.4 触发事件

SDK 客户端脚本在 [`code-sandbox-quickstart/`](../code-sandbox-quickstart/) 目录下：

```bash
cd ../code-sandbox-quickstart/

# 配置
cp .env.example .env
# 编辑 .env：E2B_API_URL=http://localhost:3000
#            CUBE_TEMPLATE_ID=<你的模板ID>

pip install -r requirements.txt

# 触发事件，观察接收端终端
python create.py       # → sandbox.created, sandbox.deleted
python pause.py        # → sandbox.paused, sandbox.resumed
python exec_code.py
python cmd.py
```

---

## 3. 事件载荷格式

```json
{
  "event": "sandbox.created",
  "timestamp": "2026-07-24T12:00:00Z",
  "sandbox_id": "sb-abc123",
  "template_id": "tpl-python-3.12"
}
```

### 事件类型

| 事件                     | 描述                   | 额外字段               |
|--------------------------|------------------------|------------------------|
| `sandbox.created`        | 沙箱创建成功           | —                      |
| `sandbox.deleted`        | 沙箱已删除             | —                      |
| `sandbox.paused`         | 沙箱已暂停             | —                      |
| `sandbox.resumed`        | 沙箱已恢复             | —                      |
| `sandbox.timeout.updated`| 沙箱超时时间已修改     | `timeout`（秒）        |
| `sandbox.refreshed`      | 沙箱 TTL 已刷新        | `duration`（秒）       |
| `api.response`           | API 请求完成           | —                      |
| `api.error`              | API 处理错误           | `handler`, `error`     |

### 签名验证

当 CubeAPI 配置了 `CUBE_API_WEBHOOK_SECRET`，每个 POST 包含：

```
X-Cube-Signature-256: sha256=<HMAC 十六进制>
```

HMAC 是 **HMAC-SHA256(原始JSON体, 共享密钥)**。接收端设置 `WEBHOOK_SECRET` 后自动验证。签名无效 → HTTP 401。

---

## 4. 端到端测试

```bash
# 终端 1 — 启动接收端
cd examples/webhook-receiver/
WEBHOOK_SECRET=test-secret python receiver.py

# 终端 2 — 启动 CubeAPI（开启 Webhook）
cd ../..
export CUBE_API_WEBHOOK_URLS=http://localhost:8080/webhook
export CUBE_API_WEBHOOK_SECRET=test-secret
export CUBE_API_WEBHOOK_EVENTS=*
cargo run --release

# 终端 3 — 触发事件
cd examples/code-sandbox-quickstart/
python create.py

# → 终端 1 显示：
#   sandbox.created  2026-07-24 20:03:15.123  sandbox=sb-abc123
```

---

## 5. 事件过滤

```bash
export CUBE_API_WEBHOOK_EVENTS=sandbox.created,sandbox.deleted,sandbox.paused,sandbox.resumed
```

过滤在 CubeAPI 侧配置，接收端打印所有收到的事件。

---

## 6. 生产环境建议

本接收端面向**开发和测试**。生产环境建议：

- 使用成熟的 Webhook 框架（Flask、FastAPI 或消息队列消费者）
- 添加持久化存储（文件日志、数据库或消息队列）
- 实现幂等处理（重试可能导致重复投递）
- 考虑以 systemd 服务或 Docker 容器运行

---

## 7. 参考

- [CubeAPI webhook 源码](../../CubeAPI/src/logging/http.rs) — Rust 投递后端
- [事件日志类型](../../CubeAPI/src/logging/mod.rs) — `LogEvent` 结构体
- [配置字段](../../CubeAPI/src/config/mod.rs) — `ServerConfig.webhook_*`
- [SDK 客户端脚本](../code-sandbox-quickstart/) — 触发事件的示例