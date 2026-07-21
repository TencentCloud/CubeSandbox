# syntax=docker/dockerfile:1.7
#
# Claude Code coding agent inside a CubeSandbox template.
#
# The image installs Node.js 24 and the Claude Code CLI on top of the
# official CubeSandbox base image. The inherited cube-entrypoint keeps envd
# running on :49983 so Cube can use the standard readiness probe.
#
# Build:
#   docker build --platform linux/amd64 \
#     -t claude-code-cube:latest \
#     examples/claude-code-integration
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

# Install Claude Code in its own layer so changing the version does not invalidate
# the slow OS + Node.js layer above.
RUN npm install -g --ignore-scripts "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" \
    && node "$(npm root -g)/@anthropic-ai/claude-code/install.cjs" \
    && claude --version \
    && npm cache clean --force \
    && rm -rf /root/.npm

ENV CLAUDE_CODE_DIR=/root/.claude \
    CLAUDE_CODE_SESSION_DIR=/root/.claude/sessions \
    CLAUDE_CODE_TELEMETRY=0 \
    NPM_CONFIG_UPDATE_NOTIFIER=false

RUN mkdir -p /workspace "${CLAUDE_CODE_SESSION_DIR}" \
    && printf '%s\n' \
        '{' \
        '  "quietStartup": true,' \
        '  "enableTelemetry": false' \
        '}' \
        > "${CLAUDE_CODE_DIR}/settings.json"

WORKDIR /workspace

EXPOSE 49983
