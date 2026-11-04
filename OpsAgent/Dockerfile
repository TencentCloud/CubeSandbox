# syntax=docker/dockerfile:1

# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

# Build from repository root so sibling modules (pkgs/CubeLog) are in context:
#   docker build -f OpsAgent/Dockerfile -t cube-ops-agent:latest .

# ── Build stage ────────────────────────────────────────────────────────────
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /build/OpsAgent

# Copy go.mod/go.sum first for better layer caching
COPY OpsAgent/go.mod OpsAgent/go.sum ./
COPY pkgs/CubeLog /build/pkgs/CubeLog
RUN go mod download

COPY OpsAgent/ ./

ARG TARGETARCH
ARG CUBE_VERSION=0.0.0-dev
ARG CUBE_COMMIT=unknown
ARG CUBE_BUILD_TIME=unknown

RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build \
    -ldflags="-s -w \
      -X github.com/tencentcloud/CubeSandbox/OpsAgent/internal/version.Version=${CUBE_VERSION} \
      -X github.com/tencentcloud/CubeSandbox/OpsAgent/internal/version.Commit=${CUBE_COMMIT} \
      -X github.com/tencentcloud/CubeSandbox/OpsAgent/internal/version.BuildTime=${CUBE_BUILD_TIME}" \
    -o /out/ops-agent ./cmd/ops-agent

# ── Runtime stage ──────────────────────────────────────────────────────────
FROM ubuntu:24.04

RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        ca-certificates curl tzdata \
    && rm -rf /var/lib/apt/lists/* \
    && cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime \
    && echo "Asia/Shanghai" > /etc/timezone

COPY --from=builder --chmod=0755 /out/ops-agent /usr/local/bin/ops-agent

EXPOSE 8890

STOPSIGNAL SIGTERM

HEALTHCHECK --interval=30s --timeout=3s --retries=3 \
    CMD curl -fsS http://127.0.0.1:8890/health

ENTRYPOINT ["ops-agent"]
