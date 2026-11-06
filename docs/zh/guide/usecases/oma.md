---
title: "OMA：云端托管智能体平台的架构与安全执行面"
author: 张师傅的博客
date: 2026-11-06
tags:
  - agent
  - managed-agents
  - security
  - architecture
lang: zh-CN
---

# OMA：云端托管智能体平台的架构与安全执行面

## 业务背景

OMA 是一个自托管的云端托管智能体（Managed Agents）平台，参考 Anthropic 的 Claude Managed Agents 设计，在企业自己的基础设施上提供会话管理、推理与执行解耦、凭证托管和文件系统能力，业务团队通过 API 定义和使用 Agent。云端沙箱采用腾讯开源的 CubeSandbox，本地开发使用自研的 e2b-local（Go 编写的 E2B 兼容网关，跑在 Docker / OrbStack VM / Apple Container 上）。

## 核心痛点

- **模型 API 到生产的鸿沟**：上下文管理、工具执行、会话持久化、凭证管理、沙箱隔离、可观测性都要应用团队自建。
- **沙箱内一体化的短板**：运行框架和执行环境同放沙箱，首次响应要等沙箱+Agent 进程启动，沙箱故障还会拖垮推理进程与会话状态。
- **凭证外传风险**：生成代码若能读到长期凭证，就存在复制或外传风险。
- **NAS/ossfs 不契合沙箱文件需求**：配额瓶颈（单文件系统最多 1000 个挂载节点）和"任务应该看到哪些文件"的视图问题。
- **私网工具触达**：云端 Agent 调用企业内部 MCP Server 受阻于防火墙。

## 基于 CubeSandbox 的方案

- **手脑分离**：Brain（DeepSeek Harness 实现的智能体循环）放在沙箱外，通过 CCRv2 协议与平台交换输入/输出/审批/会话状态；沙箱只承担工具执行（内部 Rust MCP Server）。沙箱可并行准备或按需创建，Anthropic 报告中该设计 p50 TTFT 降低约 60%、p95 降低超 90%。
- **沙箱适配层**：云端 CubeSandbox（PVM 支持适配云服务器、eBPF 网络、实现轻量），本地 e2b-local，同一套接口复用调用逻辑；保留独立适配层持续评估底层方案。
- **凭证托管**：长期凭证留在平台控制面（Vault/KMS 保护，密文与解密能力分开管理），沙箱只持有会话身份，真实凭证由出口网关在请求上游时注入。
- **FUSE 用户态文件系统**：按 Session 构建虚拟视图（mount_path 规范化、Skill 组合、只读/可写边界），文件操作经平台统一鉴权与记录。
- **MCP Tunnel 反向建连**：私网客户端（OpenAI 开源 tunnel-client）主动出站连云端，管理 API 对齐 Claude Managed Agents Tunnels 协议。

## 效果与收益

- 推理进程、执行环境、会话状态拥有各自生命周期：沙箱故障可重建，Brain 重启可按会话 ID 恢复。
- 长任务恢复、凭证安全、文件持久、私网工具、过程追溯五类共性问题由平台集中承担。
- 时间线 + 全局视图支撑按团队和场景的用量与失败分析。

## 参考资料

- 完整案例文章：[Managed Agents 云端智能体托管平台：从跑通 Agent 到稳定交付](/zh/blog/posts/2026-11-06-oma-managed-agents)
- Cube Sandbox 源码：[TencentCloud/CubeSandbox](https://github.com/TencentCloud/CubeSandbox)
