# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Multi-stage build for the cube-s3lvol sidecar (s3lvol_tgt + rcow scripts).
# Build context must be the repository root; BuildKit is required so the
# adjacent Dockerfile.dockerignore applies. The builder stage defaults to
# cubesandbox-builder (ships /opt/s3lvol-spdk and the AWS CRT); override with
# --build-arg CUBE_BUILDER_IMAGE=... The runtime stays on Ubuntu 20.04 so the
# shared libs (libaio / libnuma / libuuid) match the linked glibc; OpenSSL
# is static.

ARG CUBE_BUILDER_IMAGE=ghcr.io/tencentcloud/cubesandbox-builder:ubuntu2004
FROM ${CUBE_BUILDER_IMAGE} AS builder

ARG CUBE_VERSION=0.0.0-dev

WORKDIR /workspace
COPY CubeS3lvol/ /workspace/CubeS3lvol/

RUN set -eux; \
    cd /workspace/CubeS3lvol; \
    AWS_BUILD_TYPE=RelWithDebInfo ./setup_dep.sh --jobs "$(nproc)"; \
    AWS_BUILD_TYPE=RelWithDebInfo make S3LVOL_BUILD_TYPE=release -j"$(nproc)"; \
    AWS_BUILD_TYPE=RelWithDebInfo ./make_release.sh --no-tar --skip-smoke \
      --version "${CUBE_VERSION}" --outdir /out/CubeS3lvol; \
    pkg="$(find /out/CubeS3lvol -mindepth 1 -maxdepth 1 -type d -name 's3lvol-*' | head -1)"; \
    test -n "${pkg}"; \
    test -x "${pkg}/bin/s3lvol_tgt"; \
    mv "${pkg}" /out/s3lvol

FROM ubuntu:20.04

ARG DEBIAN_FRONTEND=noninteractive
ARG CUBE_VERSION=0.0.0-dev

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
      ca-certificates \
      coreutils \
      hostname \
      kmod \
      libaio1 \
      libnuma1 \
      libuuid1 \
      nvme-cli \
      procps \
      python3 \
      tini \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /out/s3lvol /opt/s3lvol
COPY deploy/kubernetes/images/scripts/cube-s3lvol-entrypoint.sh \
     /usr/local/bin/cube-s3lvol-entrypoint.sh

RUN chmod +x /usr/local/bin/cube-s3lvol-entrypoint.sh \
      /opt/s3lvol/bin/s3lvol_tgt \
      /opt/s3lvol/scripts/*.sh \
      /opt/s3lvol/scripts/*.py \
    && test -x /opt/s3lvol/bin/s3lvol_tgt \
    && test -f /opt/s3lvol/scripts/rcow_start.sh \
    && test -f /opt/s3lvol/scripts/s3lvol_rpc.py \
    && test -f /opt/s3lvol/scripts/s3_bucket.py

ENV S3LVOL_ROOT=/opt/s3lvol \
    RCOW_REPO_ROOT=/opt/s3lvol \
    RCOW_RPC_SOCK=/var/run/s3lvol/s3lvol.sock \
    RCOW_S3_CFG=/etc/s3lvol/s3.cfg \
    RCOW_WAL_IMG=/data/cubelet/rcow/wal_bdev.img \
    RCOW_RUN_DIR=/var/run/s3lvol \
    RCOW_LOG_DIR=/data/log/rcow \
    CUBE_VERSION=${CUBE_VERSION}

ENTRYPOINT ["/usr/bin/tini", "-s", "--", "/usr/local/bin/cube-s3lvol-entrypoint.sh"]
