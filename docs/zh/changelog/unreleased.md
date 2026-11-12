---
title: 未发布变更
---

# 未发布变更

## Go SDK

- Connect 流式调用收到 HTML 页面、404 文本页或 JSON、整段 gzip 等无效 Connect 帧时，现在会报告路由诊断信息。标记为 `text/html` 的 HTTP 200 响应会在读取流内容前被拒绝；其他非 200 文本错误保留服务器原始消息。非 200 的 HTML 及 404 文本错误消息在追加诊断提示前会被截断至 200 字符。
- Connect 调用收到 HTML 错误页时会报告路由错误。非 JSON 的 `text/*` 响应仅在 HTTP 404 时参与路由判断。文件系统 RPC（`Files.List`/`Stat`/`Exists`/`Remove`/`Rename`/`MakeDir`）也会识别 HTTP 200 返回的 HTML 路由页面。路由错误会清除未找到类错误分类，401/403 响应保留 `ErrAuthentication`。即使标记为 `text/*`，JSON API 错误文档也保留原有分类。详见 [Connect 流路由排障指南](../guide/troubleshooting/sdk-connect-stream-routing.md)。
- `Pty.Kill` 收到 HTTP 404 HTML 路由页面时会返回错误。服务端确认进程不存在时仍返回 `(false, nil)`。
- 对未跟随的重定向，Connect 流的诊断信息只包含截断后的重定向路径。
- `RunCode` 使用独立端点，不在本次改动范围内。关联 [#1759](https://github.com/TencentCloud/CubeSandbox/issues/1759)。
