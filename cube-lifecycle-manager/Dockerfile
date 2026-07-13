# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Multi-stage build for cube-lifecycle-manager — the standalone service that
# owns sandbox auto-pause / auto-resume coordination between CubeMaster,
# CubeProxy and Redis. It supersedes the older in-container cube-proxy-sidecar
# with an unchanged wire protocol (admin push + /_sidecar_resume callback).
#
# Usage:
#   docker build -t cube-lifecycle-manager:one-click .
#   docker run --network=host \
#     -e CUBE_LCM_REDIS_ADDR=127.0.0.1:6379 \
#     -e CUBE_LCM_CUBEMASTER_URL=http://127.0.0.1:8089 \
#     cube-lifecycle-manager:one-click

# ── Build stage ────────────────────────────────────────────────────────────
FROM golang:1.25-alpine AS builder

ARG CUBE_VERSION=0.0.0-dev
ARG CUBE_COMMIT=unknown
ARG CUBE_BUILD_TIME=unknown

WORKDIR /src

# Cache dependency downloads: copy manifests first, then run go mod download,
# so a source-only change doesn't invalidate the module layer.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/     cmd/
COPY internal/ internal/

# Static, PIE-free binary so the distroless runtime (no libc) can exec it.
# GOARCH is left unset so the Go toolchain matches the build host / the
# --platform arg passed by `docker build --platform linux/{amd64,arm64}`;
# see the Makefile's per-arch build target.
RUN CGO_ENABLED=0 GOOS=linux \
    go build -trimpath -ldflags "-s -w" \
      -o /out/cube-lifecycle-manager ./cmd/cube-lifecycle-manager

# Bake the version metadata into a small JSON file the runtime image ships.
# Written in the builder stage because the distroless runtime lacks a shell.
RUN mkdir -p /out/etc/cube && \
    printf '{"version":"%s","commit":"%s","build_time":"%s"}\n' \
      "${CUBE_VERSION}" "${CUBE_COMMIT}" "${CUBE_BUILD_TIME}" \
      > /out/etc/cube/version.json

# ── Runtime stage ──────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/cube-lifecycle-manager /usr/local/bin/cube-lifecycle-manager
COPY --from=builder /out/etc/cube/version.json /etc/cube/version.json

# The HTTP listener (/internal/resume + /healthz + /readyz).
EXPOSE 8083

USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/cube-lifecycle-manager"]
