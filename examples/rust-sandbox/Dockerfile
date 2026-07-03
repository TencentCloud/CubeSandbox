# syntax=docker/dockerfile:1.7
#
# CubeSandbox Rust template: ubuntu:22.04 + Rust toolchain + envd.
#
# Build:
#   docker build -t rust-sandbox:latest examples/rust-sandbox/
#
# Smoke test:
#   docker run --rm -d -p 49983:49983 --name test-rust rust-sandbox:latest
#   curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:49983/health  # => 204
#   docker exec test-rust rustc --version
#   docker exec -u user test-rust rustc --version
#   docker exec test-rust cargo --version
#   docker rm -f test-rust
#
# Register as a Cube template:
#   cubemastercli tpl create-from-image \
#     --image <registry>/rust-sandbox:latest \
#     --writable-layer-size 2G \
#     --expose-port 49983 \
#     --expose-port 8080 \
#     --probe 49983 \
#     --probe-path /health

ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${CUBE_BASE_IMAGE}

ENV DEBIAN_FRONTEND=noninteractive
ARG RUST_TOOLCHAIN=stable

# ---------------------------------------------------------------------------
# Install Rust build dependencies
# ---------------------------------------------------------------------------
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        build-essential \
        pkg-config \
        libssl-dev \
    && rm -rf /var/lib/apt/lists/*

# ---------------------------------------------------------------------------
# Install Rust toolchain via rustup as root.
# ---------------------------------------------------------------------------
# Share a single toolchain and cargo home across all users.  /root is mode
# 700 by default on Ubuntu, so we open the execute bit (711) so non-root
# users can traverse into /root/.rustup and /root/.cargo.
ENV RUSTUP_HOME=/root/.rustup \
    CARGO_HOME=/root/.cargo \
    PATH="/root/.cargo/bin:${PATH}"

RUN curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs \
        | sh -s -- -y --default-toolchain none \
    && chmod a+x /root \
    && rustup toolchain install "${RUST_TOOLCHAIN}" \
    && rustup default "${RUST_TOOLCHAIN}" \
    && rustc --version \
    && cargo --version \
    && for bin in rustc cargo rustup rustfmt clippy; do \
           ln -sf "/root/.cargo/bin/${bin}" "/usr/local/bin/${bin}"; \
       done

# ---------------------------------------------------------------------------
# Pre-warm the cargo registry.  After the pre-warm, open permissions so the
# `user` account (uid=1000, envd's default) can read and update the cache.
# ---------------------------------------------------------------------------
COPY demo_project /opt/rust-demo
RUN cd /opt/rust-demo \
    && cargo fetch \
    && cargo build --release \
    && cargo clean \
    && chmod -R a+rwX /root/.cargo
# Leave /opt/rust-demo on disk — rust_web_service.py rebuilds it at runtime.

# ---------------------------------------------------------------------------
# Verify the `user` account can find the toolchain and the registry cache.
# ---------------------------------------------------------------------------
USER user
RUN rustc --version && cargo --version \
    && du -sh "${CARGO_HOME}/registry/" 2>/dev/null

USER root

EXPOSE 49983 8080

# No CMD — cube-entrypoint.sh keeps envd as the foreground process.
# All Rust compilation and execution happens through the E2B SDK:
#   sandbox.commands.run("cargo build --release")
#   sandbox.commands.run("./target/release/binary")
