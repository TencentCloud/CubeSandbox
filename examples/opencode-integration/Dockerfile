# OpenCode inside a CubeSandbox template.
# The upstream binary and its digest are pinned for reproducible builds.

ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${CUBE_BASE_IMAGE}

ARG DEBIAN_FRONTEND=noninteractive
ARG OPENCODE_VERSION=1.18.9
ARG OPENCODE_SHA256=a0fa4b7b8bdacbd013e79a5f69d4220d36b545cd3ea296ba765f3016fa501b5b

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
        ripgrep \
    && curl -fL --retry 3 \
        "https://github.com/anomalyco/opencode/releases/download/v${OPENCODE_VERSION}/opencode-linux-x64.tar.gz" \
        -o /tmp/opencode.tar.gz \
    && echo "${OPENCODE_SHA256}  /tmp/opencode.tar.gz" | sha256sum -c - \
    && tar -xzf /tmp/opencode.tar.gz -C /usr/local/bin opencode \
    && chmod 0755 /usr/local/bin/opencode \
    && opencode --version \
    && rm -f /tmp/opencode.tar.gz \
    && rm -rf /var/lib/apt/lists/*

ENV XDG_CONFIG_HOME=/root/.config \
    XDG_DATA_HOME=/root/.local/share \
    XDG_STATE_HOME=/root/.local/state \
    XDG_CACHE_HOME=/root/.cache \
    OPENCODE_DISABLE_AUTOUPDATE=1 \
    OPENCODE_DISABLE_SHARE=1 \
    OPENCODE_DISABLE_MODELS_FETCH=1 \
    OPENCODE_DISABLE_DEFAULT_PLUGINS=1 \
    OPENCODE_DISABLE_LSP_DOWNLOAD=1

COPY opencode.json /root/.config/opencode/opencode.json

RUN mkdir -p \
        /workspace \
        /root/.local/share/opencode \
        /root/.local/state/opencode \
        /root/.cache/opencode \
    && chmod 0700 \
        /root/.config/opencode \
        /root/.local/share/opencode \
        /root/.local/state/opencode

WORKDIR /workspace

# envd is inherited from cubesandbox-base and serves this readiness endpoint.
EXPOSE 49983
