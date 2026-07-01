---
title: OpenCode 集成指南
author: xie-guangzhen
date: 2026-07-01
tags:
  - integration
  - opencode
  - coding-agent
lang: zh-CN
---

# OpenCode 集成指南

本文说明如何在 Cube Sandbox 微虚机中运行 [OpenCode](https://opencode.ai/docs/)。内容覆盖可复现模板、敏感配置注入、网络出口策略，以及通过快照实现会话保持和跨会话恢复。

## 集成对象与版本

- 集成对象：OpenCode 终端型 AI 编码 Agent
- 示例方式：使用 `opencode run` 非交互 CLI 模式
- 安装方式：`npm install -g opencode-ai`
- Cube API：兼容 E2B 的 Python SDK（`e2b-code-interpreter`）

示例工程位于 `examples/opencode-integration/`。

## 前置条件

- 已部署 Cube Sandbox，且本机可访问 CubeAPI，例如 `http://<node-ip>:3000`
- 可使用 `cubemastercli` 创建模板
- 客户端安装 Python 3.8+
- 一个 OpenCode 支持的 LLM Provider Key，例如 `OPENAI_API_KEY`、`ANTHROPIC_API_KEY`，或其他 OpenCode 已配置的 Provider

## 构建 OpenCode 模板

进入示例目录并构建模板：

```bash
cd examples/opencode-integration
./build-template.sh
```

脚本会输出 `template_id`，将其写入 `CUBE_TEMPLATE_ID`。

该模板会安装 Node.js、Git、常见构建工具和 `opencode-ai`，并创建默认工作目录 `/workspace`。

## 配置敏感信息

复制环境变量模板：

```bash
cp examples/opencode-integration/.env.example examples/opencode-integration/.env
```

填写以下变量：

```bash
export E2B_API_URL="http://<cube-api-host>:3000"
export E2B_API_KEY="e2b_000000"
export CUBE_TEMPLATE_ID="<template-id>"
export OPENAI_API_KEY="<provider-key>"
export OPENCODE_MODEL="openai/gpt-4.1-mini"
```

不要把 Provider API Key 写入镜像。推荐在创建沙箱时通过环境变量注入，或在沙箱启动后从加密密钥系统生成 OpenCode Provider 配置。

## 网络与出口策略

OpenCode 需要访问所选 LLM Provider。多数编码任务还需要访问 GitHub、npm、PyPI 等代码托管或包管理服务。建议先使用最小出口白名单，只放行模型服务和必要的软件源。

如果只需要执行已经生成好的代码，可以将规划阶段和执行阶段拆开：规划阶段允许访问 LLM，执行阶段通过 Cube 网络策略关闭公网出口，只运行确定性的测试命令。

## 运行示例

```bash
cd examples/opencode-integration
pip install -r requirements.txt
python run_opencode.py --prompt "Inspect /workspace and create a short project summary."
```

脚本会创建沙箱、上传一个最小项目、在沙箱内运行 OpenCode，并输出执行结果。

## 会话保持与恢复

短任务可在命令结束后销毁沙箱。长任务可以在 OpenCode 初始化项目后创建快照：

```bash
python run_opencode.py --prompt "Run /init and summarize this project." --snapshot
```

脚本会输出 snapshot ID。后续将该 snapshot ID 作为 `template` 使用，即可从同一文件系统状态继续：

```bash
python run_opencode.py --template <snapshot-id> --prompt "Continue from the previous state and run tests."
```

该方式可保留 `/workspace`、生成的 `AGENTS.md`、依赖缓存以及沙箱文件系统中的 OpenCode 会话数据。

## 典型使用模式

- 将 OpenCode 本身运行在 Cube Sandbox 内，实现隔离的仓库编辑环境。
- 在外部调度器中按任务创建一个沙箱，注入密钥，运行 `opencode run`，收集 diff，最后销毁沙箱。
- 拆分规划和执行阶段：规划阶段允许访问模型服务，快照后以更严格网络策略恢复并运行测试。
- 对需要安装大量依赖或拉取大仓库的长任务，先准备好环境并创建快照，再进行后续编码迭代。

## 常见问题

| 现象 | 可能原因 | 处理方式 |
| --- | --- | --- |
| `Template not found` | `CUBE_TEMPLATE_ID` 错误 | 运行 `cubemastercli tpl list` 并更新 `.env` |
| OpenCode 无法调用模型 | Provider Key 缺失或出口被拦截 | 检查 `OPENAI_API_KEY` 等环境变量，并放行模型服务出口 |
| `opencode: command not found` | 未使用 OpenCode Dockerfile 构建模板 | 重新执行 `./build-template.sh` 并使用新的 template ID |
| 安装依赖卡住 | npm/GitHub 等出口被拦截 | 放行对应软件源，或把依赖预置进模板 |
| 快照恢复后状态丢失 | 没有把 snapshot ID 作为下一次模板使用 | 传入 `--template <snapshot-id>`，或将 `CUBE_TEMPLATE_ID` 设置为 snapshot ID |

## 参考

- 示例工程：`examples/opencode-integration/`
- OpenCode 文档：<https://opencode.ai/docs/>
- Cube Sandbox 快速开始：`examples/code-sandbox-quickstart/`
- 快照示例：`examples/snapshot-rollback-clone/`
