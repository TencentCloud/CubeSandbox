---
title: CodeBuddy 集成指南
author: toxitoxi
date: 2026-07-02
tags:
  - integration
  - codebuddy
lang: zh-CN
---

# CodeBuddy 集成指南

## Integration Target and Version

本文展示如何把 CodeBuddy CLI 运行在 Cube Sandbox 中，作为隔离的终端型编码
Agent 使用。本示例基于以下版本验证：

- 基于 `ghcr.io/tencentcloud/cubesandbox-base:2026.16` 构建的 Cube Sandbox 模板
- `@tencent-ai/codebuddy-code@2.114.2`
- `e2b-code-interpreter>=2.4.1`

该集成使用 Cube Sandbox 的 E2B 兼容 API。本地 runner 会从 CodeBuddy 模板创建
沙箱，通过实例级环境变量注入凭据，并用 `codebuddy -p` 以 headless 模式启动
CodeBuddy。

## Prerequisites

- 已部署 Cube Sandbox，且本机可通过 `E2B_API_URL` 访问 CubeAPI。
- 已配置同一集群的 `cubemastercli`。
- 可使用 Docker 构建镜像，并推送到 Cube 节点可拉取的镜像仓库。
- 可用于 CodeBuddy CLI 的 CodeBuddy 账号或 API Key。
- 沙箱具备访问 CodeBuddy 和背后 LLM API 的网络出口。
- 本机 Python 3.8+，并安装 `e2b-code-interpreter` 与 `python-dotenv`。

示例所需环境变量：

```bash
export E2B_API_URL="http://<your-node-ip>:3000"
export E2B_API_KEY="e2b_000000"
export CUBE_TEMPLATE_ID="<template-id>"
export CODEBUDDY_API_KEY="<your-codebuddy-api-key>"
```

## Integration Steps

1. 使用 `examples/codebuddy-integration/template/Dockerfile` 构建模板镜像。
2. 将镜像推送到 Cube 节点可访问的 registry。
3. 使用 `cubemastercli tpl create-from-image` 创建 Cube 模板，并暴露、探测
   envd 的 `49983 /health`。
4. 复制 `examples/codebuddy-integration/.env.example` 为
   `examples/codebuddy-integration/.env`，填写 Cube 与 CodeBuddy 配置。
5. 在仓库根目录运行 `python examples/codebuddy-integration/run_codebuddy.py`
   创建沙箱并启动 CodeBuddy。
6. 运行 `python examples/codebuddy-integration/run_codebuddy.py --pause-resume`，
   验证状态可跨 `sandbox.pause()` 与 `sandbox.connect()` 保留。

示例构建命令：

```bash
IMAGE_NAME=registry.example.com/cube/codebuddy:latest \
  DOCKER_PLATFORM=linux/amd64 \
  PUSH_IMAGE=1 \
  CREATE_TEMPLATE=1 \
  WATCH_JOB=1 \
  bash examples/codebuddy-integration/build-template.sh
```

runner 会通过 `Sandbox.create(envs={...})` 注入 `CODEBUDDY_API_KEY`。不要把
API Key 写入 Dockerfile、模板环境变量或 Git。
如果你在 Apple Silicon macOS 上构建、目标 Cube 节点是 x86_64 Linux，建议保留
`DOCKER_PLATFORM=linux/amd64`。

## Key Code Snippets

模板镜像：

```dockerfile
FROM ghcr.io/tencentcloud/cubesandbox-base:2026.16

ENV DISABLE_AUTOUPDATER=1 \
    CODEBUDDY_CONFIG_DIR=/workspace/.codebuddy

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl git gnupg python3 python3-pip ripgrep \
    && mkdir -p /etc/apt/keyrings \
    && curl -fsSL https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key \
        | gpg --dearmor -o /etc/apt/keyrings/nodesource.gpg \
    && chmod 0644 /etc/apt/keyrings/nodesource.gpg \
    && echo "deb [signed-by=/etc/apt/keyrings/nodesource.gpg] https://deb.nodesource.com/node_22.x nodistro main" \
        > /etc/apt/sources.list.d/nodesource.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends nodejs \
    && npm install -g @tencent-ai/codebuddy-code@2.114.2 \
    && codebuddy --version \
    && npm cache clean --force \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/cube-entrypoint.sh"]
CMD ["sleep", "infinity"]
```

凭据注入：

```python
with Sandbox.create(
    template=os.environ["CUBE_TEMPLATE_ID"],
    timeout=600,
    envs={
        "CODEBUDDY_API_KEY": os.environ["CODEBUDDY_API_KEY"],
        "CODEBUDDY_CONFIG_DIR": "/workspace/.codebuddy",
        "DISABLE_AUTOUPDATER": "1",
    },
) as sandbox:
    result = sandbox.commands.run("codebuddy -p 'Inspect this workspace' --output-format text")
    print(result.stdout)
```

暂停与恢复：

```python
sandbox.commands.run("echo ready > /workspace/.codebuddy/state.txt")
sandbox.pause()
sandbox.connect()
result = sandbox.commands.run("cat /workspace/.codebuddy/state.txt")
print(result.stdout)
```

## Caveats

- CodeBuddy 需要访问自身服务和背后的 LLM API。若 Cube 网络策略已启用，需要放行
  必要目标，或通过 CubeEgress 转发。
- 示例中的 `CODEBUDDY_ALLOWED_TOOLS` 与 `CODEBUDDY_PERMISSION_MODE` 是 headless
  运行默认值。示例使用 `bypassPermissions`，这样 CodeBuddy 在沙箱内执行
  `python3 hello.py` 时不需要交互式审批；生产环境应按团队审批策略调整。
- 模板中建议保留 `DISABLE_AUTOUPDATER=1`，升级时通过固定 CodeBuddy 包版本重新构建镜像。
- 如果 CodeBuddy 账号使用交互式登录而不是 API-key 鉴权，应在创建沙箱时注入预登录的
  `CODEBUDDY_CONFIG_DIR`，不要把登录态写进镜像。
- 如果模板创建超时，优先确认镜像会启动 envd，且模板探针配置为 `49983 /health`。

## References

- 可运行示例：`examples/codebuddy-integration`
- CodeBuddy CLI 安装文档：https://www.codebuddy.ai/docs/cli/installation
- CodeBuddy headless 模式：https://www.codebuddy.ai/docs/cli/headless
- Cube Sandbox 自定义镜像指南：../tutorials/bring-your-own-image.md
- Cube Sandbox 网络策略指南：../network-policy.md
