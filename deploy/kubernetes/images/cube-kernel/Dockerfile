# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# K8s cube-kernel installer image: assembles pre-built guest vmlinux artifacts.
#
# Build context must contain:
#   artifacts/vmlinux-bm          # required
#   artifacts/vmlinux-pvm         # required on amd64; optional on arm64 (no PVM guest)
#   scripts/component-entrypoint.sh
#
# Example:
#   docker build -f deploy/kubernetes/images/cube-kernel/Dockerfile \
#     -t cube-kernel:TAG /path/to/cube-kernel-ctx
#
# See .github/workflows/release-docker-images.yml and
# deploy/kubernetes/images/build-cube-images.sh.

FROM ubuntu:22.04

ARG DEBIAN_FRONTEND=noninteractive
ARG TARGETARCH
ENV IMAGE_ROOT=/opt/cube-image
ENV TOOLBOX_ROOT=/usr/local/services/cubetoolbox
ENV CUBE_COMPONENT=cube-kernel
ENV CUBE_ROLE=install

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
      ca-certificates \
      tini \
      util-linux \
    && rm -rf /var/lib/apt/lists/*

COPY artifacts/ /tmp/kernel-artifacts/
COPY scripts/component-entrypoint.sh /usr/local/bin/component-entrypoint.sh

ARG CUBE_VERSION=unknown
ARG CUBE_KERNEL_BM_VERSION=
ARG CUBE_KERNEL_PVM_VERSION=
RUN set -eux; \
    mkdir -p ${IMAGE_ROOT}/cube-kernel-scf; \
    test -f /tmp/kernel-artifacts/vmlinux-bm; \
    cp /tmp/kernel-artifacts/vmlinux-bm ${IMAGE_ROOT}/cube-kernel-scf/vmlinux-bm; \
    ln -sfn vmlinux-bm ${IMAGE_ROOT}/cube-kernel-scf/vmlinux; \
    if [ -f /tmp/kernel-artifacts/vmlinux-pvm ]; then \
      cp /tmp/kernel-artifacts/vmlinux-pvm ${IMAGE_ROOT}/cube-kernel-scf/vmlinux-pvm; \
    elif [ "${TARGETARCH}" = "arm64" ]; then \
      echo "skipping vmlinux-pvm on TARGETARCH=${TARGETARCH} (no PVM guest kernel)"; \
    else \
      echo "vmlinux-pvm is required for TARGETARCH=${TARGETARCH:-amd64} cube-kernel images" >&2; \
      exit 1; \
    fi; \
    rm -rf /tmp/kernel-artifacts; \
    BM="${CUBE_KERNEL_BM_VERSION:-${CUBE_VERSION}}"; \
    PVM="${CUBE_KERNEL_PVM_VERSION:-${CUBE_VERSION}}"; \
    printf '{"schema_version":1,"variants":{"bm":{"version":"%s","tag":"%s"},"pvm":{"version":"%s","tag":"%s"}}}\n' \
      "${BM}" "${BM}" "${PVM}" "${PVM}" > ${IMAGE_ROOT}/cube-kernel-scf/version.json; \
    chmod +x /usr/local/bin/component-entrypoint.sh; \
    test -f ${IMAGE_ROOT}/cube-kernel-scf/vmlinux-bm; \
    test -L ${IMAGE_ROOT}/cube-kernel-scf/vmlinux

ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/component-entrypoint.sh"]
