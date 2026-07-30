# syntax=docker/dockerfile:1.7
#
# Minimal toolbox profile on cubesandbox-base for host argv allowlist demos.
# See README.md for build / create-from-image / limits.
#
#   docker build -t agent-tool-allowlist-sandbox:latest .

ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${CUBE_BASE_IMAGE}

ARG DEBIAN_FRONTEND=noninteractive
# Optional: docker build --build-arg INSTALL_CURL=1 …  (only for egress demos)
ARG INSTALL_CURL=0

RUN apt-get update \
    && apt-get install -y --no-install-recommends coreutils findutils \
    && if [ "$INSTALL_CURL" = "1" ]; then \
         apt-get install -y --no-install-recommends curl; \
       fi \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /etc/cube-sandbox \
    && printf '%s\n' \
        echo uname pwd ls cat head wc sha256sum \
        > /etc/cube-sandbox/tool-profile.txt

EXPOSE 49983
