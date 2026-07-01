# syntax=docker/dockerfile:1.7
#
# Gin API sandbox image for CubeSandbox.
#
# Purpose:
#   Provide a clean Go runtime environment for Agent-generated Gin APIs.
#   This image does NOT contain business code or preinstalled Gin dependencies.
#
# Runtime flow:
#   1. cubesandbox-base entrypoint starts envd on :49983.
#   2. SDK writes Agent-generated Go project files into /workspace/app.
#   3. SDK runs go mod tidy / go build.
#   4. SDK starts the generated Gin service on :8080.
#   5. SDK verifies the API endpoints with HTTP requests and reads /tmp/gin.log.
#
# Build:
#   docker build -t gin-api-sandbox:latest .

ARG CUBE_BASE_IMAGE=cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest
FROM ${CUBE_BASE_IMAGE}

ARG GO_VERSION=1.22.11
ARG GO_SHA256=0fc88d966d33896384fbde56e9a8d80a305dc17a9f48f1832e061724b1719991
ARG DEBIAN_FRONTEND=noninteractive

ENV GOPATH=/workspace/go
ENV GOCACHE=/workspace/.cache/go-build
ENV GOMODCACHE=/workspace/go/pkg/mod
ENV PATH=/usr/local/go/bin:${GOPATH}/bin:${PATH}
ENV GOPROXY=https://goproxy.cn,direct

# Minimal tools:
#   curl            -> download Go toolchain and run HTTP checks
#   ca-certificates -> HTTPS downloads
#   git          -> fallback for Go modules when GOPROXY falls back to direct
#   tar             -> extract Go toolchain archive
#   procps       -> pkill / ps for managing generated Gin process
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates=20230311+deb12u1 \
        curl=7.88.1-10+deb12u14 \
        git=1:2.39.5-0+deb12u3 \
        tar=1.34+dfsg-1.2+deb12u1 \
        procps=2:4.0.2-3 \
    && rm -rf /var/lib/apt/lists/*

# Install Go, configure shell environment, and prepare SDK workspace.
# This does not install project dependencies; it only configures the Go runtime.
RUN curl -fsSL --retry 3 --connect-timeout 20 --max-time 180 \
        -o /tmp/go.tgz \
        https://dl.google.com/go/go${GO_VERSION}.linux-amd64.tar.gz \
    && echo "${GO_SHA256}  /tmp/go.tgz" | sha256sum -c - \
    && rm -rf /usr/local/go \
    && tar -C /usr/local -xzf /tmp/go.tgz \
    && rm -f /tmp/go.tgz \
    && ln -sf /usr/local/go/bin/go /usr/local/bin/go \
    && ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt \
    && printf '%s\n' \
        'export GOROOT=/usr/local/go' \
        'export GOPATH=/workspace/go' \
        'export GOCACHE=/workspace/.cache/go-build' \
        'export GOMODCACHE=/workspace/go/pkg/mod' \
        'export PATH=/usr/local/go/bin:/workspace/go/bin:$PATH' \
        > /etc/profile.d/go.sh \
    && mkdir -p /workspace/app /workspace/go /workspace/.cache/go-build \
    && chown -R user:user /workspace \
    && touch /var/log/envd.log /var/log/jupyter.log \
    && chown user:user /var/log/envd.log /var/log/jupyter.log \
    && chmod 644 /var/log/envd.log /var/log/jupyter.log \
    && chmod 755 /workspace \
    && chmod -R 755 /workspace/app /workspace/go /workspace/.cache/go-build \
    && go env -w GOPROXY=https://goproxy.cn,direct GOSUMDB=sum.golang.google.cn

ENV HOME=/home/user
USER user
WORKDIR /workspace/app

# envd uses 49983.
# Port 49999 is reserved for the CubeSandbox template health probe.
# Agent-generated Gin services will usually listen on 8080.
EXPOSE 49983 49999 8080

# Do NOT override the cubesandbox-base entrypoint.
# The inherited cube-entrypoint.sh starts envd on ${ENVD_PORT:-49983},
# then execs this CMD as the foreground process.

