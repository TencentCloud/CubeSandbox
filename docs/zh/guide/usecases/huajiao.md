---
title: "花椒：沙箱是选项不是标配的 Agent 平台实践"
author: 王成龙、封冰清
date: 2026-09-17
tags:
  - agent
  - host-mount
  - go-sdk
  - execution-environment
lang: zh-CN
---

# 花椒：沙箱是选项不是标配的 Agent 平台实践

## 业务背景

花椒直播（花房集团旗下）的企业 Agent 平台先在花椒直播业务侧完成 0 到 1 验证，随后基于同一套 Agent Runtime 为花房集团中台独立部署一套实例。两套部署合计落地约 30 个 Agent 应用（DMG 业务助手、ChatBI、内容运营助手、法务工作台、GR 工作台等），日均 token 消耗约 8300 万。CubeSandbox 已在生产环境运行 3 个多月。

## 核心痛点

- **多用户物理隔离**：早期单服务器目录隔离，用户 A 理论上可以通过 Agent 操作到用户 B 的文件。
- **用户数据跨沙箱持久化**：沙箱可回收，但用户工作状态必须保留；普通数据盘无法同时挂多台云服务器。
- **强隔离 + 快启动**：Docker 启动慢、隔离不彻底；传统虚拟机成本高、资源分配不灵活。
- **终端语义自主可控**：平台需要一套自定义、可长期独立演进的终端协议，不能把 runtime 会话模型绑定在外部沙箱 API 语义上。

## 基于 CubeSandbox 的方案

- **执行环境抽象层**：平台最关键的一层抽象是"执行环境"，CubeSandbox 是接进执行环境服务的一种沙箱方案；隔离决策不在平台层统一，由每个 Agent 按"资源要不要在多用户间物理隔离"自行决定。组织之间（花椒 Agent / 花房 Agent）采用两套完全独立实例，不共享控制面。
- **NFS + host-mount 持久化**：用户持久化数据全部写到 NFS 目录，云服务器故障时新机器挂同一 NFS 继续服务；执行环境守护进程通过 host-mount 把目录映射进沙箱，一个会话对应一个沙箱。
- **深度社区共建**：贡献了官方 Go SDK（[PR #254](https://github.com/TencentCloud/CubeSandbox/pull/254)，15 文件 3288 行），修复连接池误关问题（[PR #322](https://github.com/TencentCloud/CubeSandbox/pull/322)），并定位 host-mount 场景快照恢复时 virtio-fs 根 inode 丢失迁移状态报 InvalidVirtioFsState 的根因（[PR #341](https://github.com/TencentCloud/CubeSandbox/pull/341)）。
- **自研 agentd 终端协议**：跑在 49984 端口，与 envd（49983）并存分工，作为模板主命令（PID 1）启动；提供 sessions/stdin/resize/healthz 等窄 HTTP API，带单调 chunk_id 的 ring buffer 增量轮询输出，是平台 exec_command / write_stdin 的唯一执行数据面，抹平底层执行环境类型差异。

## 效果与收益

- 同一套 Agent Runtime 支撑两套独立组织部署，约 30 个 Agent 应用在生产运行。
- 资源认知纠偏沉淀为实践原则：沙箱是高性能强隔离虚拟机，不是按用量弹性伸缩的资源池；测试/线上分层规划规格，配合闲置回收和监控告警动态调整。
- Go SDK 与多个修复合入上游，host-mount 快照恢复问题当前以"销毁重建"绕开未解决部分。

## 参考资料

- 完整案例文章：[沙箱是选项，不是标配：花椒 Agent 平台的架构思考与 Cube 实践](/zh/blog/posts/2026-09-17-huajiao)
- Cube Sandbox 源码：[TencentCloud/CubeSandbox](https://github.com/TencentCloud/CubeSandbox)
