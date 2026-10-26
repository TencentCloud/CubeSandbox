# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# K8s cube-agent installer image: stages independent cube-agent.ext4 (virtio-pmem1).
#
# Build context must contain:
#   package/cube-agent/cube-agent.ext4
#   package/cube-agent/version
#   scripts/component-entrypoint.sh

FROM ubuntu:22.04

ARG DEBIAN_FRONTEND=noninteractive
ENV IMAGE_ROOT=/opt/cube-image
ENV TOOLBOX_ROOT=/usr/local/services/cubetoolbox
ENV CUBE_COMPONENT=cube-agent
ENV CUBE_ROLE=install

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
      ca-certificates \
      tini \
      util-linux \
    && rm -rf /var/lib/apt/lists/*

COPY package/cube-agent ${IMAGE_ROOT}/cube-agent
COPY scripts/component-entrypoint.sh /usr/local/bin/component-entrypoint.sh

ARG CUBE_VERSION=unknown
RUN set -eu; \
    agent_ver="$(tr -d '[:space:]' < ${IMAGE_ROOT}/cube-agent/version 2>/dev/null || true)"; \
    : "${agent_ver:=${CUBE_VERSION}}"; \
    test -f ${IMAGE_ROOT}/cube-agent/cube-agent.ext4; \
    test -f ${IMAGE_ROOT}/cube-agent/version; \
    { \
      printf '{\n  "schema_version": 1,\n  "components": {\n'; \
      printf '    "cube-agent": {"version": "%s"}\n' "${agent_ver}"; \
      printf '  }\n}\n'; \
    } > ${IMAGE_ROOT}/cube-agent/version.json \
    && chmod +x /usr/local/bin/component-entrypoint.sh

ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/component-entrypoint.sh"]
