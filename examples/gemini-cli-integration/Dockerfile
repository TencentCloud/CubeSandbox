# syntax=docker/dockerfile:1.7
# Gemini CLI image for a CubeSandbox template.
# The base image already contains cube-entrypoint and envd on :49983.

ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${CUBE_BASE_IMAGE}

ARG DEBIAN_FRONTEND=noninteractive
ARG NODE_MAJOR=22
ARG GEMINI_CLI_VERSION=latest

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        bash \
        ca-certificates \
        curl \
        git \
        gnupg \
        jq \
        python3 \
        python3-pip \
        ripgrep \
    && curl -fsSL "https://deb.nodesource.com/setup_${NODE_MAJOR}.x" | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*

# Pin GEMINI_CLI_VERSION in production builds after validating an upgrade.
RUN npm install -g --ignore-scripts "@google/gemini-cli@${GEMINI_CLI_VERSION}" \
    && gemini --version \
    && npm cache clean --force \
    && rm -rf /root/.npm

ENV GEMINI_CLI_HOME=/root/.gemini \
    GEMINI_CLI_TELEMETRY_ENABLED=false \
    NPM_CONFIG_UPDATE_NOTIFIER=false

RUN mkdir -p /workspace "${GEMINI_CLI_HOME}"

WORKDIR /workspace
EXPOSE 49983
