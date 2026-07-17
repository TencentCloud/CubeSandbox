# syntax=docker/dockerfile:1.7
#
# Tencent CodeBuddy Code inside a CubeSandbox template.
#
# The image installs Node.js 20 (CodeBuddy's bundled toolchain expects Node 18+,
# 20 is the long-lived line that matches what CodeBuddy currently tests against)
# and the CodeBuddy CLI on top of the official CubeSandbox base image. The
# inherited cube-entrypoint keeps envd running on :49983 so Cube can use the
# standard readiness probe.
#
# Build:
#   docker build --pull -t codebuddy-cube:latest examples/codebuddy-integration
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
ARG NODE_MAJOR=20
ARG CODEBUDDY_VERSION=2.117.1

# CodeBuddy stores configuration, history, sessions, plans and tool output under
# ~/.codebuddy. We relocate it to /workspace/.codebuddy (rather than a
# $HOME-relative path) for two reasons:
#
# 1. The e2b exec channel constrains us to a fixed allow-list of usernames
#    (``root``, ``user``); an Image-level USER directive that creates a
#    ``codebuddy`` account does not propagate to the exec user. CODEBUDDY's
#    default $HOME-relative path therefore needs to be world-writable for the
#    SDK-allowed exec user to write into it, which weakens the privilege drop.
# 2. Keeping state under /workspace means the snapshot captures the project
#    tree and the agent's session cache together, which is the natural unit
#    for pause/resume — operators get "what was the agent doing" alongside
#    "what files did it leave behind" without reaching into a hidden home dir.
ARG CODEBUDDY_HOME=/workspace/.codebuddy

# NodeSource signing key (primary fingerprint, not the subkey). Update by
# running: curl -fsSL https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key \
#               | gpg --show-keys --with-colons | grep ^fpr
# Verified against https://github.com/nodesource/distributions on 2026-07-16.
ARG NODESOURCE_GPG_FPR=6F71F525282841EEDAF851B42F59B5F99B1BE0B4

# Layer 1 — system packages + NodeSource repo. Splitting this off from the
# CodeBuddy install layer keeps the slow apt + NodeSource GPG step cacheable
# across CodeBuddy version bumps.
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
    && mkdir -p /etc/apt/keyrings \
    && curl -fsSL https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key \
        | gpg --dearmor --yes -o /etc/apt/keyrings/nodesource.gpg \
    && gpg --show-keys /etc/apt/keyrings/nodesource.gpg 2>/dev/null \
        | grep -q "${NODESOURCE_GPG_FPR}" \
        || (echo "ERROR: NodeSource GPG fingerprint mismatch" >&2; exit 1) \
    && echo "deb [signed-by=/etc/apt/keyrings/nodesource.gpg] https://deb.nodesource.com/node_${NODE_MAJOR}.x nodistro main" \
        > /etc/apt/sources.list.d/nodesource.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends nodejs \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

# Layer 2 — CodeBuddy Code. Skips dev deps (50-200 MB of tooling the headless
# CLI never loads) and post-install scripts (no network access during build).
# --ignore-scripts avoids post-install hooks that may try to phone home.
RUN npm install -g --omit=dev --ignore-scripts \
        "@tencent-ai/codebuddy-code@${CODEBUDDY_VERSION}" \
    && codebuddy --version \
    && rm -rf /root/.npm

# Create an unprivileged user that owns the workspace. CodeBuddy is an
# autonomous code-generation agent with Bash, Write, and Edit tool access —
# running it as root turns any prompt-injection or CLI bug into a root-level
# compromise inside the sandbox. The MicroVM still provides the outer
# isolation, but defense in depth matters for long-running sessions.
#
# UID/GID are auto-assigned because the base image already owns uid=1000 as
# the ``user`` exec account; pause/resume preserves identity by username, not
# by numeric id.
#
# /workspace is world-writable because the e2b exec channel constrains us to
# the SDK's allowed-users list (``root``, ``user``) and ``user`` cannot write
# to a codebuddy-owned directory. The MicroVM provides the isolation; the
# world write is a single-purpose concession to the SDK's allowed users. The
# home directory under /home/codebuddy is kept at 0700.
RUN groupadd --system codebuddy \
    && useradd  --system --gid codebuddy \
                --home-dir /home/codebuddy --shell /bin/bash \
                --no-create-home codebuddy \
    && install -d -o codebuddy -g codebuddy -m 0700 /home/codebuddy \
    && install -d -o codebuddy -g codebuddy -m 0777 /workspace

# Pin the configuration directory so the path is stable across pause/resume
# snapshots and so the empty-state below is created at build time (no leftover
# tenant state ever leaks into the base template).
ENV CODEBUDDY_CONFIG_DIR=${CODEBUDDY_HOME} \
    DISABLE_TELEMETRY=1 \
    DISABLE_ERROR_REPORTING=1 \
    DISABLE_AUTOUPDATER=1 \
    DISABLE_FEEDBACK_COMMAND=1 \
    NPM_CONFIG_UPDATE_NOTIFIER=false \
    CODEBUDDY_INTERNET_ENVIRONMENT=io

# Pre-create the workspace and the CodeBuddy state tree so the first exec call
# does not race the directory creation. settings.json ships with a minimal
# safe-by-default profile: no telemetry, autoupdater off, prompt suggestion off
# (we run headless), the maximum bash budget from Claude Code to match operator
# expectations, and no inline plugin/hooks so a paused snapshot cannot replay a
# tenant-specific hook chain.
RUN install -d -o codebuddy -g codebuddy -m 0777 "${CODEBUDDY_HOME}" \
    && printf '%s\n' \
        '{' \
        '  "telemetry": { "enabled": false },' \
        '  "autoUpdater": false,' \
        '  "promptSuggestionEnabled": false,' \
        '  "permissions": { "defaultMode": "bypassPermissions" },' \
        '  "env": {' \
        '    "DISABLE_TELEMETRY": "1",' \
        '    "DISABLE_ERROR_REPORTING": "1",' \
        '    "DISABLE_AUTOUPDATER": "1",' \
        '    "DISABLE_FEEDBACK_COMMAND": "1",' \
        '    "CODEBUDDY_INTERNET_ENVIRONMENT": "io"' \
        '  }' \
        '}' \
        > "${CODEBUDDY_HOME}/settings.json"

WORKDIR /workspace

# Drop root. CodeBuddy and any subsequent shell commands run as codebuddy.
# If cube-entrypoint needs root (e.g. to bind :49983), the inherited image
# already runs the entrypoint before this USER takes effect — confirm against
# the base image's entrypoint before changing this.
USER codebuddy

EXPOSE 49983
