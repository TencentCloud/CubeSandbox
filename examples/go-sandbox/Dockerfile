# syntax=docker/dockerfile:1.7

# Go runtime template for Cube Sandbox.
# Adds the official Go toolchain onto cubesandbox-base; envd stays on :49983.
# No foreground service: Go programs run on demand via the E2B SDK.

# Build:
#   docker build -t cubesandbox-go:latest examples/go-sandbox
# Register (replace <your-registry>):
#   docker tag  cubesandbox-go:latest <your-registry>/cubesandbox-go:latest
#   docker push <your-registry>/cubesandbox-go:latest
#   cubemastercli tpl create-from-image \
#     --image <your-registry>/cubesandbox-go:latest \
#     --writable-layer-size 4G \
#     --expose-port 49983 --probe 49983 --probe-path /health
# ARM64 nodes: add --platform linux/arm64 to the build above.

ARG GOLANG_VERSION=1.23.4
ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16

FROM golang:${GOLANG_VERSION}-bookworm AS gobuild

FROM ${CUBE_BASE_IMAGE}

# cubesandbox-base (Ubuntu 22.04) already ships bash/curl/ca-certificates.
# git/procps omitted on purpose: examples use GOPROXY=proxy.golang.org
# (zip downloads, no VCS) and setsid/nohup (already present).

COPY --from=gobuild /usr/local/go /usr/local/go

# PATH: make `go` callable. GOPATH/GOCACHE under /workspace (writable layer,
# persist across runs). GOFLAGS=-mod=mod so examples skip `go mod tidy`.
ENV PATH=/usr/local/go/bin:$PATH \
    GOPATH=/workspace/go \
    GOCACHE=/workspace/.cache/go-build \
    GOFLAGS=-mod=mod \
    GO111MODULE=on \
    GOTOOLCHAIN=local

WORKDIR /workspace

# 49983 = envd readiness/probe port (required). No other port is opened.
EXPOSE 49983

# The base image's cube-entrypoint.sh launches envd on ${ENVD_PORT:-49983} in
# the background and then execs the foreground CMD, so envd stays up on :49983
# and the standard readiness probe (/health -> 204) passes. We rely on that
# inherited entrypoint; no extra ports or services are started at boot.
