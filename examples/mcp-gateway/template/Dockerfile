# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# Template image for the E2B-compatible `mcp` sandbox option. It layers the
# example mcp-gateway on top of the CubeSandbox code-interpreter image and
# pre-installs a set of built-in MCP servers so they start without network
# downloads. Build from the examples/mcp-gateway directory:
#
#   docker buildx build -f template/Dockerfile -t <registry>/mcp-gateway:latest .
#
# Multi-arch builds work with `--platform linux/amd64,linux/arm64` as long as
# BASE_IMAGE is itself multi-arch.

ARG BASE_IMAGE=cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest
ARG GO_IMAGE=golang:1.25-alpine

FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build
ARG TARGETOS
ARG TARGETARCH
ARG GOPROXY
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags '-s -w' -o /out/mcp-gateway ./cmd/mcp-gateway

FROM ${BASE_IMAGE}
# Space-separated catalog servers to pre-install; see `mcp-gateway catalog`.
ARG MCP_SERVERS="duckduckgo fetch time arxiv"
COPY --from=build /out/mcp-gateway /usr/local/bin/mcp-gateway
RUN mcp-gateway pull ${MCP_SERVERS} && rm -rf /root/.cache/uv
