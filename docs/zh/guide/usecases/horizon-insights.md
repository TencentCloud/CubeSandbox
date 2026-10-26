---
title: "弘则信息：金融投研 Agent 沙箱化实践"
author: 王正凯
date: 2026-08-26
tags:
  - agent
  - financial
  - host-mount
  - cubeegress
lang: zh-CN
---

# 弘则信息：金融投研 Agent 沙箱化实践

## 业务背景

弘则信息的 4as（All (Domain) Agents as a Service）是一套金融投研 Agent 产品，将领域知识、研究方法、数据工具和持续状态组织为可部署的研究 Agent。单次任务持续几十秒到数分钟，需要完整的 Agent 循环在隔离环境中运行。

## 核心痛点

- **完整 Agent 循环隔离**：投研 Agent 的 run 涉及模型交互、代码执行、文件操作、外部工具调用，需要完整的进程和文件边界。
- **领域资源版本化分发**：大型领域资源（DAS）需按版本、按授权分发到每个沙箱，不能每次 run 重复搬运。
- **凭据隔离**：模型和工具的长期 API key 不能进入 guest 环境，需在出口侧注入。
- **本地化部署**：金融机构要求运行在指定云环境或客户侧基础设施中，托管沙箱无法覆盖。

## 基于 CubeSandbox 的方案

三个核心使用场景：

| 场景 | CubeSandbox 能力 | 说明 |
|---|---|---|
| 完整 Pi loop | Sandbox guest 内运行 | 模型-工具主循环、bash/文件操作、MCP 调用均在隔离环境内 |
| DAS 资源分发 | Host Mount（只读） | 按 revision 分发不可变文件树，多沙箱共享同一份宿主缓存 |
| 凭据管理 | CubeEgress（出口注入） | 长期 key 保留在出口侧，请求离开沙箱时自动注入认证信息 |

## 效果与收益

- 同一套 Agent runtime 支撑客户侧部署和 Cloud 版服务。
- 领域资源按需可见，未挂载资源对 guest 不可见。
- 凭据由平台集中轮换和撤销，Sandbox template 无需随 key 变化重建。
- 已在多个商业合作项目中完成落地部署。

## 参考资料

- 完整案例文章：[金融投研 Agent 沙箱化：弘则信息基于 CubeSandbox 的选型与架构实践](/zh/blog/posts/2026-08-28-horizon-insights)
- Cube Sandbox 源码：[TencentCloud/CubeSandbox](https://github.com/TencentCloud/CubeSandbox)
