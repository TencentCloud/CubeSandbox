---
title: Go SDK Connect 流返回 HTML 或文本响应
author: tokove
date: 2026-09-24
tags:
  - sdk
  - networking
  - cube-proxy
lang: zh-CN
---

# Go SDK Connect 流返回 HTML 或文本响应

## 问题现象

调用 `Commands.Run`、`Files.WatchDir` 或 `Pty.Create` 等 Go SDK 接口时，出现 HTML/文本响应、无效 Connect 信封或消息过大的错误。实际返回内容可能是浏览器登录页、网关错误页、JSON 响应或被压缩的响应，而不是 Connect 流数据。

## 适用环境

- 通过 CubeProxy 发起的 Go SDK Connect 流式调用
- 数据面配置：`CUBE_PROXY_NODE_IP`、`CUBE_PROXY_PORT_HTTP` 和 `CUBE_SANDBOX_DOMAIN`

## 根因分析

SDK 的数据面请求必须到达 CubeProxy 的流式端点。如果未设置 `CUBE_PROXY_NODE_IP`、配置了错误的节点，或代理路由被 Web 服务或网关替代，返回内容就不是 Connect 帧格式。

## 解决方案

1. 将 `CUBE_PROXY_NODE_IP` 设置为客户端可访问的 CubeProxy 节点 IP：

   ```bash
   export CUBE_PROXY_NODE_IP=<cubeproxy-node-ip>
   ```

2. 确认 `CUBE_PROXY_PORT_HTTP` 和沙箱域名与 CubeProxy 配置一致。
3. 确认请求发送到了数据面端点，并检查中间网关没有重写或压缩流式响应。
4. 重试 SDK 调用。HTML 页面和 404 文本页会附带端点配置提示；其他文本错误保留服务器原始消息。

远程客户端还需要确认到 CubeProxy 节点的防火墙规则和网络路由。SDK 若运行在 CubeProxy 主机上，可按实际部署使用本机节点地址。

## 参考资料

- 关联 Issue：[#1759](https://github.com/TencentCloud/CubeSandbox/issues/1759)
