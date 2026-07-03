# syntax=docker/dockerfile:1.7
#
# CubeSandbox Go runtime template.
#
# Build:
#   docker build --platform linux/amd64 -t cubesandbox-go-runtime:local examples/go-runtime-template
#
# Local smoke:
#   docker run --platform linux/amd64 --rm -d -p 49983:49983 --name cube-go-runtime cubesandbox-go-runtime:local
#   curl -o /dev/null -w "%{http_code}\n" http://127.0.0.1:49983/health  # => 204
#   docker exec cube-go-runtime sh -lc 'go version && cd /workspace/hello-go && go test ./... && go run .'

ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
ARG CUBE_PLATFORM=linux/amd64
FROM --platform=${CUBE_PLATFORM} ${CUBE_BASE_IMAGE}

ARG GO_VERSION=1.25.4
ARG DEBIAN_FRONTEND=noninteractive

ENV GOPATH=/home/user/go \
    GOMODCACHE=/home/user/go/pkg/mod \
    PATH=/usr/local/go/bin:/home/user/go/bin:${PATH}

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        build-essential \
        ca-certificates \
        curl \
        git \
    && rm -rf /var/lib/apt/lists/*

RUN set -eux; \
    arch="$(dpkg --print-architecture)"; \
    case "${arch}" in \
        amd64) go_arch=amd64 ;; \
        arm64) go_arch=arm64 ;; \
        *) echo "unsupported architecture: ${arch}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${go_arch}.tar.gz" -o /tmp/go.tgz; \
    rm -rf /usr/local/go; \
    tar -C /usr/local -xzf /tmp/go.tgz; \
    rm -f /tmp/go.tgz; \
    go version

COPY workspace/hello-go /workspace/hello-go

RUN cd /workspace/hello-go \
    && go test ./... \
    && go run . \
    && chown -R user:user /workspace /home/user

WORKDIR /workspace

EXPOSE 49983
CMD []
