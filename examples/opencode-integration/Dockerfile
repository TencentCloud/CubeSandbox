# syntax=docker/dockerfile:1.7
#
# OpenCode coding agent inside a CubeSandbox template.
#
# The image installs Node.js 24 and the opencode-ai npm package on top of the
# official CubeSandbox base image. The inherited cube-entrypoint keeps envd
# running on :49983 so Cube can use the standard readiness probe.
#
# Build:
#   docker build -t opencode-cube:latest examples/opencode-integration
#
# Register as a Cube template:
#   cubemastercli tpl create-from-image \
#     --image <your-registry>/opencode-cube:latest \
#     --writable-layer-size 4G \
#     --expose-port 49983 \
#     --probe       49983 \
#     --probe-path  /health

ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${CUBE_BASE_IMAGE}

ARG DEBIAN_FRONTEND=noninteractive
ARG NODE_MAJOR=24
ARG OPENCODE_VERSION=0.5.0

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

# Install OpenCode in its own layer so bumping OPENCODE_VERSION does not
# invalidate the slow OS + Node.js layer above.
RUN npm install -g --ignore-scripts "opencode-ai@${OPENCODE_VERSION}" \
    && opencode --version \
    && npm cache clean --force \
    && rm -rf /root/.npm

ENV OPENCODE_DIR=/root/.opencode \
    OPENCODE_TELEMETRY=0 \
    NPM_CONFIG_UPDATE_NOTIFIER=false

RUN mkdir -p /workspace "${OPENCODE_DIR}"

WORKDIR /workspace

EXPOSE 49983
