# SPDX-License-Identifier: Apache-2.0
# Build with cube-envd as context; mount repository source read-only at test time.
ARG RUST_VERSION=1.89.0
FROM rust:${RUST_VERSION}-bookworm
RUN apt-get update && apt-get install -y --no-install-recommends \
    python3 curl socat sudo libcap2-bin iproute2 \
    && rm -rf /var/lib/apt/lists/*
COPY rust-toolchain.toml /workspace/cube-envd/rust-toolchain.toml
WORKDIR /workspace/cube-envd
RUN rustup show active-toolchain && rustup component add rustfmt clippy \
    && useradd --create-home --uid 1000 --shell /bin/bash user
