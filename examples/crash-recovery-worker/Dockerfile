# syntax=docker/dockerfile:1.7

ARG RUST_IMAGE=rust:1.88-bookworm
ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16

FROM ${RUST_IMAGE} AS builder

WORKDIR /src

COPY Cargo.toml Cargo.lock ./
COPY src ./src

RUN cargo build --release --locked

FROM ${CUBE_BASE_IMAGE}

COPY --from=builder \
    /src/target/release/crash-recovery-worker \
    /usr/local/bin/crash-recovery-worker

RUN chmod 0755 /usr/local/bin/crash-recovery-worker \
    && mkdir -p /workspace/crash-recovery

WORKDIR /workspace/crash-recovery

EXPOSE 8080 49983
