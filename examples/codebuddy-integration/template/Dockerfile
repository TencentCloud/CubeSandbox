FROM ghcr.io/tencentcloud/cubesandbox-base:2026.16

ENV DISABLE_AUTOUPDATER=1 \
    CODEBUDDY_CONFIG_DIR=/workspace/.codebuddy

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        git \
        gnupg \
        python3 \
        python3-pip \
        ripgrep \
    && curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && npm install -g @tencent-ai/codebuddy-code@2.114.2 \
    && codebuddy --version \
    && npm cache clean --force \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/cube-entrypoint.sh"]
CMD ["sleep", "infinity"]
