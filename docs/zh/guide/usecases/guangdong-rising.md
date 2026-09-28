---
title: "广晟数科：多租户沙箱平台构建实践"
author: 冯佳奇
date: 2026-09-03
tags:
  - agent
  - multi-tenant
  - sandbox-platform
  - lifecycle
lang: zh-CN
---

# 广晟数科：多租户沙箱平台构建实践

## 业务背景

广晟数科有两条产品线（AI 应用生成平台 SIN Builder、企业级 AI 助手大晟AI）运行 AI Agent 生成的代码。团队把两条产品线各自的沙箱能力收敛成独立服务 common-sandbox-runner，作为广晟数科 SIN PaaS（AI 中台）的沙箱执行层，基于 Cube Sandbox 构建多租户沙箱治理能力。

## 核心痛点

- **两套实现互不相通**：两条产品线各写一套沙箱能力，生命周期、租约、回收策略互不一致，一次泄漏故障导致两条产品线同时不可用。
- **长会话的状态复用**：Agent 会话是长连接、有状态的，同一会话必须复用同一沙箱；迟到的清理请求可能误删新一代租约正在使用的沙箱。
- **配额与回收的两难**：多副本部署下，既不能误回收正在运行的沙箱，也不能让崩溃副本遗留的沙箱持续泄漏。
- **冷启动延迟**：沙箱冷启动直接变成用户等待时间，自建 warm pool 的维护和一致性成本高。

## 基于 CubeSandbox 的方案

- **统一沙箱执行层**：独立 FastAPI 服务 common-sandbox-runner 是系统中唯一能写 Cube 的一侧，产品线只通过 `POST /v1/sandbox-runs`（SSE 流式事件）调用；租约、准入计数、GC 候选等跨请求状态全部存 Redis。
- **统一归属记账**：按 namespace + env + 项目 ID + 会话 run ID 推导 `owner_id` 写入沙箱 metadata，所有沙箱可追溯到具体产品线、环境和会话；双模板设计按任务资源量级选择镜像。
- **租约 + fencing token**：`POST /v1/leases` 按 `owner_id` 复用沙箱，`POST /v1/lease-releases` 必须同时匹配 `owner_id` 和 `lease_id`（epoch）才真正 kill，迟到的清理请求只会封存过期代次，不会误删当前租约。
- **准入控制**：Redis Lua 脚本原子获取准入槽（桶级限额 + 全局上限），最长等待 30 秒，超时返回可重试的 CAPACITY_EXCEEDED。
- **三色标记回收**：参照 JVM/Go 的三色标记清扫加代际宽限——黑色（心跳新鲜的根，永不回收）、灰色（宽限期内，禁止删除）、白色（连续两个确认周期无根引用且 generation 不变，才清扫）；配合 Cube 官方的 `on_timeout=pause` + `auto_resume` 生命周期，空闲租约软预留而非直接 kill。

## 效果与收益

- **启动延迟**：烤模板 hot-start 实测中位数 180 ms、P95 210 ms，冷启动稳定一秒以内，自建 warm pool 整体退役。
- **架构瘦身**：净删除约 1350 行代码（+193 / -1542），两条产品线的沙箱逻辑收敛成 1 个共用服务，198 个测试用例守住边界。
- **容量上限**：admission 逻辑上限从 200 放宽至 500 沙箱，解除僵尸租约导致的配额死锁，未增加任何机器资源。

## 参考资料

- 完整案例文章：[谁有权删掉一个沙箱：在 CubeSandbox 上构建多租户沙箱平台](/zh/blog/posts/2026-09-03-guangdong-rising)
- Cube Sandbox 源码：[TencentCloud/CubeSandbox](https://github.com/TencentCloud/CubeSandbox)
