# syntax=docker/dockerfile:1.7
#
# Rust-enabled CubeSandbox template.
#
# The image installs the Rust toolchain (rustc, cargo, rustup) on top
# of the official CubeSandbox base image. The inherited cube-entrypoint
# keeps envd running on :49983 so Cube can use the standard probe.
#
# A dummy Cargo project is pre-built during image creation to warm the
# crates.io index cache, reducing first-use latency in sandboxes.
#
# Build:
#   docker build -t rust-playground:latest examples/rust-playground
#
# Register as a Cube template:
#   cubemastercli tpl create-from-image \
#     --image <your-registry>/rust-playground:latest \
#     --writable-layer-size 4G \
#     --expose-port 49983 \
#     --probe       49983 \
#     --probe-path  /health

ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${CUBE_BASE_IMAGE}

ARG DEBIAN_FRONTEND=noninteractive
ARG RUST_TOOLCHAIN=stable

# Install only what Rust needs: gcc (C linker for rustc) + dev headers.
# build-essential is intentionally omitted (~20 MB for g++).
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        gcc \
        git \
        libc6-dev \
        libssl-dev \
        make \
        pkg-config \
    && rm -rf /var/lib/apt/lists/*

# Install Rust via rustup as the unprivileged "user" (uid=1000,
# created by the cubesandbox-base image) so that cargo works
# out-of-the-box without root warnings.
USER user
ENV HOME=/home/user \
    CARGO_HOME=/home/user/.cargo \
    RUSTUP_HOME=/home/user/.rustup

RUN curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs \
    | sh -s -- -y --no-modify-path --profile minimal --default-toolchain ${RUST_TOOLCHAIN} \
    && . "${CARGO_HOME}/env" \
    && rustc --version \
    && cargo --version

# Pre-build a dummy project to cache crates.io index and metadata.
# This avoids the ~15s index fetch on first cargo build in a sandbox.
RUN mkdir -p /home/user/workspace/cargo-cache-warmup/src \
    && cd /home/user/workspace/cargo-cache-warmup \
    && printf '[package]\nname = "cache-warmup"\nversion = "0.1.0"\nedition = "2021"\n' > Cargo.toml \
    && printf 'fn main() { println!("cache warmup"); }\n' > src/main.rs \
    && cargo build 2>/dev/null || true \
    && rm -rf /home/user/workspace/cargo-cache-warmup

WORKDIR /home/user/workspace

# Switch back to root for the entrypoint; cube-entrypoint.sh
# runs as root and drops to the "user" account via envd.
USER root

# Add cargo to PATH for all users so rustc/cargo are available
# regardless of which user the command runs as.
ENV CARGO_HOME=/home/user/.cargo
ENV PATH="${CARGO_HOME}/bin:${PATH}"

EXPOSE 49983
