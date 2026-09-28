---
title: "WeKnora：基于 CubeSandbox 的 Agent 持久化运行环境"
author: 陈洋、赵海龙
date: 2026-09-17
tags:
  - agent
  - snapshot
  - session-persistence
  - e2b-compat
lang: zh-CN
---

# WeKnora：基于 CubeSandbox 的 Agent 持久化运行环境

## 业务背景

WeKnora 是腾讯开源的企业级 LLM 知识平台（24.4k stars），把企业文档资产转化为 RAG 问答、ReAct 智能体和自维护 Wiki。v0.8.0 的核心特性是技能沙箱运行时：会话级常驻沙箱，按空间配置网络策略。沙箱层同时支持 Cube Sandbox、E2B、Docker 三种后端，其中 Cube Sandbox 跑通了会话绑定、技能快照、暂停恢复、模板管理和网络策略的完整链路。

## 核心痛点

- **会话状态缺失**：最初 Docker `docker run --rm` 跑完即毁，上一轮装的包下一轮就丢，附件暂存、产物收集等能力注册不上。
- **技能安装成本高**：装一个技能要解析 SKILL.md、装系统包和依赖、跑验证，是数分钟级的 agent 对话，不能摊到每个会话上。
- **共享内核隔离不够**：运行模型生成代码的场景需要内核级隔离、跨主机调度、内存态快照，Docker 都给不了。
- **绑定与回收的应用语义**：Cube 的 TTL/AutoPause 管 MicroVM 寿命，但不知道会话、租户、副本、技能代际，这些语义要应用层自己兜。

## 基于 CubeSandbox 的方案

- **中立接口 + 能力广告**：RemoteSandboxClient 六个生命周期方法 + 四个辅助接口，Cube 特有的类型/错误码/HTTP 语义全部翻译成中立 DTO，三种后端上层业务代码一行不改可互换。
- **技能快照化（最深集成点）**：从基础模板创建沙箱 → 安装技能 → 校验 → CreateSnapshot，新会话直接用快照 ID 当 TemplateID 创建，技能秒级就位；指纹机制 SHA-256(provider+APIKey+APIURL) 守门，凭据轮换后旧快照静默失效、回退基础模板。
- **会话保活**：onTimeout=pause + autoResume，空闲冻结 MicroVM 保内存态，下一轮 Connect 自动唤醒；/workspace 经 envd Files API 收成会话文件系统（附件 input/ 只读、产物 output/ 收集、跨 tool call 工作区）。
- **Redis 绑定层**：会话→沙箱权威绑定（SET NX 永不过期）+ 生命周期锁跨进程串行 + reaper 按 tenant metadata 定期对账回收孤儿。
- **轮次租约**：StaleAt 声明镜像已换但不动手，BeginTurn 时 rebuild=1 允许本轮第一次 resolve 拆建并立刻消费，解决"管理员装技能 vs 用户正在对话"抢同一台 VM 的冲突。
- **网络双开关显式定值**：allowInternetAccess 管出站、allowPublicTraffic 管入站，创建时显式设置防默认漂移。

## 效果与收益

- 三种后端（Docker/E2B/Cube）在统一抽象下可互换，Cube 独有适配收敛在单个 cube_remote_client.go 文件。
- 技能环境冻结为不可变"发行版"，新会话从快照启动秒级就位，会话体感"对话还在、环境还在"。
- 总结 8 项 Agent 平台沙箱验证清单（快照即模板、运行中打快照、pause/resume 语义、metadata 认领、网络双开关、envd 契约、模板带 envd、分页与幂等）。
- 向社区反馈 4 项架构级诉求：Create 挂 volume、快照与模板分目录、pause/超时回调、会话身份一等公民。

## 参考资料

- 完整案例文章：[WeKnora 基于 CubeSandbox 的 Agent 持久化运行环境建设](/zh/blog/posts/2026-09-17-weknora)
- WeKnora 项目地址：[Tencent/WeKnora](https://github.com/Tencent/WeKnora)
- Cube Sandbox 源码：[TencentCloud/CubeSandbox](https://github.com/TencentCloud/CubeSandbox)
