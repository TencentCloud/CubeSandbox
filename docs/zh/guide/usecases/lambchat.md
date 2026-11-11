---
title: "LambChat：Agent 平台沙箱迁移到 CubeSandbox 的选型与实录"
author: Clivia
date: 2026-10-15
tags:
  - agent
  - e2b-compat
  - pause-resume
  - multi-tenant
lang: zh-CN
---

# LambChat：Agent 平台沙箱迁移到 CubeSandbox 的选型与实录

## 业务背景

LambChat 是一个集成 Skills 能力、支持多租户的 Agent 系统，后端基于 deepagents / LangGraph。核心概念是"数字空间"：每个用户绑定一个常驻沙箱，跨会话持续存在，用户在里面装包、传文件、让 Agent 自由折腾。沙箱层同时支持 E2B、CubeSandbox、Daytona 三种后端，切换只需改配置一个字段。

## 核心痛点

- **隔离是硬需求**：用户自由折腾意味着不能让一个用户的操作影响其他用户或宿主系统。
- **用户级持久沙箱的生命周期**：空闲要自动 pause、回来要自动 resume，内存态不能丢。
- **控制面/数据面状态不一致**：resume 后控制面已显示 running，数据面 envd 未就绪返回 504。
- **bash -lc 登录 shell**：e2b SDK 默认每次执行都起新登录 shell，两阶段任务的环境变量不延续。

## 基于 CubeSandbox 的方案

- **继承式适配**：CubeSandboxBackend 继承 E2BBackend 只覆盖差异（ls 用内联 Python 实现、get_info 做 camelCase/snake_case 双向兼容、pause 独有、list_user_sandboxes 按 metadata.user_id 枚举），适配代码约 200 行。
- **用户级持久绑定 + 四级查找链**：内存 LRU 缓存 → MongoDB 绑定重连（sandbox.connect()）→ 枚举已有沙箱复用 → 新建；另有第五级兜底：发现同用户多沙箱时异步清理只留一个。
- **AutoPause 配置**：创建时传 on_timeout=pause + auto_resume=true，空闲自动暂停、请求自动恢复。
- **504 容错**：健康检查同时打控制面状态和数据面轻量请求，收到 504 判定不健康，回退到另一个沙箱或新建。
- **登录 shell 规避**：两阶段任务之间显式重设需要延续的环境变量，不依赖 shell 状态。

## 效果与收益

- 从默认后端切到 CubeSandbox 共约 300 行代码、一天完成——前提是 E2B 协议兼容可直接继承基类。
- KVM 硬件级隔离 + pause/resume 内存态保留，支撑"一个用户一个常驻沙箱"的数字空间模型。
- 沉淀 5 条选型建议：先定绑定粒度、E2B 兼容性是杠杆、resume 状态不一致要当异常处理、提前知道 bash -lc 问题、差异化能力（快照/模板/网络策略）可后补。

## 参考资料

- 完整案例文章：[换一个沙箱后端，要改多少代码？Agent 平台沙箱迁移的选型与实录](/zh/blog/posts/2026-10-15-lambchat-migration)
- Cube Sandbox 源码：[TencentCloud/CubeSandbox](https://github.com/TencentCloud/CubeSandbox)
