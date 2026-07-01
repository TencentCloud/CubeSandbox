FROM cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest

ENV DEBIAN_FRONTEND=noninteractive
ENV NPM_CONFIG_UPDATE_NOTIFIER=false
ENV OPENCODE_DISABLE_AUTOUPDATE=true

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        git \
        nodejs \
        npm \
        python3 \
        python3-pip \
        ripgrep \
    && npm install -g opencode-ai \
    && mkdir -p /workspace \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /workspace
