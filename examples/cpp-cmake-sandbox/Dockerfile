# syntax=docker/dockerfile:1.7
#
# C/C++ development template for CubeSandbox.
#
# Installs a complete C/C++ toolchain (gcc/g++/clang, CMake, Ninja, ccache,
# gdb) on top of the official CubeSandbox base image. The inherited
# cube-entrypoint keeps envd running on :49983 so Cube can use the standard
# readiness probe without any extra wiring.
#
# The compile cache (ccache) lives under /workspace/.ccache — a world-writable
# path inside the sandbox filesystem — so it is captured by snapshots and can
# be restored into a fresh sandbox for fast incremental builds.
#
# Build:
#   docker build -t cubesandbox-cpp-cmake:latest examples/cpp-cmake-sandbox
#
# Register as a Cube template:
#   cubemastercli tpl create-from-image \
#     --image <your-registry>/cubesandbox-cpp-cmake:latest \
#     --writable-layer-size 4G \
#     --expose-port 49983 \
#     --probe       49983 \
#     --probe-path  /health

ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${CUBE_BASE_IMAGE}

ARG DEBIAN_FRONTEND=noninteractive

# C/C++ toolchain: compilers + CMake + Ninja + ccache + debugger.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        build-essential \
        gcc \
        g++ \
        clang \
        cmake \
        ninja-build \
        ccache \
        gdb \
        make \
        pkg-config \
        git \
        ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Route every CMake compile through ccache. CCACHE_DIR points at a path in the
# sandbox filesystem (not root's home) so it survives snapshot/rollback and is
# writable regardless of which envd user (root or uid=1000 "user") runs builds.
ENV CCACHE_DIR=/workspace/.ccache \
    CMAKE_GENERATOR=Ninja \
    CMAKE_C_COMPILER_LAUNCHER=ccache \
    CMAKE_CXX_COMPILER_LAUNCHER=ccache

RUN mkdir -p /workspace/.ccache \
    && chmod -R 777 /workspace \
    && ccache --max-size=2G \
    && cmake --version \
    && ninja --version \
    && ccache --version

WORKDIR /workspace

EXPOSE 49983
