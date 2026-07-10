---
title: Gemini CLI 集成指南
author: initiallyqq
date: 2026-07-10
tags:
  - integration
  - gemini-cli
  - coding-agent
  - agent
lang: zh-CN
---

# Gemini CLI 集成指南

本指南将 Gemini CLI 运行在 CubeSandbox MicroVM 中，让 Agent 在不获得宿主机权限的前提下检查和修改工作区。配套示例位于 [`examples/gemini-cli-integration`](../../../examples/gemini-cli-integration/)。

## 集成能力

- 基于 `ghcr.io/tencentcloud/cubesandbox-base` 的模板 Dockerfile，内置 Node.js 和 `@google/gemini-cli`。
- 通过 E2B 兼容 Python SDK 运行的一次性编码 Agent。
- 用于验证 CubeSandbox 快照后工作区状态仍可恢复的暂停/恢复示例。
- 使用 CubeEgress 在已允许的 Google API 请求上注入 `x-goog-api-key` 的默认拒绝出口示例。

## 前置条件

- 已部署 CubeSandbox，且存在一个 `READY` 模板。
- 运行示例的可信宿主机上安装 Python 3.10+ 与 `requirements.txt` 中的依赖。
- Gemini API Key（Google AI Studio API-key 模式）。
- 发布自定义模板时，CubeSandbox 节点可访问的镜像仓库。

## 构建模板

```bash
cd examples/gemini-cli-integration
chmod +x build-template.sh
IMAGE=registry.example.com/cube/gemini-cli:2026-07-10 ./build-template.sh
```

脚本会构建并推送镜像，然后使用继承的 `envd` `49983` 端口注册健康检查。生产环境应在升级验证后固定 `GEMINI_CLI_VERSION`，不要长期依赖 `latest`。

## 配置宿主机运行器

```bash
cp .env.example .env
python3 -m pip install -r requirements.txt
```

在 `.env` 中设置 `E2B_API_URL`、`E2B_API_KEY`、`CUBE_TEMPLATE_ID` 和 `GEMINI_API_KEY`。`.env` 必须仅保存在可信运行器宿主机上，且已被 Git 忽略。

## 运行编码任务

```bash
python3 run_gemini.py --approve-all
```

运行器会创建 sandbox、写入一个小型 Python 项目，并调用 `gemini --prompt ...`。`--approve-all` 映射到 Gemini CLI 的 `--yolo`；仅在已明确限定 sandbox 与文件范围时使用，因为它会跳过每次工具调用的确认。

## 跨轮次保留工作状态

```bash
python3 resume_gemini.py --approve-all
```

第一轮会写入 `/workspace/plan.md`，随后 `sandbox.pause()` 创建快照。运行器通过 `Sandbox.connect(...)` 重连、验证 `plan.md`，再运行第二轮。不要对该工作流使用 `with Sandbox.create(...)`：退出上下文会销毁 sandbox，使暂停/恢复失效。

## 安全出口与密钥注入

简单运行器通过命令环境变量传入 `GEMINI_API_KEY`，仅适合开发调试。共享集群请运行：

```bash
python3 network_policy.py --approve-all
```

该脚本以 `allow_internet_access=False` 创建 sandbox，并仅为 `generativelanguage.googleapis.com` 设置一条 CubeEgress 规则。规则会在 Host/SNI 匹配后注入真实 `x-goog-api-key` 请求头。Gemini 只能看到占位 `GEMINI_API_KEY`，真实密钥不会出现在 VM 环境、文件系统或命令行中。

HTTPS 拦截需要模板信任 CubeEgress CA。示例设置 `NODE_EXTRA_CA_CERTS=/etc/ssl/certs/ca-certificates.crt`，使 Node 信任拦截证书链。规则语法和审计行为请参阅[安全代理](../security-proxy.md)。

## 生产建议

- 使用固定 Gemini CLI 和 Node 版本的专用模板。
- 除非已明确约束工作负载与文件范围，否则保持关闭 `--yolo`。
- 每个用户或会话使用独立 sandbox；对闲置会话暂停并设置生命周期超时。
- 生产环境采用默认拒绝出口，只放行精确的 Google API 主机。
- 快照中仅保留不敏感工作区状态。快照会保留可写文件系统和进程状态。
- 审查 CubeEgress 元数据审计日志中的规则名、目标、状态和延迟；密钥会被脱敏。

## 常见问题

| 现象 | 原因与处理 |
| --- | --- |
| `gemini: command not found` | 重新构建模板，并在镜像中检查 `gemini --version`。 |
| 鉴权失败 | 检查 API-key 模式的 Key 是否有效。凭据保险库路径还需确认 CubeEgress 使用了正确主机并注入 `x-goog-api-key`。 |
| TLS 自签名或连接错误 | 模板可能不信任 CubeEgress CA。请使用标准基础镜像/CA 路径重建，并按示例设置 `NODE_EXTRA_CA_CERTS`。 |
| 出口请求被拒绝 | 默认拒绝是预期行为。只添加必要 API 主机的显式规则，不要以开启全部网络作为替代方案。 |
| 第一轮后的文件丢失 | 确认调用了 `pause()` 并连接同一个 sandbox ID，而不是创建新 sandbox 或退出上下文管理器。 |
| Agent 等待确认 | 仅在需要自主写入时添加 `--approve-all`，否则保留审批模式。 |

## 验证

```bash
python3 -m unittest examples/gemini-cli-integration/test_common.py
python3 -m py_compile examples/gemini-cli-integration/*.py
bash -n examples/gemini-cli-integration/build-template.sh
docker build -t gemini-cli-cube:local examples/gemini-cli-integration
```

在线运行还需要已注册的 CubeSandbox 模板和凭据。在启用自主操作前，请先在非生产项目中运行 `run_gemini.py`、`resume_gemini.py` 与 `network_policy.py`。
