# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Multi-stage build for cube-api — E2B-compatible sandbox API server.
#
# Usage:
#   docker build -t cube-api:v1.0 .
#   docker run -e CUBE_MASTER_ADDR=http://cubemaster:8089 -p 3000:3000 cube-api:v1.0

# ── Build stage ────────────────────────────────────────────────────────────────
FROM rust:1.85-alpine AS builder

# musl-dev provides the musl C runtime/headers needed to link the static binary
# against the x86_64-unknown-linux-musl target (rust:alpine already ships gcc).
# We add the target explicitly so the installed musl headers are actually used.
# TLS is pure-rustls, so no OpenSSL is required.
RUN apk add --no-cache musl-dev \
    && rustup target add x86_64-unknown-linux-musl

WORKDIR /app

# Copy manifests + build script first so the (expensive) dependency-compilation
# layer is cached and reused unless the dependency set itself changes.
COPY Cargo.toml Cargo.lock rust-toolchain.toml ./
COPY build.rs ./

# Phase 1: compile dependencies against a dummy main so a one-line application
# source change doesn't invalidate the whole dependency cache.
RUN --mount=type=cache,target=/usr/local/cargo/registry \
    mkdir src && echo "fn main() {}" > src/main.rs \
    && cargo build --release --locked --target x86_64-unknown-linux-musl --bin cube-api 2>/dev/null || true \
    && rm -rf src

# Phase 2: copy the real source — only application code recompiles from here on.
COPY src/ src/
RUN --mount=type=cache,target=/usr/local/cargo/registry \
    cargo build --release --locked --target x86_64-unknown-linux-musl --bin cube-api

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

COPY --from=builder /app/target/x86_64-unknown-linux-musl/release/cube-api /usr/local/bin/cube-api

USER cube

EXPOSE 3000

# SIGTERM → graceful shutdown (axum::serve().with_graceful_shutdown)
STOPSIGNAL SIGTERM

HEALTHCHECK --interval=10s --timeout=3s --retries=3 \
    CMD curl -fsS http://127.0.0.1:3000/health

ENTRYPOINT ["cube-api"]
