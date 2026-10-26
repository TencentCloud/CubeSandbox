---
title: "Hermes Agent：在 Cube Sandbox 中运行常驻 Agent 平台"
author: 陈金博
date: 2026-08-20
tags:
  - agent
  - persistence
  - skills
  - host-mount
lang: zh-CN
---

# Hermes Agent：在 Cube Sandbox 中运行常驻 Agent 平台

## 业务背景

上海阳璞新能源科技 AI 团队将内部 Agent 运行平台 Hermes Studio 迁上 Cube Sandbox。一个常驻 Agent 应用需要沙箱记住的不只是任务产物，还包括 Agent 自身的配置、历史会话、以及一批被反复调用的 Skills。而沙箱本身天然是易失的——重启、暂停、迁移都可能让这些状态归零。

## 核心痛点

- **持久化**：Hermes home（`/root/.hermes`）和 workspace（`/workspace`）需要跨沙箱重建保留。
- **Skills 分层**：公共 Skills 需要统一维护，私有 Skills 不能被覆盖。
- **网络放行**：局域网模型服务需要单独配置出站 CIDR。
- **暂停恢复**：TAP 设备 EBUSY 导致暂停后恢复失败。

## 基于 Cube Sandbox 的方案

以 Cubelet 节点上的持久化目录作为唯一存储底座，通过 `metadata["host-mount"]` 将 Hermes home、workspace 和公共 Skills 分别挂载进沙箱。公共 Skills 采用"只读共享目录 + 私有目录符号链接"的覆盖层设计。

| 内容 | Cubelet 目录 | 沙箱路径 | 权限 |
|---|---|---|---|
| Hermes home | `/data/shared/hermes-homes/<home-id>` | `/root/.hermes` | 读写 |
| Workspace | `/data/shared/hermes-workspaces/<workspace-id>` | `/workspace` | 读写 |
| 公共 Skills | `/data/shared/hermes-common-skills` | `/opt/hermes-common-skills` | 只读 |

## 效果与收益

- 单节点接入数十个 Hermes 沙箱，持续运行约一个月。
- 持久化边界明确，沙箱重建后状态可恢复。
- Skills 集中维护，私有版本优先。
- 已提交 Issue [#953](https://github.com/TencentCloud/CubeSandbox/issues/953) 跟踪 TAP EBUSY 问题。

## 参考资料

- 完整案例文章：[在 Cube Sandbox 中运行 Hermes Agent](/zh/blog/posts/2026-08-20-hermes-agent)
- Cube Sandbox 源码：[TencentCloud/CubeSandbox](https://github.com/TencentCloud/CubeSandbox)
