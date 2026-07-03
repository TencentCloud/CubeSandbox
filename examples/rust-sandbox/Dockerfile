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

ARG DEBIAN_FRONTEND=noninteractive
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
# Install Rust toolchain via rustup (pinned stable for reproducibility).
# Installed as root; symlinked to /usr/local/bin so both root and the `user`
# account (uid=1000, used by envd for SDK commands) can invoke rustc/cargo.
# ---------------------------------------------------------------------------
RUN curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs \
    | sh -s -- -y --default-toolchain "${RUST_TOOLCHAIN}"

ENV PATH="/root/.cargo/bin:${PATH}"

# Symlink toolchain binaries so they are on every user's PATH
RUN for bin in rustc cargo rustup rustfmt clippy; do \
        ln -sf /root/.cargo/bin/"${bin}" /usr/local/bin/"${bin}"; \
    done

# Make the cargo registry+git cache readable by the `user` account (uid=1000)
# so that `sandbox.commands.run("cargo build")` works via envd without
# needing user="root".
RUN chmod -R a+rX /root/.cargo

# Verify the toolchain works for root
RUN rustc --version && cargo --version

# Verify the toolchain works for the `user` account (uid=1000)
USER user
RUN rustc --version && cargo --version
USER root

# ---------------------------------------------------------------------------
# Pre-build a demo axum project to warm the cargo registry.
# We `cargo clean` after the build so the image stays lean, but the crates
# cached under ~/.cargo/registry/ and ~/.cargo/git/ survive — which means
# the first `cargo build` inside a sandbox will only recompile, not download.
# ---------------------------------------------------------------------------
COPY demo_project /opt/rust-demo
RUN cd /opt/rust-demo \
    && cargo build --release \
    && cargo clean

EXPOSE 49983 8080

# No CMD — cube-entrypoint.sh keeps envd as the foreground process.
# All Rust compilation and execution happens through the E2B SDK:
#   sandbox.commands.run("cargo build --release")
#   sandbox.commands.run("./target/release/demo")
