# syntax=docker/dockerfile:1.7
#
# OpenCode inside a CubeSandbox template.
#
# The image installs Node.js 20 (OpenCode's npm package expects Node 18+, 20 is
# the long-lived line that OpenCode currently tests against) and the opencode-ai
# CLI on top of the official CubeSandbox base image. The inherited
# cube-entrypoint keeps envd running on :49983 so Cube can use the standard
# readiness probe.
#
# Build:
#   docker build --pull -t opencode-cube:latest examples/opencode-integration
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
ARG NODE_MAJOR=20
ARG OPENCODE_VERSION=1.17.20

# OpenCode stores its **config** under ``$XDG_CONFIG_HOME/opencode`` (default
# ``~/.config/opencode``) and its **data** — auth.json, session storage,
# logs, tool-output, snapshots — under ``$XDG_DATA_HOME/opencode`` (default
# ``~/.local/share/opencode``). Two different paths because OpenCode
# follows the XDG Base Directory spec.
#
# We relocate both to ``/workspace`` for two reasons:
#
# 1. The e2b exec channel constrains us to a fixed allow-list of usernames
#    (``root``, ``user``); an image-level USER directive that creates an
#    ``opencode`` account does not propagate to the exec user. OpenCode's
#    default $HOME-relative paths therefore need to be world-writable for
#    the SDK-allowed exec user to write into them, which weakens the
#    privilege drop.
# 2. Keeping both trees under /workspace means the snapshot captures the
#    project tree, the OpenCode config, and the OpenCode session/storage
#    cache together, which is the natural unit for pause/resume —
#    operators get "what was the agent doing" alongside "what files did
#    it leave behind" without reaching into hidden home dirs.
ARG OPENCODE_CONFIG_DIR=/workspace/.opencode/config
ARG OPENCODE_DATA_DIR=/workspace/.opencode/data

# NodeSource signing key (primary fingerprint, not the subkey). Update by
# running: curl -fsSL https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key \
#               | gpg --show-keys --with-colons | grep ^fpr
# Verified against https://github.com/nodesource/distributions on 2026-07-18.
ARG NODESOURCE_GPG_FPR=6F71F525282841EEDAF851B42F59B5F99B1BE0B4

# Layer 1 — system packages + NodeSource repo. Splitting this off from the
# OpenCode install layer keeps the slow apt + NodeSource GPG step cacheable
# across OpenCode version bumps.
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

# Layer 2 — OpenCode. Skips dev deps (50-200 MB of tooling the headless CLI
# never loads) and post-install scripts (the npm postinstall runs a binary
# downloader; we let it execute because it places platform-specific
# artifacts under the package, but we keep --omit=dev to skip the rest).
RUN npm install -g --omit=dev \
        "opencode-ai@${OPENCODE_VERSION}" \
    && opencode --version \
    && rm -rf /root/.npm

# Create an unprivileged user that owns the workspace. OpenCode is an
# autonomous code-generation agent with Bash, file-edit, and shell tool
# access — running it as root turns any prompt-injection or CLI bug into
# a root-level compromise inside the sandbox. The MicroVM still provides
# the outer isolation, but defense in depth matters for long-running
# sessions.
#
# UID/GID are auto-assigned because the base image already owns uid=1000 as
# the ``user`` exec account; pause/resume preserves identity by username,
# not by numeric id.
#
# /workspace is world-writable because the e2b exec channel constrains us
# to the SDK's allowed-users list (``root``, ``user``) and ``user`` cannot
# write to an opencode-owned directory. The MicroVM provides the
# isolation; the world write is a single-purpose concession to the SDK's
# allowed users. The home directory under /home/opencode is kept at 0700.
RUN groupadd --system opencode \
    && useradd  --system --gid opencode \
                --home-dir /home/opencode --shell /bin/bash \
                --no-create-home opencode \
    && install -d -o opencode -g opencode -m 0700 /home/opencode \
    && install -d -o opencode -g opencode -m 0777 /workspace

# Pin both config & data directories under /workspace so the paths are
# stable across pause/resume snapshots and the empty-state below is created
# at build time (no leftover tenant state ever leaks into the base
# template). XDG_CONFIG_HOME is also forced so OpenCode never falls back to
# ~/.config inside /home/opencode (which the exec user ``user`` could not
# write).
ENV OPENCODE_CONFIG_DIR=${OPENCODE_CONFIG_DIR} \
    OPENCODE_DATA_DIR=${OPENCODE_DATA_DIR} \
    XDG_CONFIG_HOME=/workspace \
    XDG_DATA_HOME=/workspace \
    OPENCODE_DISABLE_AUTOUPDATE=1 \
    NPM_CONFIG_UPDATE_NOTIFIER=false

# Pre-create the workspace and the OpenCode state trees so the first exec
# call does not race the directory creation. The base template ships with
# an empty config dir; per-tenant credentials / sessions are populated at
# runtime so a paused snapshot cannot replay a tenant-specific chain.
RUN install -d -o opencode -g opencode -m 0777 "${OPENCODE_CONFIG_DIR}" "${OPENCODE_DATA_DIR}" \
    && install -d -o opencode -g opencode -m 0777 "${OPENCODE_DATA_DIR}/storage" \
    && printf '{}\n' > "${OPENCODE_CONFIG_DIR}/opencode.json"

WORKDIR /workspace

# Drop root. OpenCode and any subsequent shell commands run as opencode.
# If cube-entrypoint needs root (e.g. to bind :49983), the inherited image
# already runs the entrypoint before this USER takes effect — confirm against
# the base image's entrypoint before changing this.
USER opencode

EXPOSE 49983
