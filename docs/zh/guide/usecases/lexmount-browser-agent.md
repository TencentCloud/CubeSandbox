---
title: "Lexmount AI：把浏览器运行时搬进 Agent 沙箱"
author: 熊袖璋
date: 2026-08-13
tags:
  - agent
  - browser
  - browser-runtime
  - production
lang: zh-CN
---

# Lexmount AI：把浏览器运行时搬进 Agent 沙箱

## 业务背景

Lexmount 自研的 Agent 专属浏览器运行时（Lexmount Insight Flow），核心应用场景是让 AI 智能体大规模、稳定地执行网页任务。这个业务形态天然对底层沙箱提出了几条"必须成立"的要求：能真实访问互联网、能批量并发拉起、能给每个沙箱注入不同的运行时状态、能准确知道单机能起多少个。

选型阶段，团队在开源社区寻找"能轻量起大量沙箱"的工具，实测对比过 OpenSandbox 和 Cube Sandbox 后，发现 Cube 的资源占用更低，因此最终选择了后者。从 v0.1.0 跟进到 v0.5.1，围绕四条边界，团队分别碰上了公网不通、模板创建失败、环境变量丢失、资源虚假超卖四个坑。

## 核心痛点

Agent 浏览器运行时是一个复合工作负载，对沙箱有四个层面的硬需求：

- **出站网络**：Agent 打开的每个页面、每次调用的每个 API，都要从沙箱里出到公网。
- **批量启动**：多用户并行、RL batch rollout 场景下，一台机器上会短时间内密集触发沙箱创建请求。
- **动态状态注入**：不同用户、不同任务的 API Key、上下文标签都不同，需要按需注入。
- **真实容量感知**：浏览器运行时是重资源工作负载，需要准确知道单机能跑多少个实例。

## 基于 Cube Sandbox 的方案

团队把沙箱化拆成两条路径，由业务编排层按风险等级做策略路由：

1. **只隔离 Bash**：低风险短执行场景，Agent Loop 保留在业务编排层，只有 `Bash/exec` 工具调用进入沙箱。沙箱用完即弃，内部无状态。
2. **整个 Agent Runtime 放进沙箱**：高风险重资源场景（浏览器自动化、长时任务），Agent 常驻进程整体进入沙箱，享有独立 CPU/内存和持久工作区。

在接入 Cube Sandbox 的过程中，团队逐一解决四个问题：

- **出站网络**：发现 `getGatewayMacAddr()` 在多邻居环境下选错网关 MAC，导致 SYN 包送到错误的 next hop。通过 `tcpdump` 实锤后修复，PR [#224](https://github.com/TencentCloud/CubeSandbox/pull/224) 已合并。
- **批量启动**：发现启动脚本在 network-agent `/readyz` 未通过时就拉起 Cubelet，导致 Images 服务不可用。修复为等 `/readyz` 通过后再启动，PR [#304](https://github.com/TencentCloud/CubeSandbox/pull/304) 已合并。
- **动态状态注入**：发现 `CreateSandboxRequest.containers` 被硬编码为 `vec![]`，环境变量被丢弃。提交 PR [#634](https://github.com/TencentCloud/CubeSandbox/pull/634) 后，确认这是 snapshot restore 架构约束而非底层缺陷。改用"控制面代理、沙箱内执行"的两段式设计补齐。
- **真实容量**：发现 v0.2.x 的 mock 资源指标导致容量虚高，v0.3.0 已修复。总结出"容量必须在业务负载真实压上去的条件下测"的原则。

## 效果与收益

- **公网连通**：修复 L2 next hop 选择问题，浏览器运行时获得稳定出站网络。
- **批量并发**：修复启动时序竞态，单机可稳定批量拉起沙箱实例。
- **状态注入**：两段式设计在 snapshot restore 架构约束下实现 per-sandbox 状态注入，热启动 P95 亚秒级。
- **容量准确**：消除 mock 指标干扰，容量评估基于真实负载和多源交叉验证。

## 参考资料

- 完整案例文章：[把浏览器搬进 Agent 沙箱：Lexmount 在 CubeSandbox 上的一线实战](/zh/blog/posts/2026-08-13-lexmount-browser-agent)
- Cube Sandbox 源码：[TencentCloud/CubeSandbox](https://github.com/TencentCloud/CubeSandbox)
