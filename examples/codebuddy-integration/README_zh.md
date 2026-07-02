# CodeBuddy 与 Cube Sandbox 集成

[English](README.md)

这个示例把 CodeBuddy CLI 运行在 Cube Sandbox MicroVM 中。本地 Python runner
通过 E2B 兼容 SDK 创建沙箱，把 CodeBuddy 凭据注入到当前沙箱实例，写入一个最小
demo workspace，然后用 `codebuddy -p` 以 headless 模式启动 CodeBuddy。

## 前置条件

- 已部署并可访问的 Cube Sandbox，且本机能访问 CubeAPI。
- 已配置好同一套部署的 `cubemastercli`。
- 可构建并推送镜像，且 Cube 节点能够拉取该镜像。
- 本机安装 Python 3.8+。
- 可用于 CodeBuddy CLI 的 CodeBuddy 账号或 API Key。
- 沙箱需要能访问 CodeBuddy 及其背后 LLM API 的网络出口。

## 1. 构建 CodeBuddy 模板

把 `IMAGE_NAME` 设置为 Cube 集群可访问的镜像仓库地址：

```bash
IMAGE_NAME=registry.example.com/cube/codebuddy:latest \
  DOCKER_PLATFORM=linux/amd64 \
  PUSH_IMAGE=1 \
  CREATE_TEMPLATE=1 \
  WATCH_JOB=1 \
  bash build-template.sh
```

模板镜像基于 `ghcr.io/tencentcloud/cubesandbox-base:2026.16`，安装 Node.js
22，并固定安装 `@tencent-ai/codebuddy-code@2.114.2`。
如果你在 Apple Silicon macOS 上构建、目标 Cube 节点是 x86_64 Linux，建议保留
`DOCKER_PLATFORM=linux/amd64`。

记录生成的 `template_id`，后续填入 `CUBE_TEMPLATE_ID`。

## 2. 配置本地环境

```bash
python3 -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt

cp .env.example .env
```

编辑 `.env`：

```bash
export E2B_API_URL="http://<your-node-ip>:3000"
export E2B_API_KEY="e2b_000000"
export CUBE_TEMPLATE_ID="<template-id>"
export CODEBUDDY_API_KEY="<your-codebuddy-api-key>"
```

不要把 `CODEBUDDY_API_KEY` 写入 Docker 镜像。runner 会通过
`Sandbox.create(envs={...})` 注入该密钥，只作用于当前沙箱实例。

## 3. 在 Cube Sandbox 中运行 CodeBuddy

```bash
python run_codebuddy.py
```

runner 会在沙箱中创建 `/tmp/codebuddy-demo`，并要求 CodeBuddy 检查该目录、执行
`python3 hello.py`、总结运行结果。

可以覆盖默认 prompt：

```bash
python run_codebuddy.py \
  --prompt "Inspect /tmp/codebuddy-demo and run python3 hello.py"
```

## 4. 验证暂停与恢复

```bash
python run_codebuddy.py --pause-resume
```

CodeBuddy 运行完成后，脚本会在 `CODEBUDDY_CONFIG_DIR` 下写入 marker，调用
`sandbox.pause()`，再通过 `sandbox.connect()` 恢复，并验证 marker 仍然存在。
这展示了编码 Agent 长任务或跨会话开发时的状态保留方式。

## 运行建议

- 可复现模板中建议保留 `DISABLE_AUTOUPDATER=1`。升级 CodeBuddy 时，通过显式版本重新构建镜像。
- 不要把凭据写入镜像或 Git。生产环境建议结合 CubeSandbox 的凭据管理、security proxy，或由出口网关托管服务凭据。
- 沙箱必须能访问 CodeBuddy 和背后的 LLM API。如果部署启用了网络白名单，需要放行相应域名，或通过 CubeEgress 转发。
- `CODEBUDDY_ALLOWED_TOOLS` 与 `CODEBUDDY_PERMISSION_MODE` 是 headless 示例默认值。示例使用
  `bypassPermissions`，这样 CodeBuddy 在沙箱内执行 `python3 hello.py` 时不需要交互式审批；生产环境应按团队审批策略调整。

## 常见问题

| 现象 | 可能原因 | 处理方式 |
| --- | --- | --- |
| `Missing required environment variables` | 没有创建 `.env` 或必填值为空 | 复制 `.env.example` 为 `.env` 并填写必填项 |
| `Template not found` | `CUBE_TEMPLATE_ID` 指向错误模板 | 重新运行 `cubemastercli tpl list` 并更新 `.env` |
| `codebuddy: command not found` | 沙箱没有使用 CodeBuddy 模板启动 | 使用本示例镜像重新创建模板 |
| CodeBuddy 鉴权失败 | `CODEBUDDY_API_KEY` 无效，或账号要求交互式登录 | 先在本地验证 key；如果使用登录态，改为注入预登录的 `CODEBUDDY_CONFIG_DIR` |
| 请求超时 | 沙箱出网被拦截 | 检查 Cube 网络策略、DNS、代理和 CubeEgress 规则 |
| 模板创建超时 | `envd` 没有健康启动 | 确认镜像基于 `cubesandbox-base`，模板探针为 `49983 /health` |

## 目录结构

```text
codebuddy-integration/
├── .env.example
├── README.md
├── README_zh.md
├── build-template.sh
├── env_utils.py
├── requirements.txt
├── run_codebuddy.py
├── template/
│   └── Dockerfile
└── test_run_codebuddy.py
```
