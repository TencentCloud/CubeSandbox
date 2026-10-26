# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Multi-stage build for the K8s cube-shim component image.
#
# Build context MUST be the repository root:
#   docker build -f CubeShim/Dockerfile -t cube-shim:TAG .
#
# Requires BuildKit (CubeShim/Dockerfile.dockerignore). Builder stage
# defaults to cubesandbox-builder (Rust toolchain for CubeShim + hypervisor).
# Override with --build-arg CUBE_BUILDER_IMAGE=...
#
# See .github/workflows/release-docker-images.yml and
# deploy/kubernetes/images/build-cube-images.sh.

# ── Build stage ────────────────────────────────────────────────────────────────
ARG CUBE_BUILDER_IMAGE=ghcr.io/tencentcloud/cubesandbox-builder:ubuntu2004
FROM ${CUBE_BUILDER_IMAGE} AS builder

WORKDIR /workspace

COPY CubeShim/ /workspace/CubeShim/
COPY hypervisor/ /workspace/hypervisor/

# Same as root Makefile `shim`: cargo build --release --locked in CubeShim.
RUN set -eux; \
    mkdir -p /out/cube-shim/bin /out/cube-shim/conf; \
    cd /workspace/CubeShim; \
    cargo build --release --locked; \
    install -m 0755 target/release/containerd-shim-cube-rs /out/cube-shim/bin/containerd-shim-cube-rs; \
    install -m 0755 target/release/cube-runtime /out/cube-shim/bin/cube-runtime

COPY deploy/one-click/config-cube.toml /out/cube-shim/conf/config-cube.toml

# ── Runtime stage ──────────────────────────────────────────────────────────────
FROM ubuntu:22.04

ARG DEBIAN_FRONTEND=noninteractive
ARG CUBE_VERSION=unknown

ENV IMAGE_ROOT=/opt/cube-image
ENV TOOLBOX_ROOT=/usr/local/services/cubetoolbox
ENV CUBE_COMPONENT=cube-shim
ENV CUBE_ROLE=install

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
      ca-certificates \
      tini \
      util-linux \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /out/cube-shim ${IMAGE_ROOT}/cube-shim
COPY deploy/kubernetes/images/scripts/component-entrypoint.sh /usr/local/bin/component-entrypoint.sh

RUN printf '{"schema_version":1,"components":{"containerd-shim-cube-rs":{"version":"%s"},"cube-runtime":{"version":"%s"}}}\n' \
      "${CUBE_VERSION}" "${CUBE_VERSION}" > ${IMAGE_ROOT}/cube-shim/version.json \
    && chmod +x \
      ${IMAGE_ROOT}/cube-shim/bin/cube-runtime \
      ${IMAGE_ROOT}/cube-shim/bin/containerd-shim-cube-rs \
      /usr/local/bin/component-entrypoint.sh

ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/component-entrypoint.sh"]
