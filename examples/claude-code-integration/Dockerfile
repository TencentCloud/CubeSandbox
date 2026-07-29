# syntax=docker/dockerfile:1.7
#
# Claude Code CLI inside a CubeSandbox template.
#
# The image installs Node.js 24 and the @anthropic-ai/claude-code CLI on top of
# the official CubeSandbox base image. The inherited cube-entrypoint keeps envd
# running on :49983 so Cube can use the standard readiness probe.
#
# Build:
#   docker build -t claude-code-cube:latest examples/claude-code-integration
#
# Register as a Cube template:
#   cubemastercli tpl create-from-image \
#     --image <your-registry>/claude-code-cube:latest \
#     --writable-layer-size 4G \
#     --expose-port 49983 \
#     --probe       49983 \
#     --probe-path  /health

ARG CUBE_BASE_IMAGE=ghcr.nju.edu.cn/tencentcloud/cubesandbox-base:2026.16
FROM ${CUBE_BASE_IMAGE}

ARG DEBIAN_FRONTEND=noninteractive
ARG NODE_MAJOR=24
ARG CLAUDE_CODE_VERSION=latest

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

# Install Claude Code CLI in its own layer so bumping the version does not
# invalidate the slow OS + Node.js layer above.
RUN npm install -g "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" \
    && claude --version \
    && npm cache clean --force \
    && rm -rf /root/.npm

# Claude Code state directory (pre-created empty so no session leaks across
# tenants; sessions are ephemeral and scoped to the sandbox lifecycle).
ENV CLAUDE_CODE_STATE_DIR=/root/.claude

RUN mkdir -p /workspace "${CLAUDE_CODE_STATE_DIR}"

WORKDIR /workspace

EXPOSE 49983
