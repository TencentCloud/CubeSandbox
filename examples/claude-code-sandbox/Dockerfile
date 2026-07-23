# CubeSandbox 模板：Claude Code 沙箱
#
# 基于 CubeSandbox 官方基础镜像，安装 Node.js 22 和 Claude Code CLI。
# 继承的 cube-entrypoint 在 :49983 端口保持 envd 运行，
# 供 Cube 使用标准就绪探针。
#
# 构建：
#   docker build -t claude-code-cube:latest examples/claude-code-sandbox
#
# 注册为 Cube 模板：
#   cubemastercli tpl create-from-image \
#     --image <your-registry>/claude-code-cube:latest \
#     --writable-layer-size 4G \
#     --expose-port 49983 \
#     --probe       49983 \
#     --probe-path  /health

ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${CUBE_BASE_IMAGE}

ARG DEBIAN_FRONTEND=noninteractive
ARG NODE_MAJOR=22
ARG CLAUDE_CODE_VERSION=1.0.0

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        bash \
        ca-certificates \
        curl \
        git \
        gnupg \
        jq \
        less \
        procps \
        python3 \
        python3-pip \
        ripgrep \
    && curl -fsSL "https://deb.nodesource.com/setup_${NODE_MAJOR}.x" | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*

# 将 Claude Code 安装在独立层中，这样更新 CLAUDE_CODE_VERSION 时
# 不会使上面耗时的 OS + Node.js 层缓存失效。
RUN npm install -g --ignore-scripts "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" \
    && claude --version \
    && npm cache clean --force \
    && rm -rf /root/.npm

ENV CLAUDE_CODE_DISABLE_TELEMETRY=1 \
    CLAUDE_CODE_SKIP_UPDATE_CHECK=1

RUN mkdir -p /workspace /root/.claude

WORKDIR /workspace

EXPOSE 49983
