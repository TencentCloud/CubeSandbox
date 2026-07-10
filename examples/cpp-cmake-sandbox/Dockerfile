# syntax=docker/dockerfile:1.7
#
# A Cube-ready C/C++ development image. The inherited entrypoint keeps envd
# listening on :49983 while CMake/Ninja builds are launched through the SDK.
#
# Build:
#   docker build -t cubesandbox-cpp-cmake:local examples/cpp-cmake-sandbox

ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${CUBE_BASE_IMAGE}

ARG DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        build-essential \
        ccache \
        cmake \
        ninja-build \
    && rm -rf /var/lib/apt/lists/*

ENV CCACHE_DIR=/home/user/.cache/ccache \
    CCACHE_COMPILERCHECK=content

COPY sample /opt/cpp-cmake-sandbox/sample

RUN install -d -o user -g user /workspace "${CCACHE_DIR}" \
    && chown -R user:user /opt/cpp-cmake-sandbox

WORKDIR /workspace
EXPOSE 49983
