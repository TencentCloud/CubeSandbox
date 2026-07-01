# syntax=docker/dockerfile:1.7
#
# Claude Code inside a CubeSandbox template.
#
# The image stacks Node.js LTS + Anthropic's official `@anthropic-ai/claude-code`
# CLI on top of `cubesandbox-base`, which already ships `envd` on `:49983`
# so the resulting sandbox passes the readiness probe out of the box.
#
# Build:
#   docker build -t claude-code-cube:latest examples/claude-code-integration
#
# Register as a Cube template (see README.md for details):
#   cubemastercli tpl create-from-image \
#     --image <your-registry>/claude-code-cube:latest \
#     --writable-layer-size 4G \
#     --expose-port 49983 \
#     --probe       49983 \
#     --probe-path  /health

ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${CUBE_BASE_IMAGE}

ARG DEBIAN_FRONTEND=noninteractive
ARG NODE_MAJOR=20
ARG CLAUDE_CODE_VERSION=latest

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
         ca-certificates curl gnupg git jq ripgrep less \
         python3 python3-pip build-essential \
    && curl -fsSL "https://deb.nodesource.com/setup_${NODE_MAJOR}.x" | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/* \
    && npm install -g "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" \
    && claude --version \
    && npm cache clean --force \
    && rm -rf /root/.npm

ENV CLAUDE_CODE_HOME=/root/.claude \
    ANTHROPIC_MODEL="" \
    ANTHROPIC_BASE_URL=""

RUN mkdir -p "${CLAUDE_CODE_HOME}" /workspace \
    && printf '%s\n' \
        '# Placeholder settings.' \
        '# Runtime settings live under /root/.claude/settings.json and are' \
        '# populated at sandbox creation time via envd env vars or session' \
        '# writes so secrets never bake into the image.' \
        > "${CLAUDE_CODE_HOME}/README.md"

WORKDIR /workspace

EXPOSE 49983
