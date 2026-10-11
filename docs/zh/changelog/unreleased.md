---
title: 未发布变更
---

# 未发布变更

## Go SDK

- Connect 流式调用收到 HTML 页面、404 文本页或 JSON、整段 gzip 等无效 Connect 帧时，现在会报告路由诊断信息。标记为 `text/html` 的 HTTP 200 响应会在读取流内容前被拒绝；其他非 200 文本错误保留服务器原始消息。非 200 的 HTML 及 404 文本错误消息在追加诊断提示前会被截断至 200 字符。
- 对 Connect 流式调用，404 HTML 与文本响应会被视为路由错误，不再匹配 `errors.Is(err, ErrSandboxNotFound)`。JSON 404 响应以及模板、卷不存在的错误分类保持原有行为。出现该诊断时，请检查 `CUBE_PROXY_NODE_IP` 并参阅 [Connect 流路由排障指南](../guide/troubleshooting/sdk-connect-stream-routing.md)。
- `RunCode` 使用独立端点，不在本次改动范围内。关联 [#1759](https://github.com/TencentCloud/CubeSandbox/issues/1759)。
