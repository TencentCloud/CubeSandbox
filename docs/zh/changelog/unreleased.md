---
title: 未发布变更
---

# 未发布变更

## Pause / 生命周期

- 未关闭的 exec 会话导致 Pause 被拒绝时，沙箱不再卡在 `unknown(failed)`。Cubelet 返回 `TaskPauseFailed`（130588）且沙箱保持 running；CubeMaster 中止 CREATING pause 绑定，便于 auto-pause 重试。Cubelet 通过 CubeShim `CheckPauseAllowed` 在克隆内存卷前做 exec 预检。**Cubelet、CubeMaster 与 CubeShim 需一起升级**。关联 [#2270](https://github.com/TencentCloud/CubeSandbox/pull/2270)。

## Go SDK

- Connect 流式调用收到 HTML 页面、404 文本页或 JSON、整段 gzip 等无效 Connect 帧时，现在会报告路由诊断信息。标记为 `text/html` 的 HTTP 200 响应会在读取流内容前被拒绝；其他非 200 文本错误保留服务器原始消息。非 200 的 HTML 及 404 文本错误消息在追加诊断提示前会被截断至 200 字符。
- Connect 调用收到 HTML 错误页时会报告路由错误。非 JSON 的 `text/*` 响应仅在 HTTP 404 时参与路由判断。文件系统 RPC（`Files.List`/`Stat`/`Exists`/`Remove`/`Rename`/`MakeDir`）也会识别 HTTP 200 返回的 HTML 路由页面。路由错误会清除未找到类错误分类，401/403 响应保留 `ErrAuthentication`。即使标记为 `text/*`，JSON API 错误文档也保留原有分类。详见 [Connect 流路由排障指南](../guide/troubleshooting/sdk-connect-stream-routing.md)。
- **不兼容行为变更。** `Pty.Kill` 收到 HTTP 404 HTML 路由页面时会返回错误。服务端确认进程不存在时仍返回 `(false, nil)`。
- 文件系统 HTTP 200 路由检测识别 `text/html`，或去除前导空白后以 `<!doctype` / `<html` 开头的正文，前缀匹配忽略大小写，并保留 JSON API 错误文档的现有例外。缺少这些 HTML 特征的纯文本或 JSON 登录页会继续按正常响应处理。
- HTTP 错误诊断最多保留 64 KiB 的响应正文。API 错误解析器可能额外读取一个字节以检测截断。PTY 一元调用和文件系统 RPC 遇到 HTTP 错误时最多读取 64 KiB。更大的错误文档可能在 JSON 解码前被截断。成功的文件系统响应仍完整读取。
- 对未跟随的重定向，Connect 流的诊断信息只包含截断后的重定向路径。
- `RunCode` 使用独立端点，不在本次改动范围内。关联 [#1759](https://github.com/TencentCloud/CubeSandbox/issues/1759)。
