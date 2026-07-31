# MiMo Code inside a CubeSandbox template.
#
# The inherited cube-entrypoint keeps envd listening on :49983 for the
# standard CubeSandbox readiness probe.

ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${CUBE_BASE_IMAGE}

ARG DEBIAN_FRONTEND=noninteractive
ARG NODE_MAJOR=24
ARG MIMO_VERSION=0.1.7

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

# MiMo's npm wrapper installs the matching platform binary. Keep lifecycle
# behavior enabled, pin the package version, and fail the build if the binary
# cannot start on this architecture.
RUN npm install -g --no-audit --no-fund \
        "@mimo-ai/cli@${MIMO_VERSION}" \
        --registry https://registry.npmjs.org \
    && mimo --version \
    && mimo run --help 2>&1 | grep -F -- "--fork" \
    && npm cache clean --force \
    && rm -rf /root/.npm

ENV MIMOCODE_HOME=/root/.mimocode \
    MIMOCODE_PURE=true \
    MIMOCODE_DISABLE_SHARE=true \
    MIMOCODE_DISABLE_AUTOUPDATE=true \
    MIMOCODE_DISABLE_MODELS_FETCH=true \
    MIMOCODE_DISABLE_LSP_DOWNLOAD=true \
    MIMOCODE_DISABLE_EXTERNAL_SKILLS=true \
    MIMOCODE_ENABLE_ANALYSIS=false \
    NPM_CONFIG_UPDATE_NOTIFIER=false

RUN mkdir -p \
        /workspace \
        "${MIMOCODE_HOME}/config" \
        "${MIMOCODE_HOME}/data" \
        "${MIMOCODE_HOME}/state" \
        "${MIMOCODE_HOME}/cache"

WORKDIR /workspace

EXPOSE 49983
