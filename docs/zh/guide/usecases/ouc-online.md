---
title: "国开在线：Agent 代码执行层的会话隔离与生命周期实践"
author: 陆昊洋
date: 2026-11-06
tags:
  - agent
  - session-isolation
  - lifecycle
  - pause-resume
lang: zh-CN
---

# 国开在线：Agent 代码执行层的会话隔离与生命周期实践

## 业务背景

国开在线（国家开放大学在线教育科技有限公司）正在开发一款面向教育与办公场景的类 Manus Agent 平台，支持用户自定义 Agent、Skill、Tool、知识库和 MCP，典型任务包括知识库生成 XLSX 报表、PPTX 课件生成与校验、文档解析再加工、脚本化数据分析和多 Agent 协作。平台分五层：lea-agent（LLM 主循环）、lea-config（配置管理）、lea-tools（工具网关 + 沙箱会话管理）、CubeSandbox（隔离执行）、NFS/OBS（会话工作区与交付物存储）。目前处于评估期，尚未上生产。

## 核心痛点

- **不可信代码的执行边界**：任意 Python/Shell/Node 脚本都可能被模型生成执行，普通容器/共享虚机不适合充当边界。
- **生命周期的两难**：会话沙箱用完立即删除会让下一条消息重付冷启动；长期保留又持续占用沙箱槽位和宿主资源。
- **多副本下的回收误删风险**：应用本地时间戳只代表单个副本见过的流量，服务重启还会归零。
- **按文档推荐资源部署直接卡死**：4 核 8G/16G 机器在 `modprobe kvm_pvm` 加载 PVM KVM 模块时系统级 OOM 卡死。

## 基于 CubeSandbox 的方案

- **三段式生命周期**：创建（首次执行代码时创建，写入 lea.session_id / lea.tenant_id 元数据）→ 保活（15 分钟无调用由 `on_timeout=pause` + `auto_resume=true` 自动暂停，下条消息自动恢复、工作目录原样可用）→ 回收（平台循环每 60 秒扫描，只回收带平台会话标记且已 paused 的沙箱）。
- **回收判据收敛到平台侧状态**：用 Cube 返回的状态和 endAt 而非应用本地时间戳，所有副本看到同一份事实，消除误删隐患。
- **数据不随沙箱销毁**：会话 /workspace 通过 NFS host-mount 独立保存，长期交付物留在 OBS。
- **部署排障**：将 `kmem_cache.max_num` 从默认 16000 调至 2000 后模块顺利加载——定位为模块加载阶段系统级内存耗尽（因无 journald 持久化，根因只能确认到"内存耗尽"这一层）。

## 效果与收益

- 预热后沙箱创建 0.3–0.6 秒，首次执行端到端约 2.8 秒（轻量样本，未做压测）。
- 沉淀四条评估期建议：资源下限自行验证、生命周期判据依赖平台侧状态、部署前先完成日志持久化、评估期就完成回收策略设计。

## 参考资料

- 完整案例文章：[在 Cube Sandbox 中构建 Agent 代码执行层：从会话隔离、三段式生命周期到部署资源排障的工程实践](/zh/blog/posts/2026-11-06-ouc-online)
- Cube Sandbox 源码：[TencentCloud/CubeSandbox](https://github.com/TencentCloud/CubeSandbox)
