# syntax=docker/dockerfile:1.7
#
# OpenCode inside a CubeSandbox template.
#
# The inherited cube-entrypoint keeps envd listening on :49983. Do not replace
# the base image ENTRYPOINT or CMD: CubeSandbox uses envd for command, file, and
# readiness APIs.

ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${CUBE_BASE_IMAGE}

ARG DEBIAN_FRONTEND=noninteractive
ARG NODE_MAJOR=24
ARG OPENCODE_VERSION=1.17.18

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

# opencode-ai's postinstall script installs the platform-specific binary, so
# npm lifecycle scripts must remain enabled. Pinning the version makes template
# rebuilds deterministic and prevents an upstream CLI change from silently
# changing the documented command contract.
RUN npm install -g "opencode-ai@${OPENCODE_VERSION}" \
    && opencode --version \
    && npm cache clean --force \
    && rm -rf /root/.npm

ENV OPENCODE_AUTO_SHARE=false \
    OPENCODE_DISABLE_AUTOUPDATE=true \
    OPENCODE_DISABLE_PRUNE=true \
    OPENCODE_DISABLE_TERMINAL_TITLE=true \
    NPM_CONFIG_UPDATE_NOTIFIER=false

RUN mkdir -p \
        /workspace \
        /root/.config/opencode \
        /root/.local/share/opencode

WORKDIR /workspace

EXPOSE 49983
