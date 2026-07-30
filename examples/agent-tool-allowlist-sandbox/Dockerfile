# syntax=docker/dockerfile:1.7
#
# Agent tool toolbox profile on cubesandbox-base (BYOI template for #645).
#
# This image documents the *intended* guest tool set that pairs with the
# host-side argv gate in ../code-sandbox-quickstart/tool_allowlist.py.
# It does NOT remove bash from the base image (honest limitation) — guest
# confinement still relies on MicroVM isolation + host allowlisting + egress.
#
# Build:
#   docker build -t agent-tool-allowlist-sandbox:latest \
#     examples/agent-tool-allowlist-sandbox
#
# Register:
#   cubemastercli tpl create-from-image \
#     --image <registry>/agent-tool-allowlist-sandbox:latest \
#     --writable-layer-size 1G \
#     --expose-port 49983 \
#     --probe 49983 --probe-path /health

ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${CUBE_BASE_IMAGE}

ARG DEBIAN_FRONTEND=noninteractive

# Small toolbox used by the host allowlist demos (echo/cat/uname/…).
# Keep the layer lean; do not treat this as a full code-interpreter image.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        coreutils \
        findutils \
        curl \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /etc/cube-sandbox \
    && printf '%s\n' \
        'echo' \
        'uname' \
        'pwd' \
        'ls' \
        'cat' \
        'head' \
        'wc' \
        'sha256sum' \
        > /etc/cube-sandbox/tool-profile.txt \
    && printf '%s\n' \
        'profile=agent-tool-allowlist' \
        'host_gate=examples/code-sandbox-quickstart/tool_allowlist.py' \
        'note=host argv gate is authoritative; this file is the guest intent marker' \
        > /etc/cube-sandbox/tool-profile.meta

# Inherit cube-entrypoint (envd on :49983). Empty CMD keeps base behavior.
EXPOSE 49983
