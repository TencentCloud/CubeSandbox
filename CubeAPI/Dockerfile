# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Multi-stage build for cube-api — E2B-compatible sandbox API server.
#
# Usage:
#   docker build -t cube-api:v1.0 .
#   docker run -e CUBE_MASTER_ADDR=http://cubemaster:8089 -p 3000:3000 cube-api:v1.0

# ── Build stage ────────────────────────────────────────────────────────────────
# TARGETARCH is auto-populated by BuildKit/buildx (amd64 | arm64). Falls back to
# the host arch for a plain `docker build` without buildx.
FROM rust:1.85-alpine AS builder
ARG TARGETARCH

# musl-dev provides the musl C runtime/headers needed to link the static binary
# against the <arch>-unknown-linux-musl target (rust:alpine already ships gcc).
# We add the target explicitly so the installed musl headers are actually used.
# TLS is pure-rustls, so no OpenSSL is required.
#
# The target triple is derived from TARGETARCH so the image builds natively on
# both amd64 and arm64. It is written to /etc/rust-target so the later build
# step in this stage reuses the same triple without re-deriving it.
RUN set -eux; \
    arch="${TARGETARCH:-$(apk --print-arch)}"; \
    case "${arch}" in \
      amd64|x86_64)  rust_target=x86_64-unknown-linux-musl;; \
      arm64|aarch64) rust_target=aarch64-unknown-linux-musl;; \
      *) echo "unsupported TARGETARCH: ${arch}" >&2; exit 1;; \
    esac; \
    echo "${rust_target}" > /etc/rust-target; \
    apk add --no-cache musl-dev; \
    rustup target add "${rust_target}"

WORKDIR /app

# Release metadata injected by CI (see release-docker-images.yml). build.rs
# embeds these into the binary via CUBE_VERSION_FULL.
ARG CUBE_VERSION=0.0.0-dev
ARG CUBE_COMMIT=unknown
ARG CUBE_BUILD_TIME=unknown
ENV CUBE_VERSION=$CUBE_VERSION \
    CUBE_COMMIT=$CUBE_COMMIT \
    CUBE_BUILD_TIME=$CUBE_BUILD_TIME

# Single-shot build: copy manifests + sources together, then cargo build once.
# Do NOT use a dummy-main dependency-cache phase — Cargo's mtime-based
# fingerprinting can skip relinking after COPY overwrites src/, leaving a
# ~400KB empty binary in the final image (see cargo#9312).
COPY Cargo.toml Cargo.lock rust-toolchain.toml build.rs ./
COPY src/ src/

# The built binary is staged at a fixed, arch-independent path so the runtime
# stage's COPY --from doesn't need to know the target triple.
# Size + --help gates catch a silently-broken binary before it is published.
RUN --mount=type=cache,target=/usr/local/cargo/registry \
    rust_target="$(cat /etc/rust-target)" \
    && cargo build --release --locked --target "${rust_target}" --bin cube-api \
    && install -Dm0755 "target/${rust_target}/release/cube-api" /out/cube-api \
    && test "$(stat -c%s /out/cube-api)" -gt 5000000 \
    && /out/cube-api --help 2>&1 | grep -qi 'cube-api'

# ── Runtime stage ──────────────────────────────────────────────────────────────
FROM alpine:3.21

# curl is required by the HEALTHCHECK below; tzdata for Asia/Shanghai local time.
RUN apk add --no-cache ca-certificates curl tzdata \
    && cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime \
    && echo "Asia/Shanghai" > /etc/timezone

# Run as an unprivileged user. The app's default log dir is <binary_dir>/log,
# which a non-root user cannot create under /usr/local/bin, so point LOG_DIR at
# a dedicated writable directory owned by the runtime user.
RUN addgroup -S cube && adduser -S -G cube cube \
    && mkdir -p /var/log/cube-api \
    && chown -R cube:cube /var/log/cube-api
ENV LOG_DIR=/var/log/cube-api

COPY --from=builder /out/cube-api /usr/local/bin/cube-api

USER cube

EXPOSE 3000

# SIGTERM → graceful shutdown (axum::serve().with_graceful_shutdown)
STOPSIGNAL SIGTERM

HEALTHCHECK --interval=10s --timeout=3s --retries=3 \
    CMD curl -fsS http://127.0.0.1:3000/health

ENTRYPOINT ["cube-api"]
