---
title: CodeBuddy CI 集成指南
author: dujunjin
date: 2026-07-23
tags:
  - integration
  - codebuddy
  - ci
  - coding-agent
lang: zh-CN
---

# CodeBuddy CI 集成指南

当 CI 需要让 Agent 检查、测试或生成不受信任代码的报告时，可在 CubeSandbox MicroVM 中运行 [CodeBuddy Code CLI](https://www.npmjs.com/package/@tencent-ai/codebuddy-code)。源码和 Agent 工具留在 MicroVM 内，CI Runner 只保留最少的 CubeAPI 与 CodeBuddy 凭据，避免把 Agent 放在高权限 Runner 上执行。

配套可运行示例位于仓库中的 `examples/codebuddy-ci-integration/`。

## 支持的接口

示例固定 CodeBuddy Code `2.125.5`，使用无交互接口：`--print --output-format json --session-id <id>`。后续轮次保持同一 ID 并使用 `--resume <id>`。升级 CLI 后请先执行 `codebuddy --help`，不要假定交互式参数能在 E2B 命令通道中工作。

## 接入步骤

1. 基于提供的 Dockerfile 构建镜像（继承 `cubesandbox-base`），并使用 49983 端口健康检查注册模板。
2. 将 `E2B_API_URL`、`E2B_API_KEY`、`CODEBUDDY_AUTH_TOKEN` 保存为 CI Secret，将就绪的 `CUBE_TEMPLATE_ID` 保存为非敏感 CI Variable。
3. 仅允许 CI Runner 访问 CubeAPI；模板应配置 CubeEgress 默认拒绝，只放行租户所需的 CodeBuddy API 主机和经明确评估的端点。
4. 上传源码 `.tar`，不要挂载 CI 工作区；排除 `.git`、凭据和任务不需要的文件。

驱动只向 Agent 命令转发 `CODEBUDDY_AUTH_TOKEN`，不会转发 Runner 的 GitHub Token、镜像仓库密码或其他宿主环境变量。

## 最小执行流程

```bash
tar --exclude=.git -cf /tmp/project.tar .
cd examples/codebuddy-ci-integration
python -m pip install -r requirements.txt
python run_codebuddy_ci.py --source-tar /tmp/project.tar
```

默认提示词会运行最小相关测试并写入 `/workspace/codebuddy-ci-report.md`。生产流水线应使用更小、更便于审计的 `--prompt`，并保留“不 commit、不 push”的约束。CLI 使用 `bypassPermissions` 仅限一次性、出口受限的 MicroVM；不要与宽松外网或生产机密并用。

## 使用快照继续长任务

```bash
python run_codebuddy_ci.py --source-tar /tmp/project.tar --pause
python resume_codebuddy_ci.py <输出的-sandbox-id>
```

`pause()` 会保存可写层，包括 `/workspace` 与 CodeBuddy 会话目录。恢复驱动用 `Sandbox.connect` 连接同一实例，再以相同会话 ID 调用 CLI。快照属于敏感产物：应限制访问、设置过期策略，并在取得最终报告后销毁 Sandbox。

## GitHub Actions 模式

将配套的 `github-actions.yml` 复制到实际 CI 仓库。它使用 `pull_request`、`permissions: contents: read`、tar 上传，并且不使用 `pull_request_target`，从而避免 Fork 代码取得写 Token。不要让 Agent 直接发布评论或 commit；这些副作用应放到另一个经过审查的工作流步骤。

## 常见问题

| 现象 | 处理方式 |
| --- | --- |
| 鉴权失败 | 确认 `CODEBUDDY_AUTH_TOKEN` 是 CI Secret，并在命令执行时注入，而不是写进 Dockerfile。 |
| 403 或模型超时 | 只增加必需的 CubeEgress allow 规则，保持默认拒绝。 |
| 模板未就绪 | 保留基础镜像中 envd 的 49983 健康检查端点。 |
| 恢复后找不到会话 | 使用相同的 `CODEBUDDY_SESSION_ID`，且不要提前 kill 已暂停的 Sandbox。 |

## 参考

- 本仓库的 `examples/codebuddy-ci-integration/`
- [Issue #644](https://github.com/TencentCloud/CubeSandbox/issues/644)
- [CodeBuddy Code CLI npm 包](https://www.npmjs.com/package/@tencent-ai/codebuddy-code)
