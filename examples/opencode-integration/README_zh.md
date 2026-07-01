# OpenCode + CubeSandbox 示例

[English README](README.md)

本示例演示如何在 CubeSandbox 微虚机中运行 OpenCode 终端型 AI 编码 Agent，覆盖模板构建、Provider Key 注入、非交互 `opencode run`，以及可选的快照会话保持。

## 文件说明

| 文件 | 说明 |
| --- | --- |
| `template/Dockerfile` | 包含 Node.js、Git、Python、ripgrep 和 `opencode-ai` 的沙箱镜像 |
| `build-template.sh` | 构建镜像，并输出 `cubemastercli tpl create-from-image` 命令 |
| `run_opencode.py` | 创建沙箱、写入 `/workspace` 示例项目、运行 OpenCode，并可选创建快照 |
| `.env.example` | 本地环境变量模板 |

## 1. 构建模板

```bash
./build-template.sh
```

如果 Cube 节点无法拉取本地 Docker 镜像，请先推送镜像：

```bash
export IMAGE_REGISTRY=<your-registry>/<namespace>
export PUSH_IMAGE=1
./build-template.sh
```

然后执行脚本输出的 `cubemastercli tpl create-from-image` 命令，并记录返回的 template ID。

## 2. 配置环境变量

```bash
cp .env.example .env
```

编辑 `.env`：

```bash
export E2B_API_URL="http://<cube-api-host>:3000"
export E2B_API_KEY="e2b_000000"
export CUBE_TEMPLATE_ID="<template-id>"
export OPENAI_API_KEY="<provider-key>"
export OPENCODE_MODEL="openai/gpt-4.1-mini"
```

OpenCode 支持多个 Provider。请按你的 OpenCode 配置填写对应 Provider Key 和模型名。

## 3. 运行示例

```bash
pip install -r requirements.txt
python run_opencode.py --prompt "Inspect /workspace and create a short project summary."
```

预期流程：

```text
sandbox sbx-...
$ opencode --version
...
$ opencode run --auto 'Inspect /workspace and create a short project summary.'
...
```

## 4. 快照与恢复

在 OpenCode 初始化项目状态后创建快照：

```bash
python run_opencode.py --prompt "Run /init and summarize this project." --snapshot
```

从快照恢复：

```bash
python run_opencode.py --template <snapshot-id> --prompt "Continue from the previous state and inspect AGENTS.md."
```

快照会保留 `/workspace`、生成文件、依赖缓存，以及沙箱文件系统中的 OpenCode 本地状态。

## 5. 网络策略

普通 OpenCode 任务需要访问所选 LLM Provider 和必要的软件源。对于不需要模型调用的确定性执行，可使用断网模式：

```bash
python run_opencode.py --network no-internet --prompt "Run python app.py and report the output."
```

仅在 prompt 不需要访问外部模型，或模型服务可通过内部白名单/代理访问时使用断网模式。

## 常见问题

| 现象 | 处理方式 |
| --- | --- |
| `Missing required environment variable` | source `.env`，或从本目录运行脚本 |
| `Template not found` | 使用 `cubemastercli tpl list` 检查 `CUBE_TEMPLATE_ID` |
| `opencode: command not found` | 重新构建 OpenCode 镜像并创建新的 Cube 模板 |
| Provider 鉴权失败 | 检查 Provider Key 环境变量和 OpenCode 模型名 |
| 命令超时 | 增加 sandbox timeout，或把依赖预置进镜像 |

## 相关文档

- `docs/zh/guide/integrations/opencode.md`
- `examples/code-sandbox-quickstart/`
- `examples/snapshot-rollback-clone/`
