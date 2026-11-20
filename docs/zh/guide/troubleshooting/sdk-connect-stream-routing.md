---
title: SDK Connect 流返回 HTML 或文本响应
author: tokove
date: 2026-09-24
tags:
  - sdk
  - networking
  - cube-proxy
lang: zh-CN
---

# SDK Connect 流返回 HTML 或文本响应

## 问题现象

调用读取 Connect 流的 SDK 接口时，出现 HTML/文本响应、无效 Connect 信封或消息过大的错误：

- Go SDK：`Commands.Run`、`Files.WatchDir`、`Pty.Create`、`Pty.Connect`
- Python SDK：`commands.run`、`files.watch_dir`、`pty.create`、`pty.connect`
- Node SDK：`commands.run`、`files.watchDir`、`pty.create`、`pty.connect`

常见报错为 `received HTML instead of a Connect response`、`response is not a Connect envelope (first 5 bytes: ...)`，或 HTTP 错误后附带 `response may be an HTML/text page instead of a Connect stream`。不带该诊断的旧版 SDK 会报 `Connect stream message too large: 558124867 bytes`，即 HTML 前缀 `<!DOC` 被当作信封头解析。实际返回内容可能是浏览器登录页、网关错误页、JSON 响应或被压缩的响应，而不是 Connect 流数据。

已知限制：`RunCode` 使用独立的 Jupyter `/execute` 端点，暂未被该诊断覆盖——误路由的 200 HTML 页面被喂给其流解析器后，表现为返回空结果且无错误（见 [#1759](https://github.com/TencentCloud/CubeSandbox/issues/1759)）。纯 HTTP 的 `Files.Read`/`Files.Write` 路径同样未覆盖：误路由的 200 HTML 页面会被 `readFile` 当作文件内容返回，`writeFile` 则把任何低于 400 的状态码都视为成功。Python SDK 同样如此：`files.read` 和 `files.write` 走纯 `/files` HTTP 请求，误路由的 200 HTML 页面会被 `files.read` 当作文件内容返回。Node SDK 也有同样的限制：`runCode` 未覆盖，`files.read` 会把 200 HTML 页面当作文件内容返回，`files.write` 把任何低于 400 的状态码都视为成功。

Node SDK 的报错以失败的调用开头：`commands.run` 为 `command failed: ...`，`files.watchDir` 为 `WatchDir failed: ...`，`pty.create` / `pty.connect` 为 `Start failed: ...` / `Connect failed: ...`。报错中引用的 HTML 页面内容最多保留 200 字节，错误响应体最多读取 64 KiB（更长时以 ` [truncated]` 结尾），其中的控制字符会被替换为 U+FFFD。

## 适用环境

- 通过 CubeProxy 发起的 Go、Python 和 Node SDK Connect 流式调用
- 数据面配置：`CUBE_PROXY_NODE_IP`、`CUBE_PROXY_PORT_HTTP` 和 `CUBE_SANDBOX_DOMAIN`

## 根因分析

SDK 的数据面请求必须到达 CubeProxy 的流式端点。当设置了 `CUBE_PROXY_NODE_IP` 时，它必须指向客户端可达的 CubeProxy 节点；未设置时，客户端必须能将 `*.<CUBE_SANDBOX_DOMAIN>` 解析到 CubeProxy。若两者都不成立，或路由被 Web 服务或网关接管，返回内容就不是 Connect 帧格式。

## 解决方案

1. 如果设置了 `CUBE_PROXY_NODE_IP`，请确认它指向客户端可达的 CubeProxy 节点 IP：

   ```bash
   export CUBE_PROXY_NODE_IP=<cubeproxy-node-ip>
   ```

   未设置时，请确认客户端能将 `*.<CUBE_SANDBOX_DOMAIN>` 解析到 CubeProxy。

2. 确认 `CUBE_PROXY_PORT_HTTP` 和沙箱域名与 CubeProxy 配置一致。
3. 确认请求发送到了数据面端点，并检查中间网关没有重写或压缩流式响应。
4. 重试 SDK 调用。HTML 页面和 404 文本页会附带端点配置提示；其他文本错误保留服务器原始消息。

远程客户端还需要确认到 CubeProxy 节点的防火墙规则和网络路由。SDK 若运行在 CubeProxy 主机上，可按实际部署使用本机节点地址。

## 参考资料

- 关联 Issue：[#1759](https://github.com/TencentCloud/CubeSandbox/issues/1759)
