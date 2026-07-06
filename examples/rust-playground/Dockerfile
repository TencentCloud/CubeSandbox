# syntax=docker/dockerfile:1.7
#
# Rust playground inside a CubeSandbox template.
#
# The image installs the Rust toolchain (rustc, cargo, rustup) on top
# of the official CubeSandbox base image. The inherited cube-entrypoint
# keeps envd running on :49983 so Cube can use the standard probe.
#
# Build:
#   docker build -t rust-playground:latest examples/rust-playground
#
# Register as a Cube template:
#   cubemastercli tpl create-from-image \
#     --image <your-registry>/rust-playground:latest \
#     --writable-layer-size 2G \
#     --expose-port 49983 \
#     --probe       49983 \
#     --probe-path  /health

ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${CUBE_BASE_IMAGE}

ARG DEBIAN_FRONTEND=noninteractive
ARG RUST_TOOLCHAIN=stable

# build-essential would bring g++ (~20 MB), so we install only what Rust
# actually needs: gcc (the C linker that rustc requires) + development headers.
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

WORKDIR /home/user/workspace

# Switch back to root for the entrypoint; cube-entrypoint.sh
# runs as root and drops to the "user" account via envd.
USER root

# Add cargo to PATH for all users so rustc/cargo are available
# regardless of which user the command runs as.
ENV CARGO_HOME=/home/user/.cargo
ENV PATH="${CARGO_HOME}/bin:${PATH}"

EXPOSE 49983
