---
title: "Lenovo Cloud Agent：从 Daytona 到 CubeSandbox 的沙箱迁移"
author: 李健
date: 2026-08-20
tags:
  - agent
  - migration
  - daytona
  - e2b-compat
lang: zh-CN
---

# Lenovo Cloud Agent：从 Daytona 到 CubeSandbox 的沙箱迁移

## 业务背景

联想研究院 AI Lab 的云端 Agent 产品经历了两个阶段：1.0 把沙箱当作"每次会话起一个、用完即弃"的隔离执行环境；2.0 把守护进程和 Agent 整体放进沙箱，让沙箱成为一个可以备份、可以回退的独立运行节点。早期使用 Daytona 沙箱，因启动慢（>10 秒）、网络受限（需 VPN）、SaaS 成本三个问题叠加，开始迁移到 Cube Sandbox。

## 核心痛点

- **API 不兼容**：Daytona 和 E2B（Cube 兼容）的 API 不兼容，无法简单替换。
- **启动性能**：Daytona 沙箱启动超过 10 秒，优化后仍 5 秒，批量并发场景下成为瓶颈。
- **Volume 共享写入**：Cube 早期版本不支持多沙箱共同修改同一挂载目录。
- **服务器重启后沙箱丢失**：2.0 模式下长期运行沙箱，重启后未做快照的沙箱消失。

## 基于 Cube Sandbox 的方案

迁移分三步走：解除与 Daytona 的强绑定 → 做 SDK 适配层（同时支持 Daytona 和 E2B 接口）→ 对接 Cube。适配层定义统一的 Sandbox Provider 基类，上层业务代码不感知底层差异。

启动性能：从 10 秒以上压缩到 100 毫秒以下。Volume 共享写入问题在 v0.6.0 已修复。跨机恢复计划在 v0.7.0 推出。

## 效果与收益

- 启动速度从 >10 秒压到 <100 毫秒，用户无感等待。
- SaaS 费用省掉，VPN 问题消失。
- 本地部署后调试体验大幅提升。
- 沙箱角色从"工具执行容器"演进为"独立运行环境"。

## 参考资料

- 完整案例文章：[从 Daytona 到 CubeSandbox：联想云端 Agent 的沙箱迁移与演进](/zh/blog/posts/2026-08-20-lenovo-cloud-agent)
- Cube Sandbox 源码：[TencentCloud/CubeSandbox](https://github.com/TencentCloud/CubeSandbox)
