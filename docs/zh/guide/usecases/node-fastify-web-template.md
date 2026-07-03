---
title: "面向有状态沙箱 Web 应用的 Node.js Fastify 模板"
author: Alpaca233114514
date: 2026-07-03
tags:
  - nodejs
  - fastify
  - web-template
  - snapshot
lang: zh-CN
---

# 面向有状态沙箱 Web 应用的 Node.js Fastify 模板

## 业务背景

很多 Agent 和开发者工具工作流并不只需要一次性的 Code Interpreter。它们还需要能运行长生命周期 Web 服务、暴露 HTTP 端口、保留工作区状态，并继续支持 CubeSandbox pause / resume 等生命周期能力的沙箱。

`examples/node-fastify-web-template` 示例展示了如何把这个模式封装成可复用的 Node.js 模板。模板在 `3000` 端口运行 TypeScript Fastify API，同时在 `49983` 端口保留 CubeSandbox `envd`，用于 SDK 访问。

## 核心痛点

- **双进程运行时**：模板既要保持 `envd` 运行以支持 SDK 操作，也要启动用户自己的 Web 服务。
- **端口暴露**：注册模板时需要同时暴露 SDK 控制端口和应用端口。
- **状态持久化**：应用数据应写入沙箱工作区，才能被快照和恢复流程保留。
- **运行可复现**：模板应能通过 Docker 构建，并在注册到 CubeSandbox 前完成验证。
- **异常处理**：非法 JSON、无效请求体、损坏的状态文件都需要确定性的行为，不能只覆盖 happy path。

## 基于 Cube Sandbox 的方案

该示例基于 `node:24-bookworm-slim` 构建 Docker 镜像，并从 `ghcr.io/tencentcloud/cubesandbox-base:2026.16` 复制 `envd`。容器入口先在 `49983` 端口启动 `envd`，再运行编译后的 Fastify 服务：

```bash
docker build -t cube-node-fastify-web:latest .

cubemastercli -a 127.0.0.1 -p 8089 tpl create-from-image \
  --image cube-node-fastify-web:latest \
  --writable-layer-size 1G \
  --expose-port 49983 \
  --expose-port 3000 \
  --probe 49983 \
  --probe-path /health
```

应用内部把运行状态写入 `/workspace/state`：

- `POST /api/counter` 递增持久化 counter 文件。
- `POST /api/write-note` 追加 JSONL notes。
- `GET /api/info` 返回 Node.js 运行时和工作区路径。

Python demo 使用 CubeSandbox 暴露的 E2B 兼容 SDK 入口。`run_demo.py` 创建沙箱、解析 `3000` 端口对应的 Web host、调用 Fastify API 并写入状态。`snapshot_resume.py` 暂停并重新连接同一个沙箱，然后验证 counter 会从暂停前的值继续递增。

## 效果与收益

- **可复用的 Web 模板模式**：团队可以从最小 Node.js + Fastify 示例开始，而不必从零处理 `envd`、端口和 SDK 接入。
- **有状态 Web 应用验证**：counter 和 note 接口让工作区持久化能力可见且易测。
- **覆盖快照恢复链路**：demo 验证 Web 服务状态可以跨 pause / resume 保留。
- **非 happy-path 测试**：本地测试覆盖损坏状态、非法请求体、非法 JSON，以及随机端口上的真实 listener。
- **已在真实 CubeSandbox 环境验证**：模板已注册为 `tpl-03f52e94be8c48ca8ef68dee`，并通过 Kimi K2.7 辅助的半自动真机测试完成端到端验证。

## 参考资料

- 示例源码：`examples/node-fastify-web-template`
- 示例索引：[CubeSandbox 示例项目](../tutorials/examples.md)
- 兼容协议：[E2B Sandbox](https://e2b.dev)
