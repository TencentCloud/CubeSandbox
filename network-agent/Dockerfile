# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Multi-stage build for the K8s network-agent component image.
#
# Build context MUST be the repository root:
#   docker build -f network-agent/Dockerfile -t network-agent:TAG .
#
# Requires BuildKit (network-agent/Dockerfile.dockerignore). Builder stage
# defaults to cubesandbox-builder (Go + clang/bpf2go for CubeNet/cubevs gen).
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

COPY CubeNet/ /workspace/CubeNet/
COPY cubelog/ /workspace/cubelog/
COPY network-agent/ /workspace/network-agent/
# network-agent `make proto` syncs api into Cubelet client stub dir.
COPY Cubelet/pkg/networkagentclient/ /workspace/Cubelet/pkg/networkagentclient/

# Generate eBPF bindings (not committed) then regenerate protobuf stubs.
RUN set -eux; \
    (cd /workspace/CubeNet && make -C cubevs gen); \
    (cd /workspace/network-agent && make proto)

ENV CGO_ENABLED=0
RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) goarch=amd64 ;; \
      arm64) goarch=arm64 ;; \
      *) echo "unsupported TARGETARCH: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    mkdir -p /out/bin; \
    export CUBE_VERSION CUBE_COMMIT CUBE_BUILD_TIME; \
    cd /workspace/network-agent; \
    go mod download; \
    ldflags="-s -w \
      -X 'github.com/tencentcloud/CubeSandbox/network-agent/pkg/version.Version=${CUBE_VERSION}' \
      -X 'github.com/tencentcloud/CubeSandbox/network-agent/pkg/version.Commit=${CUBE_COMMIT}' \
      -X 'github.com/tencentcloud/CubeSandbox/network-agent/pkg/version.BuildTime=${CUBE_BUILD_TIME}'"; \
    GOOS=linux GOARCH="${goarch}" go build -ldflags "${ldflags}" \
      -o /out/bin/network-agent ./cmd/network-agent; \
    cd /workspace/CubeNet/cubevs; \
    go mod download; \
    GOOS=linux GOARCH="${goarch}" go build \
      -o /out/bin/cubevsmapdump ./cmd/cubevsmapdump

# Assemble one-click-compatible network-agent tree for the runtime stage.
COPY configs/single-node/network-agent.yaml /tmp/network-agent.yaml
RUN set -eux; \
    mkdir -p /out/network-agent/bin /out/network-agent/state; \
    cp /out/bin/network-agent /out/bin/cubevsmapdump /out/network-agent/bin/; \
    cp /tmp/network-agent.yaml /out/network-agent/network-agent.yaml

# ── Runtime stage ──────────────────────────────────────────────────────────────
FROM ubuntu:22.04

ARG DEBIAN_FRONTEND=noninteractive
ARG CUBE_VERSION=unknown

ENV IMAGE_ROOT=/opt/cube-image
ENV TOOLBOX_ROOT=/usr/local/services/cubetoolbox
ENV CUBE_COMPONENT=network-agent
ENV PATH="/usr/local/services/cubetoolbox/cube-shim/bin:${PATH}"

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
      ca-certificates \
      curl \
      iproute2 \
      iptables \
      iputils-ping \
      kmod \
      net-tools \
      procps \
      tini \
      util-linux \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /out/network-agent ${IMAGE_ROOT}/network-agent
COPY deploy/kubernetes/images/scripts/component-entrypoint.sh /usr/local/bin/component-entrypoint.sh

RUN printf '{"schema_version":1,"components":{"network-agent":{"version":"%s"}}}\n' \
      "${CUBE_VERSION}" > ${IMAGE_ROOT}/network-agent/version.json \
    && chmod +x \
      ${IMAGE_ROOT}/network-agent/bin/network-agent \
      ${IMAGE_ROOT}/network-agent/bin/cubevsmapdump \
      /usr/local/bin/component-entrypoint.sh \
    && ln -sfn ${IMAGE_ROOT}/network-agent/bin/cubevsmapdump /usr/local/bin/cubevsmapdump \
    && mkdir -p /data/cubelet /data/log /tmp/cube /run/cube-node \
    && "${IMAGE_ROOT}/network-agent/bin/network-agent" --help >/dev/null

EXPOSE 19090
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/component-entrypoint.sh"]
