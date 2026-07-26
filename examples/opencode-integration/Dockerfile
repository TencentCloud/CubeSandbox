# syntax=docker/dockerfile:1.7
#
# OpenCode inside a CubeSandbox template.
#
# The inherited cube-entrypoint keeps envd running on :49983 and serves
# /health, so Cube can use the standard readiness probe.

ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16@sha256:34ea312a63a5534e66ab17005c23d7fbaf33c38bccd5411ee402d901e63a3193
FROM ${CUBE_BASE_IMAGE}

ARG DEBIAN_FRONTEND=noninteractive
ARG NODE_VERSION=22.23.1
ARG NODE_SHA256=7a8cb04b4a1df4eaf432125324b81b29a088e73570a23259a8de1c65d07fc129
ARG OPENCODE_VERSION=1.17.13

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        bash \
        ca-certificates \
        curl \
        git \
        jq \
        less \
        procps \
        python3 \
        python3-pip \
        ripgrep \
    && rm -rf /var/lib/apt/lists/*

RUN curl -fsSLo /tmp/node.tar.gz \
        "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-x64.tar.gz" \
    && echo "${NODE_SHA256}  /tmp/node.tar.gz" | sha256sum --check --strict \
    && tar -xzf /tmp/node.tar.gz -C /usr/local --strip-components=1 --no-same-owner \
    && rm -f /tmp/node.tar.gz \
    && node --version \
    && npm --version

RUN npm install -g "opencode-ai@${OPENCODE_VERSION}" \
    && opencode --version \
    && npm cache clean --force \
    && rm -rf /root/.npm

ENV OPENCODE_DISABLE_AUTOUPDATE=1 \
    OPENCODE_DISABLE_PRUNE=1 \
    OPENCODE_DISABLE_TERMINAL_TITLE=1 \
    NPM_CONFIG_UPDATE_NOTIFIER=false

RUN mkdir -p /workspace /root/.local/share/opencode /root/.config/opencode

WORKDIR /workspace

EXPOSE 49983
