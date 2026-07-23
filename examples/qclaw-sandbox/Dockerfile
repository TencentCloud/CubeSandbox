# CubeSandbox 模板：OpenClaw (QClaw) 智能体网关沙箱
#
# 基于 CubeSandbox 官方基础镜像，安装 Node.js 22 和 OpenClaw 网关。
# 继承的 cube-entrypoint 在 :49983 端口保持 envd 运行，
# 供 Cube 使用标准就绪探针。
# OpenClaw 作为 supervisor 管理的守护进程运行在 18789 端口。
#
# 构建：
#   docker build -t qclaw-cube:latest examples/qclaw-sandbox
#
# 注册为 Cube 模板：
#   cubemastercli tpl create-from-image \
#     --image <your-registry>/qclaw-cube:latest \
#     --writable-layer-size 4G \
#     --expose-port 49983 \
#     --probe       49983 \
#     --probe-path  /health

ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${CUBE_BASE_IMAGE}

ARG DEBIAN_FRONTEND=noninteractive
ARG NODE_MAJOR=22
ARG QCLAW_VERSION=0.2.34

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
        supervisor \
    && curl -fsSL "https://deb.nodesource.com/setup_${NODE_MAJOR}.x" | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*

# 将 OpenClaw 安装在独立层中，这样更新 QCLAW_VERSION 时
# 不会使上面耗时的 OS + Node.js 层缓存失效。
# OpenClaw 可能需要内部 npm 源，可通过 --build-arg NPM_REGISTRY=<url> 指定。
ARG NPM_REGISTRY=
RUN if [ -n "${NPM_REGISTRY}" ]; then \
        npm config set registry "${NPM_REGISTRY}"; \
    fi \
    && npm install -g --ignore-scripts "openclaw@${QCLAW_VERSION}" \
    && openclaw --version \
    && npm cache clean --force \
    && rm -rf /root/.npm

# OpenClaw 状态目录（遵循 CubeOps AgentHub 约定）。
RUN mkdir -p /root/.openclaw/agents/main/agent \
             /root/.openclaw/agents/main/sessions \
             /root/.openclaw/workspace

# 最小初始配置，使 OpenClaw 无需首次设置即可启动。
# 完整配置由 CubeOps 在运行时通过 envd apply 脚本写入。
# 包含一个默认开发令牌，确保网关就绪检查脚本能正常运行。
RUN printf '%s\n' \
    '{' \
    '  "gateway": {' \
    '    "bind": "lan",' \
    '    "port": 18789,' \
    '    "mode": "local",' \
    '    "auth": {"mode": "token", "token": "dev-token-cubesandbox"}' \
    '  },' \
    '  "agents": {' \
    '    "defaults": {' \
    '      "workspace": "/root/.openclaw/workspace"' \
    '    }' \
    '  },' \
    '  "session": {"dmScope": "per-channel-peer"}' \
    '}' > /root/.openclaw/openclaw.json

# OpenClaw 网关进程的 Supervisor 配置。
RUN mkdir -p /opt/gem/supervisord /var/log \
    && printf '%s\n' \
        '[program:openclaw]' \
        'command=openclaw gateway run' \
        'directory=/root' \
        'autostart=true' \
        'autorestart=true' \
        'stdout_logfile=/var/log/openclaw.log' \
        'stderr_logfile=/var/log/openclaw.log' \
        'environment=NODE_ENV="production",OPENCLAW_DEFAULT_MODEL="anthropic/claude-sonnet-4-6",OPENCLAW_BIND="lan"' \
        > /opt/gem/supervisord/openclaw.conf

RUN mkdir -p /workspace

ENV OPENCLAW_DEFAULT_MODEL=anthropic/claude-sonnet-4-6 \
    OPENCLAW_BIND=lan \
    NODE_EXTRA_CA_CERTS=/etc/ssl/certs/ca-certificates.crt

WORKDIR /workspace

EXPOSE 49983 18789
