---
title: Python 生产事故 RCA 代码解释器沙箱
author: Sam
date: 2026-07-04
tags:
  - python
  - data-science
  - code-interpreter
  - incident-rca
  - e2b
lang: zh-CN
---

# Python 生产事故 RCA 代码解释器沙箱

## 业务背景

AI Agent 越来越多地参与 SRE 和平台团队的事故排查。线上异常发生时，工程师通常会导出服务指标、部署事件、告警和 runbook 阈值，然后让 Agent 辅助定位异常窗口、关联变更并生成复盘报告。

`examples/python-data-science` 模板在 Cube Sandbox 中演示这一工作流。Agent 风格的客户端会上传多个输入文件，在隔离沙箱中运行 Python，通过 CubeSandbox snapshot 为中间分析状态创建 checkpoint，从该 checkpoint fork 出新沙箱继续追问分析，最后下载打包后的 RCA 结果。

## 核心痛点

- 事故分析需要真实的数据处理，而不是只靠自然语言猜测。
- 指标、告警、部署事件和 runbook 策略通常来自多个文件。
- Code Interpreter 会话需要在多轮分析之间保留工作区状态。
- 事故分析常常需要在追问或共享可复现状态前创建 checkpoint。
- 中文报告和图表要求沙箱镜像内置中文字体。
- 追问式分析经常需要镜像构建时没有预装的第三方包。
- RCA 产物需要足够结构化，才能附到工单、复盘文档或后续自动化流程中。

## 基于 Cube Sandbox 的方案

模板基于 digest 固定的 `ghcr.io/tencentcloud/cubesandbox-base:latest` 构建，安装锁定版本的 Python 数据科学环境和 `fonts-wqy-zenhei` 中文字体。示例通过 E2B 兼容 SDK 启动沙箱，并上传：

- `incident_metrics.csv`：服务时间序列指标；
- `deployments.json`：部署事件；
- `alerts.csv`：告警时间线；
- `runbook.json`：SLO 阈值和处理策略提示。

工作流使用可复用的代码解释器模式：

1. **异常检测轮次**：计算错误率、检测 SLO 违约点，并把中间状态写入 `/tmp/cubesandbox-incident-rca/state`。
2. **Snapshot checkpoint**：在输入文件和第一轮状态就绪后创建 CubeSandbox snapshot。
3. **Fork 后追问 RCA 轮次**：从 checkpoint 启动新沙箱，验证状态文件已继承，随后关联部署事件，渲染中文图表，写出 Markdown RCA 报告，生成结构化 JSON/CSV 产物，并把所有结果打包为 tarball。

客户端随后下载压缩包、在本地解压并校验 manifest。这个流程贴近 AI Code Interpreter 对多文件输入、有状态分析、可 checkpoint 工作区和可下载产物的真实处理方式，同时把代码执行隔离在 Cube Sandbox 内。

这不是一个泛化 SDK 集成示例：可复用部分是 Python 代码解释器运行时和产物模式，生产事故 RCA 是验证该模板能力的代表性场景。

## 效果与收益

- 提供一个可复用的 AI 事故分析沙箱起点。
- 演示有状态多轮 Code Interpreter 工作流，无需在宿主机直接修改分析文件。
- 展示 CubeSandbox snapshot/fork 作为可复现追问分析的 checkpoint 机制。
- 展示沙箱运行时通过 `pip3 install emoji humanize` 动态扩展依赖。
- 生成可审计产物：中文图表、RCA 报告、SLO 汇总、部署关联 JSON、manifest 和中间状态。
- 演示面向中文工程报告的 Matplotlib 字体渲染。
- 已登记到教程示例索引，便于新用户和其他模板一起检索。

在受限出口或离线部署中，可以把动态依赖预装进镜像，或通过内网 PyPI 镜像提供。依赖进入模板镜像后，核心流程不需要不受限制的公网访问。

## 参考资料

- 相关示例：`examples/python-data-science`
- 相关 issue：[CubeSandbox 沙箱模板与示例生态](https://github.com/TencentCloud/CubeSandbox/issues/645)
