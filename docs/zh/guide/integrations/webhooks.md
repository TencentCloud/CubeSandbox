---
title: Webhook 事件通知
author: initiallyqq
date: 2026-07-10
tags:
  - integration
  - webhook
  - observability
lang: zh-CN
---

# Webhook 事件通知

CubeAPI 可以将沙箱生命周期事件异步发送到一个或多个 HTTP endpoint。Webhook
投递在后台任务中执行，因此接收端慢或不可用不会阻塞创建、删除、暂停或恢复请求。

## 配置

将 `WEBHOOK_URLS` 设置为以逗号分隔的 endpoint URL 列表。不设置该变量时
Webhook 保持关闭。

```bash
export WEBHOOK_URLS=https://ops.example.com/cube-events,https://audit.example.com/events
export WEBHOOK_EVENTS=sandbox.created,sandbox.deleted,sandbox.paused,sandbox.resumed
export WEBHOOK_SECRET=replace-with-a-shared-secret
export WEBHOOK_QUEUE_CAPACITY=1000
export WEBHOOK_MAX_RETRIES=3
export WEBHOOK_RETRY_BASE_MS=200
export WEBHOOK_REQUEST_TIMEOUT_SECS=10
```

`WEBHOOK_EVENTS` 为可选项，默认包含全部四种沙箱生命周期事件。每个已配置
endpoint 会收到相同的事件类型。

## Payload 与签名

CubeAPI 每个事件发送一个 JSON 对象，其中包含 `event`、`timestamp`、`level`
以及 `sandbox_id`、`template_id` 等事件字段。

设置 `WEBHOOK_SECRET` 后，每个请求会携带：

```text
X-Cube-Signature-256: sha256=<原始请求体的 HMAC-SHA256 十六进制值>
```

请在解析 JSON 前使用原始请求体校验签名。仓库内附带的
[接收端示例](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/webhook-receiver/README_zh.md)
使用 Python 标准库完成校验。

## 投递行为

每个 endpoint 都有一个有界内存队列。队列满时会记录告警并丢弃新事件，不会延迟
沙箱 API。网络错误、HTTP `408`、`429` 和 `5xx` 会以指数退避方式重试；其他
`4xx` 通常意味着配置或接收端错误，因此不会重试。重试次数上限为 10 次，单次
退避等待最长为 30 秒。非 loopback endpoint 应始终使用 HTTPS；远程 Webhook
配置为明文 HTTP 时，CubeAPI 会记录告警。
