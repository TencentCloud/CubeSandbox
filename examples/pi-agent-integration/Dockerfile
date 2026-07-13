# syntax=docker/dockerfile:1.7
#
# Pi coding agent inside a CubeSandbox template.
#
# The image installs Node.js 24 and the Pi coding-agent CLI on top of the
# official CubeSandbox base image. The inherited cube-entrypoint keeps envd
# running on :49983 so Cube can use the standard readiness probe.
#
# Build:
#   docker build -t pi-agent-cube:latest examples/pi-agent-integration
#
# Register as a Cube template:
#   cubemastercli tpl create-from-image \
#     --image <your-registry>/pi-agent-cube:latest \
#     --writable-layer-size 4G \
#     --expose-port 49983 \
#     --probe       49983 \
#     --probe-path  /health

ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${CUBE_BASE_IMAGE}

ARG DEBIAN_FRONTEND=noninteractive
ARG NODE_MAJOR=24
ARG PI_VERSION=0.80.3

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

# Install Pi in its own layer so bumping PI_VERSION does not invalidate the slow
# OS + Node.js layer above (only this short layer rebuilds on a version bump).
RUN npm install -g --ignore-scripts "@earendil-works/pi-coding-agent@${PI_VERSION}" \
    && pi --version \
    && npm cache clean --force \
    && rm -rf /root/.npm

ENV PI_CODING_AGENT_DIR=/root/.pi/agent \
    PI_CODING_AGENT_SESSION_DIR=/root/.pi/agent/sessions \
    PI_SKIP_VERSION_CHECK=1 \
    PI_TELEMETRY=0 \
    NPM_CONFIG_UPDATE_NOTIFIER=false

RUN mkdir -p /workspace "${PI_CODING_AGENT_SESSION_DIR}" \
    && printf '%s\n' \
        '{' \
        '  "quietStartup": true,' \
        '  "enableInstallTelemetry": false' \
        '}' \
        > "${PI_CODING_AGENT_DIR}/settings.json"

WORKDIR /workspace

EXPOSE 49983
