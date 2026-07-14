---
title: Webhook 事件通知
author: zhangzherui
date: 2026-07-12
tags: [integration, webhook, lifecycle, security]
---

# Webhook 事件通知

CubeAPI 可在不等待接收端 I/O 的情况下，把沙箱生命周期事件投递到多个 HTTP
endpoint。每个 endpoint 可独立配置订阅事件和 HMAC 密钥。

## 配置

```bash
export WEBHOOK_ENDPOINTS_JSON='[
  {"url":"http://receiver:8088/webhook","events":["sandbox.created","sandbox.deleted"],"secret":"created-endpoint-secret"},
  {"url":"https://audit.example.com/cube","events":["sandbox.paused","sandbox.resumed"],"secret":"audit-secret"}
]'
export WEBHOOK_QUEUE_CAPACITY=1024
export WEBHOOK_MAX_RETRIES=3
export WEBHOOK_RETRY_BASE_MS=250
export WEBHOOK_REQUEST_TIMEOUT_SECS=10
```

JSON 无效、URL 不是 HTTP(S) 或订阅为空时，CubeAPI 会启动失败，避免静默漏报。
不设置该变量即关闭 Webhook。

## Payload 与签名

每次 POST 都是扁平化的结构化 `LogEvent`：

```json
{"timestamp":"2026-07-12T08:00:00Z","level":"info","event":"sandbox.created","sandbox_id":"sb-123","template_id":"tpl-456"}
```

请求包含 `X-Cube-Event`。配置密钥后还包含 `X-Cube-Timestamp: <Unix 秒>` 和
`X-Cube-Signature-256: sha256=<hex>`。接收端应对
`<timestamp>.<原始请求体>` 计算 HMAC-SHA256，拒绝超出五分钟新鲜度窗口的时间戳，
以常量时间比较签名，然后再解析 JSON。参见[接收端示例](../../../examples/webhook-receiver/README_zh.md)。

## 投递语义

API 主路径仅尝试写入有界队列，不等待网络；队列满时丢弃新事件并记录告警。
网络错误、HTTP 408/429 与 5xx 使用指数退避重试，其他 4xx 不重试。优雅关闭时
会排空 flush barrier 前已接收的事件。重试意味着接收端可能收到重复事件，应使用
`(event, sandbox_id, timestamp)` 实现幂等。内存队列不会跨 CubeAPI 崩溃恢复。

设置示例接收端的 `WECOM_BOT_URL` 即可对接企业微信群机器人，使接收端负责密钥
隔离与消息格式转换。
