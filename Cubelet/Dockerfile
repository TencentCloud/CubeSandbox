# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Multi-stage build for the K8s cubelet component image.
#
# Build context MUST be the repository root:
#   docker build -f Cubelet/Dockerfile -t cubelet:TAG .
#
# Requires BuildKit (Cubelet/Dockerfile.dockerignore). Builder stage defaults to
# the project cubesandbox-builder image (Go + Rust + gcc for CGO/cubecow).
# Override with --build-arg CUBE_BUILDER_IMAGE=...
#
# See .github/workflows/release-docker-images.yml and
# deploy/kubernetes/images/build-cube-images.sh.

# ── Build stage ────────────────────────────────────────────────────────────────
ARG CUBE_BUILDER_IMAGE=ghcr.io/tencentcloud/cubesandbox-builder:ubuntu2004
FROM ${CUBE_BUILDER_IMAGE} AS builder

ARG TARGETARCH
ARG CUBE_VERSION=0.0.0-dev
ARG CUBE_COMMIT=unknown
ARG CUBE_BUILD_TIME=unknown

WORKDIR /workspace

COPY cubecow/ /workspace/cubecow/
COPY cubelog/ /workspace/cubelog/
COPY Cubelet/ /workspace/Cubelet/

# cubecow static library (same as root `make cubecow-sdk` inside the builder).
RUN set -eux; \
    mkdir -p /workspace/Cubelet/third_party/cubecow/lib \
             /workspace/Cubelet/third_party/cubecow/include; \
    (cd /workspace/cubecow && cargo build --release -p cubecow); \
    install -m 0644 /workspace/cubecow/target/release/libcubecow.a \
      /workspace/Cubelet/third_party/cubecow/lib/libcubecow.a; \
    install -m 0644 /workspace/cubecow/include/cubecow.h \
      /workspace/Cubelet/third_party/cubecow/include/cubecow.h

# Regenerate protobuf stubs (same as root `make cubelet` → make proto).
ENV CGO_ENABLED=1
RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) goarch=amd64 ;; \
      arm64) goarch=arm64 ;; \
      *) echo "unsupported TARGETARCH: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    mkdir -p /out/bin; \
    cd /workspace/Cubelet; \
    go mod download; \
    make proto; \
    ldflags="-s -w \
      -X 'github.com/tencentcloud/CubeSandbox/Cubelet/pkg/version.Version=${CUBE_VERSION}' \
      -X 'github.com/tencentcloud/CubeSandbox/Cubelet/pkg/version.Commit=${CUBE_COMMIT}' \
      -X 'github.com/tencentcloud/CubeSandbox/Cubelet/pkg/version.BuildTime=${CUBE_BUILD_TIME}'"; \
    GOOS=linux GOARCH="${goarch}" go build -ldflags "${ldflags}" \
      -o /out/bin/cubelet ./cmd/cubelet; \
    GOOS=linux GOARCH="${goarch}" go build -ldflags "${ldflags}" \
      -o /out/bin/cubecli ./cmd/cubecli

# Assemble one-click-compatible Cubelet tree for the runtime stage.
RUN set -eux; \
    mkdir -p /out/Cubelet/bin /out/Cubelet/config /out/Cubelet/dynamicconf /out/Cubelet/plugin; \
    cp /out/bin/cubelet /out/bin/cubecli /out/Cubelet/bin/; \
    cp -a /workspace/Cubelet/config/. /out/Cubelet/config/; \
    cp -a /workspace/Cubelet/dynamicconf/. /out/Cubelet/dynamicconf/; \
    if [ -f /workspace/Cubelet/contrib/nicl ]; then \
      cp /workspace/Cubelet/contrib/nicl /out/Cubelet/bin/nicl; \
      chmod +x /out/Cubelet/bin/nicl; \
    fi; \
    if [ -f /workspace/Cubelet/contrib/cubelet-code-deploy.sh ]; then \
      cp /workspace/Cubelet/contrib/cubelet-code-deploy.sh /out/Cubelet/bin/cubelet-code-deploy.sh; \
      chmod +x /out/Cubelet/bin/cubelet-code-deploy.sh; \
    fi

# ── Runtime stage ──────────────────────────────────────────────────────────────
FROM ubuntu:22.04

ARG DEBIAN_FRONTEND=noninteractive
ARG CUBE_VERSION=unknown

ENV IMAGE_ROOT=/opt/cube-image
ENV TOOLBOX_ROOT=/usr/local/services/cubetoolbox
ENV CUBE_COMPONENT=cubelet
ENV PATH="/usr/local/services/cubetoolbox/cube-shim/bin:${PATH}"

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
      ca-certificates \
      curl \
      iproute2 \
      iptables \
      iputils-ping \
      kmod \
      lsof \
      net-tools \
      procps \
      psmisc \
      e2fsprogs \
      tini \
      util-linux \
      xfsprogs \
    && rm -rf /var/lib/apt/lists/*

# Volume plugin host tools (cosfs / coscmd / jq).
COPY deploy/scripts/docker-install-volume-deps.sh /tmp/docker-install-volume-deps.sh
RUN chmod +x /tmp/docker-install-volume-deps.sh \
    && /tmp/docker-install-volume-deps.sh \
    && rm -f /tmp/docker-install-volume-deps.sh

COPY --from=builder /out/Cubelet ${IMAGE_ROOT}/Cubelet
COPY examples/volume/cos/binary/cube-volume-cos.sh ${IMAGE_ROOT}/Cubelet/plugin/cube-volume-cos
COPY examples/volume/cos/volume-cos.conf.example ${IMAGE_ROOT}/Cubelet/plugin/volume-cos.conf.example
COPY deploy/kubernetes/images/scripts/component-entrypoint.sh /usr/local/bin/component-entrypoint.sh

RUN printf '{"schema_version":1,"components":{"cubelet":{"version":"%s"},"cubecli":{"version":"%s"}}}\n' \
      "${CUBE_VERSION}" "${CUBE_VERSION}" > ${IMAGE_ROOT}/Cubelet/version.json \
    && chmod +x \
      ${IMAGE_ROOT}/Cubelet/bin/cubelet \
      ${IMAGE_ROOT}/Cubelet/bin/cubecli \
      ${IMAGE_ROOT}/Cubelet/plugin/cube-volume-cos \
      /usr/local/bin/component-entrypoint.sh \
    && mkdir -p \
      /data/cubelet \
      /data/log/Cubelet \
      /data/log/CubeShim \
      /data/log/CubeVmm \
      /data/cube-shim/disks \
      /data/snapshot_pack/disks \
      /tmp/cube \
      /run/cube-node \
    && "${IMAGE_ROOT}/Cubelet/bin/cubelet" --help >/dev/null \
    && "${IMAGE_ROOT}/Cubelet/bin/cubecli" --help >/dev/null

EXPOSE 9999 9998 9966
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/component-entrypoint.sh"]
