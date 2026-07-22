# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Multi-stage build for cube-master.
#
# Build context MUST be the repository root:
#   docker build -f CubeMaster/docker/Dockerfile -t cube-master:TAG .
#
# See .github/workflows/release-docker-images.yml.

# ── Build stage ────────────────────────────────────────────────────────────────
FROM golang:1.25-bookworm AS builder
ARG TARGETARCH
ARG CUBE_VERSION=0.0.0-dev
ARG CUBE_COMMIT=unknown
ARG CUBE_BUILD_TIME=unknown

WORKDIR /src

COPY CubeMaster/go.mod CubeMaster/go.sum ./
COPY cubelog/go.mod cubelog/go.sum /cubelog/
COPY CubeDB/go.mod CubeDB/go.sum /CubeDB/
COPY Cubelet/go.mod Cubelet/go.sum /Cubelet/
RUN go mod download

COPY CubeMaster/ ./
COPY cubelog/ /cubelog/
COPY CubeDB/ /CubeDB/
COPY Cubelet/ /Cubelet/

ENV CGO_ENABLED=0
RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) goarch=amd64 ;; \
      arm64) goarch=arm64 ;; \
      *) echo "unsupported TARGETARCH: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    GOOS=linux GOARCH="${goarch}" go build -o /out/cubemaster \
      -ldflags "-s -w \
        -X 'github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/version.Version=${CUBE_VERSION}' \
        -X 'github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/version.Commit=${CUBE_COMMIT}' \
        -X 'github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/version.BuildTime=${CUBE_BUILD_TIME}'" \
      ./cmd/cubemaster

# ── Runtime stage ──────────────────────────────────────────────────────────────
FROM ubuntu:24.04

RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        bash-completion \
        bpftrace \
        busybox \
        ca-certificates \
        curl \
        dnsutils \
        ethtool \
        file \
        gawk \
        iperf \
        iperf3 \
        iproute2 \
        iputils-ping \
        netcat-openbsd \
        netperf \
        net-tools \
        sysstat \
        tcpdump \
        telnet \
        tzdata \
        vim \
        wget \
        skopeo \
        umoci \
    && rm -rf /var/lib/apt/lists/* \
    && cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime \
    && echo "Asia/Shanghai" > /etc/timezone \
    && update-ca-certificates

# Volume plugin host tools (cosfs / coscmd / jq) for containerized CubeMaster.
# Build context is the repository root (see header comment).
COPY deploy/scripts/docker-install-volume-deps.sh /tmp/docker-install-volume-deps.sh
RUN chmod +x /tmp/docker-install-volume-deps.sh \
    && /tmp/docker-install-volume-deps.sh \
    && rm -f /tmp/docker-install-volume-deps.sh

# Layout matches one-click install prefix:
#   /usr/local/services/cubetoolbox/CubeMaster/{bin,plugin,conf.yaml}
RUN mkdir -p /usr/local/services/cubetoolbox/CubeMaster/bin \
    /usr/local/services/cubetoolbox/CubeMaster/plugin
COPY --from=builder --chmod=0755 /out/cubemaster \
    /usr/local/services/cubetoolbox/CubeMaster/bin/cubemaster

# COS volume Controller plugin (binary only; credentials via chart Secret mount).
COPY examples/volume/cos/binary/cube-volume-cos.sh \
    /usr/local/services/cubetoolbox/CubeMaster/plugin/cube-volume-cos
COPY examples/volume/cos/volume-cos.conf.example \
    /usr/local/services/cubetoolbox/CubeMaster/plugin/volume-cos.conf.example
RUN chmod +x /usr/local/services/cubetoolbox/CubeMaster/plugin/cube-volume-cos

RUN ["/usr/local/services/cubetoolbox/CubeMaster/bin/cubemaster", "--version"]

WORKDIR /usr/local/services/cubetoolbox/CubeMaster/

EXPOSE 8089

STOPSIGNAL SIGTERM

HEALTHCHECK --interval=10s --timeout=3s --retries=3 --start-period=60s \
    CMD curl -fsS http://127.0.0.1:8089/notify/health

ENTRYPOINT ["/usr/local/services/cubetoolbox/CubeMaster/bin/cubemaster"]
