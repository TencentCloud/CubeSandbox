# syntax=docker/dockerfile:1.7
#
# CodeBuddy coding agent inside a CubeSandbox template.
#
# The image installs Node.js 24 and the CodeBuddy CLI npm package on top of the
# official CubeSandbox base image. The inherited cube-entrypoint keeps envd
# running on :49983 so Cube can use the standard readiness probe.
#
# Build:
#   docker build -t codebuddy-cube:latest examples/codebuddy-integration
#
# Register as a Cube template:
#   cubemastercli tpl create-from-image \
#     --image <your-registry>/codebuddy-cube:latest \
#     --writable-layer-size 4G \
#     --expose-port 49983 \
#     --probe       49983 \
#     --probe-path  /health

ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${CUBE_BASE_IMAGE}

ARG DEBIAN_FRONTEND=noninteractive
ARG NODE_MAJOR=24
ARG CODEBUDDY_VERSION=2.119.2

ENV CODEBUDDY_HOME=/root/.codebuddy \
    CODEBUDDY_TELEMETRY=0 \
    NPM_CONFIG_UPDATE_NOTIFIER=false

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
    && curl -fsSL "https://deb.nodesource.com/setup_${NODE_MAJOR}.x" | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*

# Install CodeBuddy CLI in its own layer so bumping CODEBUDDY_VERSION does not
# invalidate the slow OS + Node.js layer above (only this short layer rebuilds
# on a version bump).
RUN npm install -g --no-audit --no-fund "@tencent-ai/codebuddy-code@${CODEBUDDY_VERSION}" \
    && codebuddy --version \
    && rm -rf /root/.npm

RUN mkdir -p /workspace "${CODEBUDDY_HOME}" \
    && printf '%s\n' \
        '{' \
        '  "telemetry": false,' \
        '  "quietStartup": true' \
        '}' \
        > "${CODEBUDDY_HOME}/settings.json"

WORKDIR /workspace

EXPOSE 49983
