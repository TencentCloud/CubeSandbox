# syntax=docker/dockerfile:1.7
#
# CubeSandbox Rust runtime template.
#
# Build:
#   docker build --platform linux/amd64 -t cubesandbox-rust-runtime:local examples/rust-runtime-template
#
# Local smoke:
#   docker run --platform linux/amd64 --rm -d -p 49983:49983 --name cube-rust-runtime cubesandbox-rust-runtime:local
#   curl -o /dev/null -w "%{http_code}\n" http://127.0.0.1:49983/health  # => 204
#   docker exec cube-rust-runtime sh -lc 'rustc --version && cargo --version && cd /workspace/hello-rust && cargo test && cargo run --quiet'

ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
ARG CUBE_PLATFORM=linux/amd64
FROM --platform=${CUBE_PLATFORM} ${CUBE_BASE_IMAGE}

ARG RUST_TOOLCHAIN=1.89
ARG DEBIAN_FRONTEND=noninteractive

ENV RUSTUP_HOME=/usr/local/rustup \
    CARGO_HOME=/usr/local/cargo \
    PATH=/usr/local/cargo/bin:${PATH} \
    CARGO_NET_GIT_FETCH_WITH_CLI=true

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        build-essential \
        ca-certificates \
        curl \
        git \
        pkg-config \
    && rm -rf /var/lib/apt/lists/*

RUN curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs \
        | sh -s -- -y --profile minimal --default-toolchain "${RUST_TOOLCHAIN}" \
    && rustc --version \
    && cargo --version

COPY workspace/hello-rust /workspace/hello-rust

RUN cd /workspace/hello-rust \
    && cargo test \
    && cargo run --quiet \
    && rm -rf target \
    && chown -R user:user /workspace /home/user "${RUSTUP_HOME}" "${CARGO_HOME}"

WORKDIR /workspace

EXPOSE 49983
CMD []
