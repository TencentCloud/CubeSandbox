# syntax=docker/dockerfile:1.7
#
# Claude Code inside a CubeSandbox template.
#
# The image installs Node.js 24 and the @anthropic-ai/claude-code npm package on
# top of the official CubeSandbox base image. The inherited cube-entrypoint keeps
# envd running on :49983 so Cube can use the standard readiness probe.
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

ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${CUBE_BASE_IMAGE}

ARG DEBIAN_FRONTEND=noninteractive
ARG NODE_MAJOR=24
ARG CLAUDE_CODE_VERSION=1.0.33

ENV CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 \
    NPM_CONFIG_UPDATE_NOTIFIER=false

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

# Install Claude Code in its own layer so bumping CLAUDE_CODE_VERSION does not
# invalidate the slow OS + Node.js layer above (only this short layer rebuilds
# on a version bump).
RUN npm install -g --no-audit --no-fund "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" \
    && claude --version \
    && rm -rf /root/.npm

RUN mkdir -p /workspace

WORKDIR /workspace

EXPOSE 49983
