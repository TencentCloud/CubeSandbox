# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Multi-stage build for cube-master.
#
# Build context MUST be the repository root:
#   docker build -f CubeMaster/Dockerfile -t cube-master:TAG .
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
RUN go mod download

COPY CubeMaster/ ./

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
      ./cmd/cubemaster/...

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

COPY --from=builder --chmod=0755 /out/cubemaster /usr/local/services/cubemaster/cubemaster

RUN ["/usr/local/services/cubemaster/cubemaster", "--version"]

WORKDIR /usr/local/services/cubemaster/

EXPOSE 8089

STOPSIGNAL SIGTERM

HEALTHCHECK --interval=10s --timeout=3s --retries=3 --start-period=60s \
    CMD curl -fsS http://127.0.0.1:8089/notify/health

ENTRYPOINT ["/usr/local/services/cubemaster/cubemaster"]
