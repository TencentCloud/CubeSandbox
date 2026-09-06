# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Multi-stage build for cube-templatecenter.
#
# Build context MUST be the repository root:
#   docker build -f CubeTemplateCenter/docker/Dockerfile -t cube-templatecenter:TAG .
#
# See .github/workflows/release-docker-images.yml.

# ── Build stage ────────────────────────────────────────────────────────────────
FROM golang:1.25-bookworm AS builder
ARG TARGETARCH
ARG CUBE_VERSION=0.0.0-dev
ARG CUBE_COMMIT=unknown
ARG CUBE_BUILD_TIME=unknown

WORKDIR /src

# CubeTemplateCenter/go.mod replaces CubeMaster, CubeDB, Cubelet and cubelog with
# local paths, so all five module roots have to be in the build context.
COPY CubeTemplateCenter/go.mod CubeTemplateCenter/go.sum ./
COPY CubeMaster/go.mod CubeMaster/go.sum /CubeMaster/
COPY cubelog/go.mod cubelog/go.sum /cubelog/
COPY CubeDB/go.mod CubeDB/go.sum /CubeDB/
COPY Cubelet/go.mod Cubelet/go.sum /Cubelet/
RUN go mod download

COPY CubeTemplateCenter/ ./
COPY CubeMaster/ /CubeMaster/
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
    GOOS=linux GOARCH="${goarch}" go build -o /out/templatecenter \
      -ldflags "-s -w \
        -X 'github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/version.Version=${CUBE_VERSION}' \
        -X 'github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/version.Commit=${CUBE_COMMIT}' \
        -X 'github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/version.BuildTime=${CUBE_BUILD_TIME}'" \
      ./cmd/templatecenter

# ── Runtime stage ──────────────────────────────────────────────────────────────
FROM ubuntu:24.04

# skopeo + umoci are MANDATORY here, not diagnostic conveniences: this process is
# the one that actually pulls the source image and unpacks its layers before
# mkfs. e2fsprogs provides mkfs.ext4, and mount/losetup (util-linux) back the
# loop-device path. A missing tool surfaces late, as a failed build reported over
# the status callback, so they are pinned in the image rather than assumed.
RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        e2fsprogs \
        file \
        gawk \
        iproute2 \
        iputils-ping \
        netcat-openbsd \
        skopeo \
        tzdata \
        umoci \
        util-linux \
        xz-utils \
    && rm -rf /var/lib/apt/lists/* \
    && cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime \
    && echo "Asia/Shanghai" > /etc/timezone \
    && update-ca-certificates

# Layout matches the one-click install prefix:
#   /usr/local/services/cubetoolbox/CubeTemplateCenter/{bin,conf.yaml}
RUN mkdir -p /usr/local/services/cubetoolbox/CubeTemplateCenter/bin
COPY --from=builder --chmod=0755 /out/templatecenter \
    /usr/local/services/cubetoolbox/CubeTemplateCenter/bin/templatecenter

RUN ["/usr/local/services/cubetoolbox/CubeTemplateCenter/bin/templatecenter", "--version"]

WORKDIR /usr/local/services/cubetoolbox/CubeTemplateCenter/

# Config is read from an environment variable, NOT from a -conf flag (the process
# reuses CubeMaster's config loader). Deployments mount conf.yaml here.
#
# Every variable this process reads is spelled CUBE_TEMPLATE_CENTER_*; the
# pre-rename CUBE_TC_* / CUBE_MASTER_* / CUBEMASTER_* spellings still work as a
# fallback and log a deprecation notice. See CubeTemplateCenter/pkg/tcconfig.
ENV CUBE_TEMPLATE_CENTER_CONFIG_PATH=/usr/local/services/cubetoolbox/CubeTemplateCenter/conf.yaml

EXPOSE 8090

STOPSIGNAL SIGTERM

# start-period is generous because the first boot runs schema migration behind a
# DB lock, which waits for CubeMaster when both start together.
HEALTHCHECK --interval=10s --timeout=3s --retries=3 --start-period=60s \
    CMD curl -fsS http://127.0.0.1:8090/health

ENTRYPOINT ["/usr/local/services/cubetoolbox/CubeTemplateCenter/bin/templatecenter"]
