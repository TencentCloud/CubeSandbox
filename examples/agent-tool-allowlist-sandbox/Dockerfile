# syntax=docker/dockerfile:1.7
#
# Agent toolbox BYOI: cubesandbox-base + guest cube-tool runner + tool-profile.
# Host demos allowlist "cube-tool" (and/or bare toolbox names); the wrapper
# re-checks the real tool name inside the MicroVM.
#
#   docker build -t agent-tool-allowlist-sandbox:latest .
#
# Base image already ships coreutils (echo/cat/…). Skip apt unless INSTALL_CURL=1.

ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${CUBE_BASE_IMAGE}

ARG DEBIAN_FRONTEND=noninteractive
# Optional: docker build --build-arg INSTALL_CURL=1 …  (only for egress demos)
ARG INSTALL_CURL=0

RUN mkdir -p /etc/cube-sandbox /workspace /usr/local/bin \
    && if [ "$INSTALL_CURL" = "1" ]; then \
         apt-get update \
         && apt-get install -y --no-install-recommends curl \
         && rm -rf /var/lib/apt/lists/*; \
       fi

COPY tool-profile.txt /etc/cube-sandbox/tool-profile.txt
COPY guest/cube-tool /usr/local/bin/cube-tool
COPY workspace/ /workspace/

RUN sed -i 's/\r$//' /usr/local/bin/cube-tool /etc/cube-sandbox/tool-profile.txt \
    && chmod 0755 /usr/local/bin/cube-tool \
    && /usr/local/bin/cube-tool echo "cube-tool-ok" \
    && if /usr/local/bin/cube-tool bash -c id; then echo "cube-tool must deny bash" >&2; exit 1; fi \
    && test -f /workspace/README.txt

WORKDIR /workspace
ENV CUBE_TOOL_PROFILE=/etc/cube-sandbox/tool-profile.txt

EXPOSE 49983
