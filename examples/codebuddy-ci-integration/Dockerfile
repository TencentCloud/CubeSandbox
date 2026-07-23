# syntax=docker/dockerfile:1.7
#
# CodeBuddy Code CLI template for CubeSandbox CI jobs. The inherited
# cube-entrypoint keeps envd listening on :49983 for Cube's readiness probe.
ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${CUBE_BASE_IMAGE}

ARG DEBIAN_FRONTEND=noninteractive
ARG NODE_MAJOR=24
ARG CODEBUDDY_VERSION=2.125.5

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        bash ca-certificates curl git jq python3 ripgrep \
    && curl -fsSL "https://deb.nodesource.com/setup_${NODE_MAJOR}.x" | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && npm install -g --ignore-scripts "@tencent-ai/codebuddy-code@${CODEBUDDY_VERSION}" \
    && codebuddy --version \
    && npm cache clean --force \
    && rm -rf /var/lib/apt/lists/* /root/.npm

# Keep CodeBuddy's session store on the writable layer. A CubeSandbox snapshot
# therefore captures it together with /workspace and can resume the CI task.
ENV CODEBUDDY_CONFIG_DIR=/root/.codebuddy \
    CODEBUDDY_DISABLE_TELEMETRY=1 \
    NPM_CONFIG_UPDATE_NOTIFIER=false

RUN mkdir -p /workspace "${CODEBUDDY_CONFIG_DIR}"
WORKDIR /workspace
EXPOSE 49983
