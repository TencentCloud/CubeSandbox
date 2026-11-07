# syntax=docker/dockerfile:1.7
#
# OpenHands agent server baked into a CubeSandbox template.
#
# The cubesandbox-base entrypoint keeps envd on :49983 (the in-guest daemon
# behind the SDK's file/command API) and execs our CMD as the foreground
# service, so the OpenHands agent server is already listening on :8000 when
# the template's boot snapshot is taken. Every sandbox created from this template therefore hot-starts with a
# live agent server — no per-session cold start, unlike DockerWorkspace which
# boots a fresh container per session.
#
# Build:
#   docker build -t openhands-sandbox:latest examples/openhands-integration
#
# Local smoke test (plain Docker, outside CubeSandbox):
#   docker run --rm -d -p 8000:8000 -p 49983:49983 \
#     --name oh-sbx openhands-sandbox:latest
#   curl http://127.0.0.1:8000/ready
#   curl -o /dev/null -w "%{http_code}\n" http://127.0.0.1:49983/health  # => 204

ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${CUBE_BASE_IMAGE}

ARG DEBIAN_FRONTEND=noninteractive
# Pinned server version; bump together with the host-side requirements.txt so
# the SDK and the agent server stay protocol-compatible.
ARG OPENHANDS_AGENT_SERVER_VERSION=1.38.0
ARG PYTHON_VERSION=3.12

# git for agent version-control workflows; system python3 + pip because a
# coding-agent workspace without an interpreter on PATH forces the agent to
# hunt for one (the OpenHands venv python is intentionally not on PATH).
RUN apt-get update \
    && apt-get install -y --no-install-recommends git python3 python3-pip \
    && rm -rf /var/lib/apt/lists/*

# The published cubesandbox-base is Ubuntu 22.04 (Python 3.10), while the
# OpenHands SDK requires Python >= 3.12. Install a standalone CPython via uv
# so the same Dockerfile works on any base revision and on amd64/arm64 alike.
# The installer URL and uv version are pinned for reproducible rebuilds.
ARG UV_VERSION=0.11.33
RUN curl -fsSL "https://astral.sh/uv/${UV_VERSION}/install.sh" \
    | UV_INSTALL_DIR=/usr/local/bin sh

# UV_PYTHON_INSTALL_DIR must be world-readable: the venv's interpreter symlink
# points into it, and the agent server runs as the unprivileged `user` account
# (uv's default install dir under /root is not traversable by uid 1000).
# openhands-sdk is pinned explicitly so a rebuild cannot silently pull a
# newer SDK than the server/tools were released with.
RUN UV_PYTHON_INSTALL_DIR=/opt/uv/python uv venv /opt/openhands --python ${PYTHON_VERSION} \
    && VIRTUAL_ENV=/opt/openhands uv pip install --no-cache \
        openhands-agent-server==${OPENHANDS_AGENT_SERVER_VERSION} \
        openhands-tools==${OPENHANDS_AGENT_SERVER_VERSION} \
        openhands-sdk==${OPENHANDS_AGENT_SERVER_VERSION}

# Python TLS clients (httpx under litellm in the agent server) default to
# certifi's bundle and ignore the system trust store, so point them at the
# system bundle explicitly. With the CubeEgress CA bake at template
# registration (--with-cube-ca, the default) that store includes the
# interception root, so credential-injection egress rules work against the
# pre-warmed server without image changes; without CubeEgress the system
# bundle is a superset of certifi's roots and nothing changes.
ENV SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt \
    REQUESTS_CA_BUNDLE=/etc/ssl/certs/ca-certificates.crt

# Agent working directory. Owned by the uid-1000 `user` account that envd also
# uses for file/command operations, so files created by the agent and via the
# E2B-compatible SDK stay mutually accessible.
RUN mkdir -p /workspace && chown user:user /workspace

EXPOSE 8000 49983

# Drop to the unprivileged `user` account for the agent server; envd (started
# by the inherited cube-entrypoint) keeps running on :49983. sudo -E preserves
# env so SESSION_API_KEY can be injected for the optional authenticated mode.
CMD ["sudo", "-E", "-H", "-u", "user", "/bin/sh", "-c", \
     "cd /workspace && exec /opt/openhands/bin/agent-server --host 0.0.0.0 --port 8000"]
