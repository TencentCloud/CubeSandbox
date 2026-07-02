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
    && mkdir -p /etc/apt/keyrings \
    && curl -fsSL https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key \
        | gpg --dearmor -o /etc/apt/keyrings/nodesource.gpg \
    && chmod 0644 /etc/apt/keyrings/nodesource.gpg \
    && echo "deb [signed-by=/etc/apt/keyrings/nodesource.gpg] https://deb.nodesource.com/node_22.x nodistro main" \
        > /etc/apt/sources.list.d/nodesource.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends nodejs \
    && npm install -g @tencent-ai/codebuddy-code@2.114.2 \
    && codebuddy --version \
    && npm cache clean --force \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/cube-entrypoint.sh"]
CMD ["sleep", "infinity"]
